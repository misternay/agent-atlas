// Package api serves the read-only REST API, the resumable WebSocket stream and
// the embedded dashboard on the loopback interface (design §9.3, §10.2).
package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agentatlas/internal/config"
	"agentatlas/internal/correlate"
	"agentatlas/internal/graph"
	"agentatlas/internal/ingest"
	"agentatlas/internal/normalize"
	"agentatlas/internal/shim"
	"agentatlas/internal/storage/sqlite"
	"agentatlas/internal/telemetry"
)

// Version is the Atlas version string.
var Version = "0.1.0"

// Server holds API dependencies.
type Server struct {
	store   *sqlite.Store
	pipe    *ingest.Pipeline
	hub     *Hub
	guard   *guard
	paths   config.Paths
	cfg     config.Config
	ui      fs.FS
	started time.Time
	// RetentionNotes is set by the daemon after pruning (shown in storage view).
	RetentionNotes func() map[string]any
}

// New creates the API server. addr must be a loopback host:port.
func New(store *sqlite.Store, pipe *ingest.Pipeline, hub *Hub, token string, addr string, paths config.Paths, cfg config.Config, ui fs.FS) (*Server, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	return &Server{store: store, pipe: pipe, hub: hub, guard: &guard{token: token, port: port}, paths: paths, cfg: cfg, ui: ui, started: time.Now()}, nil
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.Handler { return s.guard.wrap(true, h) }
	mux.Handle("GET /api/v1/health", s.guard.wrap(false, http.HandlerFunc(s.handleHealth)))
	mux.Handle("GET /api/v1/sessions", auth(s.handleSessions))
	mux.Handle("GET /api/v1/sessions/{id}", auth(s.handleSession))
	mux.Handle("GET /api/v1/sessions/{id}/events", auth(s.handleSessionEvents))
	mux.Handle("GET /api/v1/sessions/{id}/replay", auth(s.handleReplay))
	mux.Handle("GET /api/v1/traces/{traceId}", auth(s.handleTrace))
	mux.Handle("GET /api/v1/search", auth(s.handleSearch))
	mux.Handle("GET /api/v1/graph", auth(s.handleGraph))
	mux.Handle("GET /api/v1/snapshot", auth(s.handleSnapshot))
	mux.Handle("GET /api/v1/capabilities", auth(s.handleCapabilities))
	mux.Handle("GET /api/v1/settings", auth(s.handleSettings))
	mux.Handle("GET /api/v1/stream", auth(s.handleStream))
	mux.Handle("POST /api/v1/admin/pause", auth(s.handlePause))
	mux.Handle("POST /api/v1/admin/clear-history", auth(s.handleClear))
	mux.Handle("GET /api/v1/export/preview", auth(s.handleExportPreview))
	mux.Handle("POST /api/v1/admin/signin-code", auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"code": s.guard.issueCode()})
	}))
	mux.Handle("GET /auth", s.guard.wrap(false, http.HandlerFunc(s.guard.authRedirect)))
	mux.Handle("POST /api/v1/auth/exchange", s.guard.wrap(false, http.HandlerFunc(s.guard.authExchange)))
	mux.Handle("/api/", s.guard.wrap(true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})))
	mux.Handle("/", s.guard.wrap(false, s.uiHandler()))
	return mux
}

func (s *Server) uiHandler() http.Handler {
	if s.ui == nil {
		return http.NotFoundHandler()
	}
	files := http.FileServer(http.FS(s.ui))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" {
			if _, err := fs.Stat(s.ui, p); err == nil {
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		// SPA fallback
		b, err := fs.ReadFile(s.ui, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

func qInt(r *http.Request, k string, def int64) int64 {
	v := r.URL.Query().Get(k)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// qTime parses RFC3339 or epoch-microsecond values.
func qTime(r *http.Request, k string) int64 {
	v := r.URL.Query().Get(k)
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UnixMicro()
	}
	return 0
}

func serverError(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	_ = err
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !s.guard.authenticated(r) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "atlasd"})
		return
	}
	st, err := s.store.Stats(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "service": "atlasd", "version": Version, "uptime_s": int(time.Since(s.started).Seconds()),
		"paused": s.pipe.Paused(), "ingest": s.pipe.Stats.Snapshot(), "storage": st,
		"subscribers": s.hub.Subscribers(), "export_enabled": s.cfg.ExportEnabled,
		"spool_bytes": dirSize(s.paths.Spool),
	})
}

func dirSize(dir string) int64 {
	var n int64
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if fi, err := e.Info(); err == nil {
			n += fi.Size()
		}
	}
	return n
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := sqlite.SessionFilter{Status: q.Get("status"), Harness: q.Get("harness"), Workspace: q.Get("workspace"), SessionID: q.Get("q"),
		From: qTime(r, "from"), To: qTime(r, "to"), Limit: int(qInt(r, "limit", 100)), Offset: int(qInt(r, "offset", 0))}
	if f.Status == "idle" || f.Status == "stale" {
		f.Status = "active" // idle/stale are derived from last activity client-side
	}
	list, total, err := s.store.ListSessions(r.Context(), f)
	if err != nil {
		serverError(w, err)
		return
	}
	if list == nil {
		list = []correlate.Session{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": list, "total": total, "limit": f.Limit, "offset": f.Offset})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.store.GetSession(r.Context(), r.PathValue("id"))
	if err != nil {
		serverError(w, err)
		return
	}
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	after := qInt(r, "after_seq", 0)
	minSeq, maxSeq, _ := s.store.SeqBounds(r.Context())
	evs, err := s.store.SessionEvents(r.Context(), id, after, int(qInt(r, "limit", 500)))
	if err != nil {
		serverError(w, err)
		return
	}
	if evs == nil {
		evs = []sqlite.StoredEvent{}
	}
	next := after
	if len(evs) > 0 {
		next = evs[len(evs)-1].Seq
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs, "next_after_seq": next, "min_seq": minSeq, "max_seq": maxSeq})
}

// handleReplay re-projects a session's stored events in sequence order with a
// fresh projector. Nothing is executed; it only reconstructs the view.
func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		serverError(w, err)
		return
	}
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	evs, truncated, err := s.store.ReplayEvents(r.Context(), id)
	if err != nil {
		serverError(w, err)
		return
	}
	p := correlate.New(nil)
	frames := make([]correlate.Delta, 0, len(evs))
	for _, se := range evs {
		frames = append(frames, p.Apply(se.Seq, se.Event))
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sess, "frames": frames, "executes_tools": false, "truncated": truncated})
}

func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	tid := r.PathValue("traceId")
	sess, err := s.store.GetSessionByTrace(r.Context(), tid)
	if err != nil {
		serverError(w, err)
		return
	}
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "trace not found"})
		return
	}
	spans, links, err := s.store.TraceSpans(r.Context(), tid)
	if err != nil {
		serverError(w, err)
		return
	}
	if r.URL.Query().Get("format") == "otlp" {
		writeJSON(w, http.StatusOK, telemetry.ToOTLP(sess, spans, links))
		return
	}
	if spans == nil {
		spans = []correlate.Span{}
	}
	if links == nil {
		links = []correlate.Link{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sess, "spans": spans, "links": links})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := sqlite.SpanFilter{Tool: q.Get("tool"), Category: q.Get("category"), Status: q.Get("status"), Harness: q.Get("harness"),
		Workspace: q.Get("workspace"), SessionID: q.Get("session_id"), Resource: q.Get("resource"), Evidence: q.Get("evidence"),
		Kind: q.Get("kind"), From: qTime(r, "from"), To: qTime(r, "to"), Limit: int(qInt(r, "limit", 100)), Offset: int(qInt(r, "offset", 0))}
	res, total, err := s.store.SearchSpans(r.Context(), f)
	if err != nil {
		serverError(w, err)
		return
	}
	if res == nil {
		res = []sqlite.SpanResult{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"spans": res, "total": total, "limit": f.Limit, "offset": f.Offset})
}

func (s *Server) snapshot(r *http.Request) (*sqlite.Snapshot, error) {
	window := time.Duration(qInt(r, "window_s", int64(config.DefaultLiveWindow/time.Second))) * time.Second
	if window <= 0 || window > 24*time.Hour {
		window = config.DefaultLiveWindow
	}
	sessWindow := time.Duration(qInt(r, "sessions_since_s", 86400)) * time.Second
	now := time.Now()
	return s.store.LiveSnapshot(r.Context(), now.Add(-window).UnixMicro(), now.Add(-sessWindow).UnixMicro(), r.URL.Query().Get("session_id"), int(qInt(r, "max_spans", 2000)))
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshot(r)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshot(r)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, graph.Build(snap, int(qInt(r, "max_tools", 40))))
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	observed, err := s.store.LastEventByHarness(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	var out []map[string]any
	for _, a := range normalize.Adapters() {
		caps := a.Capabilities()
		obs := observed[a.Name()]
		var configuredNotObserved []string
		for _, h := range caps.HookEvents {
			if _, ok := obs[h]; !ok {
				configuredNotObserved = append(configuredNotObserved, h)
			}
		}
		out = append(out, map[string]any{"harness": a.Name(), "capabilities": caps, "observed_hooks": obs, "not_yet_observed": configuredNotObserved})
	}
	writeJSON(w, http.StatusOK, map[string]any{"harnesses": out})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"paused": s.pipe.Paused(), "addr": s.cfg.Addr, "retention_days": s.cfg.RetentionDays, "max_db_bytes": s.cfg.MaxDBBytes,
		"spool_max_bytes": s.cfg.SpoolMaxBytes, "stale_after_secs": s.cfg.StaleAfterSecs, "export_enabled": s.cfg.ExportEnabled,
		"raw_prompts": false, "raw_tool_output": false, "home": s.paths.Home,
		"stored_fields": []string{"event type", "native hook name", "timestamps", "session/turn/tool-call ids", "tool name & category",
			"bounded, secret-redacted target label (e.g. program name, workspace-relative path, MCP server, URL host)",
			"status, evidence level and outcome basis", "duration", "home-collapsed workspace path", "model name, permission mode"},
		"never_stored": []string{"prompts", "tool output / responses", "file contents", "full command lines and arguments",
			"environment variables", "transcript paths", "assistant messages", "auth headers / tokens"},
		"retention": func() map[string]any {
			if s.RetentionNotes != nil {
				return s.RetentionNotes()
			}
			return nil
		}(),
	})
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paused bool `json:"paused"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	if err := SetPaused(s.paths, s.pipe, body.Paused); err != nil {
		serverError(w, err)
		return
	}
	_ = s.store.SetMeta(r.Context(), "paused", strconv.FormatBool(body.Paused))
	p := body.Paused
	s.hub.Broadcast(Message{Type: "status", Paused: &p})
	writeJSON(w, http.StatusOK, map[string]any{"paused": body.Paused})
}

// SetPaused toggles collection and the shim-visible pause marker file.
func SetPaused(paths config.Paths, pipe *ingest.Pipeline, paused bool) error {
	pipe.SetPaused(paused)
	marker := filepath.Join(paths.Home, shim.PausedFile)
	if paused {
		return os.WriteFile(marker, nil, 0o600)
	}
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil || body.Confirm != "delete all history" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `send {"confirm":"delete all history"}`})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	// Runs on the writer goroutine; the projector cache is reset before any
	// further commit so deleted rows cannot be resurrected.
	if err := s.pipe.Exclusive(ctx, func() error { return s.store.ClearHistory(ctx) }); err != nil {
		serverError(w, err)
		return
	}
	s.hub.Broadcast(Message{Type: "resync_required", Reason: "history_cleared"})
	writeJSON(w, http.StatusOK, map[string]any{"cleared": true})
}

// HistoryDeleted bumps the history generation and tells every client to
// resync (clients that miss the broadcast resync on reconnect via ?gen=).
func (s *Server) HistoryDeleted(ctx context.Context, reason string) {
	_, _ = s.store.BumpGeneration(ctx)
	s.hub.Broadcast(Message{Type: "resync_required", Reason: reason})
}

func (s *Server) handleExportPreview(w http.ResponseWriter, r *http.Request) {
	tid := r.URL.Query().Get("trace_id")
	var sess *correlate.Session
	var err error
	if tid != "" {
		sess, err = s.store.GetSessionByTrace(r.Context(), tid)
	} else {
		var list []correlate.Session
		list, _, err = s.store.ListSessions(r.Context(), sqlite.SessionFilter{Limit: 1})
		if len(list) > 0 {
			sess = &list[0]
		}
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if sess == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": s.cfg.ExportEnabled, "endpoint": s.cfg.ExportEndpoint, "preview": nil})
		return
	}
	spans, links, err := s.store.TraceSpans(r.Context(), sess.TraceID)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": s.cfg.ExportEnabled, "endpoint": s.cfg.ExportEndpoint, "preview": telemetry.ToOTLP(sess, spans, links)})
}
