package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"

	"agentatlas/internal/config"
	"agentatlas/internal/correlate"
	"agentatlas/internal/event"
	"agentatlas/internal/ingest"
	"agentatlas/internal/storage/sqlite"
)

type harness struct {
	srv   *httptest.Server
	store *sqlite.Store
	pipe  *ingest.Pipeline
	hub   *Hub
	token string
	host  string
	stop  func()
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsFor(home)
	store, err := sqlite.Open(paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub()
	pipe := ingest.NewPipeline(store, "h", hub.Publish, 1024, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pipe.Run(ctx); close(done) }()
	ts := httptest.NewUnstartedServer(nil)
	addr := ts.Listener.Addr().String()
	token := strings.Repeat("ab", 32)
	ui := fstest.MapFS{"index.html": {Data: []byte("<html>ui</html>")}}
	s, err := New(store, pipe, hub, token, addr, paths, config.Default(), ui)
	if err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = s.Handler()
	ts.Start()
	h := &harness{srv: ts, store: store, pipe: pipe, hub: hub, token: token, host: addr}
	h.stop = func() { ts.Close(); cancel(); <-done; store.Close() }
	t.Cleanup(h.stop)
	return h
}

func (h *harness) req(t *testing.T, method, path string, hdr map[string]string, body string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, r)
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func ev(i int, typ, call string) event.Event {
	return event.Event{SchemaVersion: 1, EventID: "e" + correlate.Itoa(int64(i)), Timestamp: time.Now().UTC(), HostID: "h",
		Harness: event.HarnessClaude, SessionID: "S1", TurnID: "T", ToolCallID: call, Type: typ, Evidence: event.EvidenceObserved,
		Privacy: event.PrivacyMetadataOnly, Tool: &event.Tool{Name: "Bash"}}
}

func (h *harness) commit(t *testing.T, evs ...event.Event) {
	t.Helper()
	if err := h.pipe.SubmitWait(context.Background(), evs); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	want := evs[len(evs)-1].EventID
	for time.Now().Before(deadline) {
		if all, _ := h.store.EventsAfter(context.Background(), 0, 5000); len(all) > 0 {
			for _, se := range all {
				if se.Event.EventID == want {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("events not committed")
}

func TestAccessControls(t *testing.T) {
	h := newHarness(t)
	bearer := map[string]string{"Authorization": "Bearer " + h.token}
	if r := h.req(t, "GET", "/api/v1/sessions", nil, ""); r.StatusCode != 401 {
		t.Errorf("unauthenticated: %d", r.StatusCode)
	}
	if r := h.req(t, "GET", "/api/v1/sessions", map[string]string{"Authorization": "Bearer wrong"}, ""); r.StatusCode != 401 {
		t.Errorf("wrong token: %d", r.StatusCode)
	}
	if r := h.req(t, "GET", "/api/v1/sessions", bearer, ""); r.StatusCode != 200 {
		t.Errorf("bearer: %d", r.StatusCode)
	}
	// DNS rebinding: foreign Host header is refused even with a valid token.
	if r := h.req(t, "GET", "/api/v1/sessions", map[string]string{"Authorization": "Bearer " + h.token, "Host": "evil.example:80"}, ""); r.StatusCode != 403 {
		t.Errorf("foreign host: %d", r.StatusCode)
	}
	// Cross-site Origin refused.
	if r := h.req(t, "GET", "/api/v1/sessions", map[string]string{"Authorization": "Bearer " + h.token, "Origin": "https://evil.example"}, ""); r.StatusCode != 403 {
		t.Errorf("foreign origin: %d", r.StatusCode)
	}
	// CORS preflight refused, and no ACAO header ever.
	r := h.req(t, "OPTIONS", "/api/v1/sessions", map[string]string{"Origin": "http://" + h.host}, "")
	if r.StatusCode != 403 || r.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("preflight: %d %q", r.StatusCode, r.Header.Get("Access-Control-Allow-Origin"))
	}
	// CSRF: state change without custom header refused.
	if r := h.req(t, "POST", "/api/v1/admin/pause", bearer, `{"paused":true}`); r.StatusCode != 403 {
		t.Errorf("csrf: %d", r.StatusCode)
	}
	hdr := map[string]string{"Authorization": "Bearer " + h.token, "X-Atlas-CSRF": "1", "Origin": "http://" + h.host}
	if r := h.req(t, "POST", "/api/v1/admin/pause", hdr, `{"paused":true}`); r.StatusCode != 200 || !h.pipe.Paused() {
		t.Errorf("pause: %d paused=%v", r.StatusCode, h.pipe.Paused())
	}
	h.req(t, "POST", "/api/v1/admin/pause", hdr, `{"paused":false}`)
	// health: public minimal, authenticated detailed
	var pub map[string]any
	json.NewDecoder(h.req(t, "GET", "/api/v1/health", nil, "").Body).Decode(&pub)
	if _, leaked := pub["storage"]; leaked || pub["ok"] != true {
		t.Errorf("public health %v", pub)
	}
	// security headers
	r = h.req(t, "GET", "/", nil, "")
	if !strings.Contains(r.Header.Get("Content-Security-Policy"), "default-src 'self'") || r.Header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("headers %v", r.Header)
	}
}

func TestBrowserSignInUsesSingleUseCodeAndOriginScopedSession(t *testing.T) {
	h := newHarness(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if resp, err := client.Get(h.srv.URL + "/auth?token=" + h.token); err != nil || resp.StatusCode != 401 {
		t.Fatal("long-lived token must not be accepted in URLs", err)
	} else {
		resp.Body.Close()
	}
	var code struct{ Code string }
	issued := h.req(t, "POST", "/api/v1/admin/signin-code", map[string]string{"Authorization": "Bearer " + h.token, "X-Atlas-CSRF": "1"}, "{}")
	if err := json.NewDecoder(issued.Body).Decode(&code); err != nil || code.Code == "" {
		t.Fatal("could not issue sign-in code", err)
	}
	resp, err := client.Get(h.srv.URL + "/auth?code=" + code.Code)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "/#signin="+code.Code || len(resp.Cookies()) != 0 {
		t.Fatalf("unexpected sign-in redirect: %d %v", resp.StatusCode, resp.Header)
	}
	headers := map[string]string{"X-Atlas-CSRF": "1", "Origin": h.srv.URL}
	body := `{"code":"` + code.Code + `"}`
	exchange := h.req(t, "POST", "/api/v1/auth/exchange", headers, body)
	var session struct{ Session string }
	if err := json.NewDecoder(exchange.Body).Decode(&session); err != nil || exchange.StatusCode != 200 || session.Session == "" || session.Session == h.token || len(exchange.Cookies()) != 0 {
		t.Fatal("sign-in exchange failed or returned the wrong kind of credential", err)
	}
	if status := h.req(t, "POST", "/api/v1/auth/exchange", headers, body).StatusCode; status != 401 {
		t.Fatalf("sign-in code must be single-use: %d", status)
	}
	if status := h.req(t, "GET", "/api/v1/sessions", map[string]string{"Authorization": "AtlasSession " + session.Session}, "").StatusCode; status != 200 {
		t.Fatalf("browser session: %d", status)
	}
	if status := h.req(t, "GET", "/api/v1/sessions", map[string]string{"Authorization": "Bearer " + session.Session}, "").StatusCode; status != 401 {
		t.Fatalf("browser session accepted as API token: %d", status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.srv.URL, "http")+"/api/v1/stream", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{h.srv.URL}}, Subprotocols: []string{sessionProtocol, sessionPrefix + session.Session},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	if ws.Subprotocol() != sessionProtocol || readMsg(t, ws).Type != "hello" {
		t.Fatal("browser stream did not negotiate the public session protocol")
	}
}

func dialWS(t *testing.T, h *harness, after int64) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := "ws://" + h.host + "/api/v1/stream?after_seq=" + correlate.Itoa(after)
	c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + h.token}, "Origin": {"http://" + h.host}}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func readMsg(t *testing.T, c *websocket.Conn) Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var m Message
	json.Unmarshal(b, &m)
	return m
}

func TestStreamLiveResumeAndResync(t *testing.T) {
	h := newHarness(t)
	h.commit(t, ev(1, event.TypeSessionStarted, ""), ev(2, event.TypeToolStarted, "A"))

	c := dialWS(t, h, 0)
	if m := readMsg(t, c); m.Type != "hello" || m.Cursor != 2 {
		t.Fatalf("hello %+v", m)
	}
	for want := int64(1); want <= 2; want++ { // cursor 0 catches up first
		if m := readMsg(t, c); m.Seq != want || !m.Replay {
			t.Fatalf("catch-up %+v", m)
		}
	}
	h.commit(t, ev(3, event.TypeToolCompleted, "A"))
	m := readMsg(t, c)
	if m.Type != "delta" || m.Seq != 3 || len(m.Spans) == 0 || m.Spans[0].Status != event.StatusCompleted {
		t.Fatalf("live delta %+v", m)
	}
	c.Close(websocket.StatusNormalClosure, "")

	// missed events while disconnected
	h.commit(t, ev(4, event.TypeToolStarted, "B"), ev(5, event.TypeToolStarted, "C"))
	c2 := dialWS(t, h, 3)
	if m := readMsg(t, c2); m.Type != "hello" {
		t.Fatalf("hello2 %+v", m)
	}
	seen := map[int64]bool{}
	for i := 0; i < 2; i++ {
		m := readMsg(t, c2)
		if m.Type != "delta" || !m.Replay || seen[m.Seq] {
			t.Fatalf("catch-up %+v", m)
		}
		seen[m.Seq] = true
	}
	if !seen[4] || !seen[5] {
		t.Fatalf("catch-up seqs %v", seen)
	}
	h.commit(t, ev(6, event.TypeToolCompleted, "B"))
	if m := readMsg(t, c2); m.Seq != 6 || m.Replay {
		t.Fatalf("post catch-up live %+v", m)
	}
	c2.Close(websocket.StatusNormalClosure, "")

	// cursor beyond retained history (after clear) → resync_required
	if err := h.store.ClearHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	c3 := dialWS(t, h, 2)
	readMsg(t, c3) // hello
	if m := readMsg(t, c3); m.Type != "resync_required" {
		t.Fatalf("expected resync_required, got %+v", m)
	}
}

func TestReplayIsDeterministicAndReadOnly(t *testing.T) {
	h := newHarness(t)
	h.commit(t, ev(1, event.TypeSessionStarted, ""), ev(2, event.TypeToolStarted, "A"), ev(3, event.TypeToolStarted, "B"), ev(4, event.TypeToolCompleted, "A"), ev(5, event.TypeSessionEnded, ""))
	sid := correlate.SessionID("h", event.HarnessClaude, "S1")
	bearer := map[string]string{"Authorization": "Bearer " + h.token}
	before, _ := h.store.Stats(context.Background())
	b1, _ := io.ReadAll(h.req(t, "GET", "/api/v1/sessions/"+sid+"/replay", bearer, "").Body)
	b2, _ := io.ReadAll(h.req(t, "GET", "/api/v1/sessions/"+sid+"/replay", bearer, "").Body)
	if string(b1) != string(b2) {
		t.Fatal("replay output differs between runs")
	}
	var r struct {
		Frames        []correlate.Delta `json:"frames"`
		ExecutesTools bool              `json:"executes_tools"`
	}
	json.Unmarshal(b1, &r)
	if len(r.Frames) != 5 || r.ExecutesTools {
		t.Fatalf("frames %d executes=%v", len(r.Frames), r.ExecutesTools)
	}
	last := r.Frames[len(r.Frames)-1]
	if last.Session.Status != "ended" || last.Session.UnknownCount != 1 {
		t.Fatalf("final replay state %+v", last.Session)
	}
	after, _ := h.store.Stats(context.Background())
	if before.Events != after.Events || before.Spans != after.Spans {
		t.Fatal("replay modified storage")
	}
}

func TestSearchAndTraceEndpoints(t *testing.T) {
	h := newHarness(t)
	e := ev(1, event.TypeToolStarted, "A")
	e.Resources = []event.Resource{{Kind: "file", Label: "src/auth.ts", Relation: "reads", Evidence: event.EvidenceInferred}}
	h.commit(t, e, ev(2, event.TypeToolFailed, "A"))
	bearer := map[string]string{"Authorization": "Bearer " + h.token}
	var sr struct {
		Spans []sqlite.SpanResult `json:"spans"`
		Total int                 `json:"total"`
	}
	json.NewDecoder(h.req(t, "GET", "/api/v1/search?tool=Bash&status=failed&harness=claude&resource=auth&evidence=inferred", bearer, "").Body).Decode(&sr)
	if sr.Total != 1 {
		t.Fatalf("search total %d", sr.Total)
	}
	var otlp map[string]any
	json.NewDecoder(h.req(t, "GET", "/api/v1/traces/"+sr.Spans[0].TraceID+"?format=otlp", bearer, "").Body).Decode(&otlp)
	if _, ok := otlp["resourceSpans"]; !ok {
		t.Fatalf("otlp %v", otlp)
	}
	if r := h.req(t, "GET", "/api/v1/traces/nope", bearer, ""); r.StatusCode != 404 {
		t.Errorf("missing trace %d", r.StatusCode)
	}
}

func TestIngestSocketEndToEnd(t *testing.T) {
	h := newHarness(t)
	sockDir, _ := os.MkdirTemp("", "as")
	defer os.RemoveAll(sockDir)
	sock, err := ingest.Listen(filepath.Join(sockDir, "s.sock"), h.pipe)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sock.Serve(ctx)
	send := func(line string) string {
		c, err := net.Dial("unix", filepath.Join(sockDir, "s.sock"))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Write([]byte(line + "\n"))
		b := make([]byte, 32)
		n, _ := c.Read(b)
		return strings.TrimSpace(string(b[:n]))
	}
	raw := `{"session_id":"X","cwd":"/w","hook_event_name":"PreToolUse","tool_name":"Read","tool_input":{"file_path":"/w/a.go"},"tool_use_id":"t9"}`
	env, _ := json.Marshal(event.Envelope{V: 1, EnvelopeID: "abc", ReceivedAt: time.Now(), Harness: "claude", HostID: "h", Raw: json.RawMessage(raw)})
	if r := send(string(env)); r != ingest.ReplyOK {
		t.Fatalf("reply %q", r)
	}
	if r := send("garbage"); r != ingest.ReplyRejected {
		t.Fatalf("garbage reply %q", r)
	}
	bad, _ := json.Marshal(event.Envelope{V: 1, EnvelopeID: "x", ReceivedAt: time.Now(), Harness: "claude", Raw: json.RawMessage(`{"hook_event_name":"PreToolUse"}`)})
	if r := send(string(bad)); r != ingest.ReplyRejected {
		t.Fatalf("missing session reply %q", r)
	}
	h.pipe.SetPaused(true)
	if r := send(string(env)); r != ingest.ReplyPaused {
		t.Fatalf("paused reply %q", r)
	}
	h.pipe.SetPaused(false)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := h.store.Stats(context.Background()); st.Events == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("event not committed via socket")
}

// Regression tests for review findings.
func TestStreamCursorZeroCatchesUpAndGenerationForcesResync(t *testing.T) {
	h := newHarness(t)
	// client took its snapshot at cursor 0 (empty DB); events then commit before it subscribes
	h.commit(t, ev(1, event.TypeSessionStarted, ""), ev(2, event.TypeToolStarted, "A"))
	c := dialWS(t, h, 0)
	readMsg(t, c) // hello
	for want := int64(1); want <= 2; want++ {
		if m := readMsg(t, c); m.Type != "delta" || m.Seq != want {
			t.Fatalf("cursor-0 catch-up: %+v", m)
		}
	}
	c.Close(websocket.StatusNormalClosure, "")
	// history generation changed while the client was away → resync even if the cursor looks current
	h.store.BumpGeneration(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c2, _, err := websocket.Dial(ctx, "ws://"+h.host+"/api/v1/stream?after_seq=2&gen=0", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + h.token}}})
	if err != nil {
		t.Fatal(err)
	}
	readMsg(t, c2)
	if m := readMsg(t, c2); m.Type != "resync_required" {
		t.Fatalf("stale generation not resynced: %+v", m)
	}
}

func TestCatchUpIncludesSpansClosedBySessionEnd(t *testing.T) {
	h := newHarness(t)
	h.commit(t, ev(1, event.TypeSessionStarted, ""), ev(2, event.TypeToolStarted, "A"))
	h.commit(t, ev(3, event.TypeSessionEnded, ""))
	c := dialWS(t, h, 2)
	readMsg(t, c)
	m := readMsg(t, c)
	var toolClosed bool
	for _, sp := range m.Spans {
		if sp.ToolCallID == "A" && sp.Status == event.StatusUnknown {
			toolClosed = true
		}
	}
	if m.Seq != 3 || !toolClosed {
		t.Fatalf("catch-up of session.ended missing closed tool span: %+v", m.Spans)
	}
}

func TestClearHistoryDoesNotResurrectCachedRows(t *testing.T) {
	h := newHarness(t)
	h.commit(t, ev(1, event.TypeSessionStarted, ""), ev(2, event.TypeToolStarted, "A"), ev(3, event.TypeToolFailed, "A"))
	hdr := map[string]string{"Authorization": "Bearer " + h.token, "X-Atlas-CSRF": "1"}
	if r := h.req(t, "POST", "/api/v1/admin/clear-history", hdr, `{"confirm":"delete all history"}`); r.StatusCode != 200 {
		t.Fatalf("clear: %d", r.StatusCode)
	}
	h.commit(t, ev(4, event.TypeToolStarted, "B"))
	sess, _, _ := h.store.ListSessions(context.Background(), sqlite.SessionFilter{})
	if len(sess) != 1 || sess[0].ToolCount != 1 || sess[0].ErrorCount != 0 {
		t.Fatalf("stale projector state resurrected: %+v", sess)
	}
	spans, _, _ := h.store.TraceSpans(context.Background(), sess[0].TraceID)
	var root bool
	for _, sp := range spans {
		if sp.Kind == correlate.KindSession {
			root = true
		}
	}
	if !root {
		t.Fatal("root span missing after clear (projector cache not reset)")
	}
	if h.store.Generation(context.Background()) != 1 {
		t.Fatal("generation not bumped on clear")
	}
}
