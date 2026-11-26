package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentatlas/internal/config"
	"agentatlas/internal/correlate"
	"agentatlas/internal/event"
	"agentatlas/internal/storage/sqlite"
	"agentatlas/internal/telemetry"
)

func TestExportIsOffByDefault(t *testing.T) {
	if exportEnabled(config.Default()) {
		t.Fatal("export must be off by default")
	}
	c := config.Default()
	c.ExportEnabled, c.ExportEndpoint = true, "http://127.0.0.1:4318/v1/traces"
	if exportEnabled(c) {
		t.Fatal("export must require the preview acknowledgement")
	}
	c.ExportPreviewAck = true
	if !exportEnabled(c) {
		t.Fatal("explicit opt-in should enable export")
	}
}

func TestExportDrainsBacklogOldestFirstExactlyOnce(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	base := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	var evs []event.Event
	const N = 130 // more than one page
	for i := 0; i < N; i++ {
		sid := fmt.Sprintf("S%03d", i)
		for k, typ := range []string{event.TypeSessionStarted, event.TypeSessionEnded} {
			evs = append(evs, event.Event{SchemaVersion: 1, EventID: fmt.Sprintf("%s-%d", sid, k), Timestamp: base.Add(time.Duration(i)*time.Second + time.Duration(k)*time.Millisecond),
				HostID: "h", Harness: event.HarnessClaude, SessionID: sid, Type: typ, Evidence: event.EvidenceObserved, Privacy: event.PrivacyMetadataOnly})
		}
	}
	if _, err := st.Commit(ctx, evs, correlate.New(st)); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var p struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []struct {
						Attributes []struct {
							Key   string            `json:"key"`
							Value map[string]string `json:"value"`
						} `json:"attributes"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		json.Unmarshal(b, &p)
		for _, a := range p.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes {
			if a.Key == "atlas.session.id" {
				mu.Lock()
				got = append(got, a.Value["stringValue"])
				mu.Unlock()
			}
		}
	}))
	defer srv.Close()
	exp := &telemetry.Exporter{Endpoint: srv.URL}
	n, err := exportOnce(ctx, st, exp)
	if err != nil || n != N {
		t.Fatalf("exported %d (%v), want %d", n, err, N)
	}
	for i, s := range got {
		if want := fmt.Sprintf("S%03d", i); s != want {
			t.Fatalf("order: got %s at %d, want %s", s, i, want)
		}
	}
	if n, _ := exportOnce(ctx, st, exp); n != 0 {
		t.Fatalf("re-exported %d sessions", n)
	}
}

// An end committed late (e.g. drained from the spool) with an older timestamp
// must still be exported.
func TestExportFollowsCommitOrderNotTimestamps(t *testing.T) {
	ctx := context.Background()
	st, _ := sqlite.Open(filepath.Join(t.TempDir(), "a.db"))
	defer st.Close()
	base := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	mk := func(id, sid, typ string, sec int) event.Event {
		return event.Event{SchemaVersion: 1, EventID: id, Timestamp: base.Add(time.Duration(sec) * time.Second), HostID: "h", Harness: event.HarnessClaude, SessionID: sid, Type: typ, Evidence: event.EvidenceObserved, Privacy: event.PrivacyMetadataOnly}
	}
	proj := correlate.New(st)
	st.Commit(ctx, []event.Event{mk("c1", "C", event.TypeSessionStarted, 50), mk("b1", "B", event.TypeSessionStarted, 100), mk("b2", "B", event.TypeSessionEnded, 200)}, proj)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	exp := &telemetry.Exporter{Endpoint: srv.URL}
	if n, _ := exportOnce(ctx, st, exp); n != 1 {
		t.Fatalf("first export %d", n)
	}
	st.Commit(ctx, []event.Event{mk("c2", "C", event.TypeSessionEnded, 150)}, proj) // late, older timestamp
	if n, _ := exportOnce(ctx, st, exp); n != 1 {
		t.Fatalf("late-ended session not exported: %d", n)
	}
}

func TestExporterKeepsTelemetryOnTheConfiguredEndpoint(t *testing.T) {
	var redirected atomic.Bool
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/traces" {
			http.Redirect(w, r, "/other", http.StatusTemporaryRedirect)
			return
		}
		redirected.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	exporter := telemetry.Exporter{Endpoint: collector.URL + "/v1/traces", Client: collector.Client()}
	if err := exporter.Export(t.Context(), map[string]any{"resourceSpans": []any{}}); err == nil || redirected.Load() {
		t.Fatalf("redirect must be reported without forwarding telemetry: %v", err)
	}
}
