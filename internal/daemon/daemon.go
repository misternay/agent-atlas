// Package daemon wires atlasd: storage, ingestion socket, writer pipeline,
// WebSocket hub, REST API, retention and stale sweeps.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"agentatlas/internal/api"
	"agentatlas/internal/config"
	"agentatlas/internal/correlate"
	"agentatlas/internal/ingest"
	"agentatlas/internal/storage/sqlite"
	"agentatlas/internal/telemetry"
)

// Options configure a daemon run.
type Options struct {
	Home   string
	Addr   string // overrides config when non-empty
	UI     fs.FS
	Logger *slog.Logger
	// Ready, if set, receives the bound HTTP address once serving.
	Ready chan<- string
}

// Run starts atlasd and blocks until ctx is cancelled.
func Run(ctx context.Context, o Options) error {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	if err := config.EnsureHome(o.Home); err != nil {
		return err
	}
	lock, err := config.TryDaemonLock(o.Home)
	if err != nil {
		return fmt.Errorf("acquire daemon lock: %w", err)
	}
	defer lock.Close()
	paths := config.PathsFor(o.Home)
	cfg, err := config.Load(paths)
	if err != nil {
		log.Warn("config unreadable; using defaults", "err", err)
	}
	if o.Addr != "" {
		cfg.Addr = o.Addr
	}
	if err := requireLoopback(cfg.Addr); err != nil {
		return err
	}
	control, err := ingest.ListenUnix(paths.Control)
	if err != nil {
		return err
	}
	defer control.Close()
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Addr, err)
	}
	defer ln.Close()
	host, _, _ := net.SplitHostPort(cfg.Addr)
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	addr := net.JoinHostPort(host, port)
	token, err := api.LoadOrCreateToken(paths.Token)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	store, err := sqlite.Open(paths.DB)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer store.Close()

	hub := api.NewHub()
	pipe := ingest.NewPipeline(store, cfg.HostID, func(d []correlate.Delta) { hub.Publish(d) }, 4096, log)
	if v, _ := store.GetMeta(ctx, "paused"); v == "true" {
		_ = api.SetPaused(paths, pipe, true)
	} else {
		_ = api.SetPaused(paths, pipe, false)
	}

	sock, err := ingest.Listen(paths.Socket, pipe)
	if err != nil {
		return err
	}
	defer sock.Close()
	srv, err := api.New(store, pipe, hub, token, addr, paths, cfg, o.UI)
	if err != nil {
		return err
	}
	ret := &retention{store: store, pipe: pipe, cfg: cfg, log: log}
	ret.onDeleted = func(ctx context.Context) { srv.HistoryDeleted(ctx, "retention_pruned") }
	srv.RetentionNotes = ret.notes

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	handler := srv.Handler()
	baseContext := func(net.Listener) context.Context { return runCtx }
	httpSrv := &http.Server{Handler: handler, BaseContext: baseContext, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 120 * time.Second}
	controlMux := http.NewServeMux()
	controlMux.HandleFunc("GET /control/address", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"url": "http://" + addr})
	})
	controlMux.HandleFunc("POST /control/stop", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		cancel()
	})
	controlMux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		r.Host = addr
		handler.ServeHTTP(w, r)
	})
	controlSrv := &http.Server{Handler: controlMux, BaseContext: baseContext, ReadHeaderTimeout: 5 * time.Second}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); pipe.Run(runCtx) }()
	wg.Add(1)
	go func() { defer wg.Done(); sock.Serve(runCtx) }()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := controlSrv.Serve(control); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("control server", "err", err)
			cancel()
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			cancel()
		}
	}()
	// Recover anything the shim spooled while we were down.
	if n, err := pipe.DrainSpool(runCtx, paths.Spool); err != nil {
		log.Warn("spool drain", "err", err)
	} else if n > 0 {
		log.Info("ingested spooled envelopes", "count", n)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		maintenance(runCtx, pipe, ret, paths, cfg, log)
	}()
	if exportEnabled(cfg) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			exportLoop(runCtx, store, &telemetry.Exporter{Endpoint: cfg.ExportEndpoint}, log)
		}()
		log.Info("OTLP export enabled (explicit opt-in)", "endpoint", cfg.ExportEndpoint)
	} else if cfg.ExportEnabled {
		log.Warn("export_enabled is set but export_preview_ack/export_endpoint missing; export stays off")
	}
	log.Info("atlasd started", "addr", ln.Addr().String(), "socket", paths.Socket, "home", o.Home)
	if o.Ready != nil {
		o.Ready <- ln.Addr().String()
	}
	<-runCtx.Done()
	shutdownCtx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	_ = controlSrv.Shutdown(shutdownCtx)
	_ = httpSrv.Shutdown(shutdownCtx)
	cancel()
	wg.Wait()
	log.Info("atlasd stopped")
	return nil
}

func requireLoopback(addr string) error {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(h)
	if h == "localhost" || (ip != nil && ip.IsLoopback()) {
		return nil
	}
	return fmt.Errorf("refusing to bind non-loopback address %q (UI/API is local-only)", addr)
}

// exportEnabled reports whether OTLP export is explicitly opted into.
func exportEnabled(cfg config.Config) bool {
	return cfg.ExportEnabled && cfg.ExportPreviewAck && cfg.ExportEndpoint != ""
}

// exportLoop sends each ended session's sanitized trace once.
func exportLoop(ctx context.Context, store *sqlite.Store, exp *telemetry.Exporter, log *slog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if _, err := exportOnce(ctx, store, exp); err != nil {
			log.Warn("otlp export failed; will retry", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// exportOnce exports ended sessions in commit order of their session.ended
// events, after the persisted sequence cursor, draining any backlog in pages.
func exportOnce(ctx context.Context, store *sqlite.Store, exp *telemetry.Exporter) (int, error) {
	n := 0
	for {
		cur, _ := store.GetMeta(ctx, "export_cursor_seq")
		after, _ := strconv.ParseInt(cur, 10, 64)
		list, err := store.EndedSessionsAfterSeq(ctx, after, 100)
		if err != nil {
			return n, err
		}
		if len(list) == 0 {
			return n, nil
		}
		for _, es := range list {
			s := es.Session
			spans, links, err := store.TraceSpans(ctx, s.TraceID)
			if err != nil {
				return n, err
			}
			if err := exp.Export(ctx, telemetry.ToOTLP(&s, spans, links)); err != nil {
				return n, err
			}
			_ = store.SetMeta(ctx, "export_cursor_seq", strconv.FormatInt(es.Seq, 10))
			n++
		}
	}
}

func maintenance(ctx context.Context, pipe *ingest.Pipeline, ret *retention, paths config.Paths, cfg config.Config, log *slog.Logger) {
	stale := time.NewTicker(30 * time.Second)
	spoolT := time.NewTicker(15 * time.Second)
	retT := time.NewTicker(10 * time.Minute)
	defer stale.Stop()
	defer spoolT.Stop()
	defer retT.Stop()
	ret.run(ctx) // once at startup
	threshold := time.Duration(cfg.StaleAfterSecs) * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-stale.C:
			if n, err := pipe.SweepStale(ctx, threshold); err != nil {
				log.Warn("stale sweep", "err", err)
			} else if n > 0 {
				log.Info("marked stale", "spans", n)
			}
		case <-spoolT.C:
			if _, err := pipe.DrainSpool(ctx, paths.Spool); err != nil {
				log.Warn("spool drain", "err", err)
			}
		case <-retT.C:
			ret.run(ctx)
		}
	}
}
