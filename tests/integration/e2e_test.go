// Package integration runs the real atlas/atlasd binaries end to end.
package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

var binDir string

func TestMain(m *testing.M) {
	d, err := os.MkdirTemp("", "atlas-bin")
	if err != nil {
		panic(err)
	}
	binDir = d
	root, _ := filepath.Abs(filepath.Join("..", ".."))
	for _, c := range []string{"atlas", "atlasd"} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(d, c), "./cmd/"+c)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintln(os.Stderr, string(out))
			panic(err)
		}
	}
	code := m.Run()
	os.RemoveAll(d)
	os.Exit(code)
}

type env struct {
	home   string
	port   int
	daemon *exec.Cmd
}

func freePort(t testing.TB) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func newEnv(t testing.TB) *env {
	home, err := os.MkdirTemp("", "atl")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{home: home, port: freePort(t)}
	os.WriteFile(filepath.Join(home, "config.json"), []byte(fmt.Sprintf(`{"addr":"127.0.0.1:%d"}`, e.port)), 0o600)
	t.Cleanup(func() {
		e.stop(t, syscall.SIGTERM)
		os.RemoveAll(home)
	})
	return e
}

func (e *env) start(t testing.TB) {
	t.Helper()
	cmd := exec.Command(filepath.Join(binDir, "atlasd"), "--home", e.home, "--log", filepath.Join(e.home, "atlasd.log"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	e.daemon = cmd
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if r, err := http.Get(e.url("/api/v1/health")); err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := os.ReadFile(filepath.Join(e.home, "atlasd.log"))
	t.Fatalf("atlasd did not start: %s", b)
}

func (e *env) stop(t testing.TB, sig syscall.Signal) {
	if e.daemon == nil || e.daemon.Process == nil {
		return
	}
	_ = e.daemon.Process.Signal(sig)
	_, _ = e.daemon.Process.Wait()
	e.daemon = nil
}

func (e *env) url(p string) string { return fmt.Sprintf("http://127.0.0.1:%d%s", e.port, p) }

func (e *env) token() string {
	b, _ := os.ReadFile(filepath.Join(e.home, "token"))
	return strings.TrimSpace(string(b))
}

func (e *env) get(t testing.TB, p string, out any) {
	t.Helper()
	req, _ := http.NewRequest("GET", e.url(p), nil)
	req.Header.Set("Authorization", "Bearer "+e.token())
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("GET %s: %d", p, r.StatusCode)
	}
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}

// hook runs the real shim like a harness would, returning stdout and exit code.
func (e *env) hook(t testing.TB, harness, payload string) (string, int, time.Duration) {
	t.Helper()
	cmd := exec.Command(filepath.Join(binDir, "atlas"), "hook", "--harness", harness)
	cmd.Env = append(os.Environ(), "ATLAS_HOME="+e.home)
	cmd.Stdin = strings.NewReader(payload)
	var out bytes.Buffer
	cmd.Stdout = &out
	start := time.Now()
	err := cmd.Run()
	d := time.Since(start)
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return out.String(), code, d
}

func fixture(t testing.TB, name string) []string {
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func (e *env) waitEvents(t testing.TB, n int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var h struct {
			Storage struct {
				Events int64 `json:"events"`
			} `json:"storage"`
		}
		e.get(t, "/api/v1/health", &h)
		if h.Storage.Events >= n {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("expected %d events", n)
}

type sessionsResp struct {
	Sessions []struct {
		ID           string `json:"id"`
		Harness      string `json:"harness"`
		Status       string `json:"status"`
		ToolCount    int    `json:"tool_count"`
		ErrorCount   int    `json:"error_count"`
		DeniedCount  int    `json:"denied_count"`
		UnknownCount int    `json:"unknown_count"`
		TraceID      string `json:"trace_id"`
	} `json:"sessions"`
}

// Acceptance: "Agent workflow remains functional when the daemon is stopped."
func TestHookWithDaemonDownIsSilentFastAndRecovers(t *testing.T) {
	e := newEnv(t)
	lines := fixture(t, "claude-hooks/session-real-2.1.330.jsonl")
	var worst time.Duration
	for _, l := range lines {
		out, code, d := e.hook(t, "claude", l)
		if out != "" || code != 0 {
			t.Fatalf("hook must print nothing and exit 0 (got %q, %d)", out, code)
		}
		if d > worst {
			worst = d
		}
	}
	// Bound = the shim's hard ceiling (cmd/atlas runHook); typical is a few ms.
	if worst > 2600*time.Millisecond {
		t.Errorf("hook too slow with daemon down: %v", worst)
	}
	// garbage and empty input also exit 0 silently
	for _, p := range []string{"", "{", strings.Repeat("z", 2<<20)} {
		if out, code, _ := e.hook(t, "claude", p); out != "" || code != 0 {
			t.Fatalf("bad input: %q %d", out, code)
		}
	}
	e.start(t) // drains the spool
	e.waitEvents(t, 16)
	var s sessionsResp
	e.get(t, "/api/v1/sessions", &s)
	if len(s.Sessions) != 1 || s.Sessions[0].Status != "ended" || s.Sessions[0].ToolCount != 6 {
		t.Fatalf("recovered sessions %+v", s.Sessions)
	}
	// Drop markers are drained after the spooled envelopes, so poll for the gap.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var h map[string]any
		e.get(t, "/api/v1/health", &h)
		ing := h["ingest"].(map[string]any)
		if ing["dropped"].(float64) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("oversize payload should be reported as a telemetry gap: %v", ing)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Acceptance: restart preserves committed events and span history; crash recovery.
func TestCrashRestartPreservesHistory(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	lines := fixture(t, "codex-hooks/session-documented-synthetic.jsonl")
	for _, l := range lines[:8] {
		e.hook(t, "codex", l)
	}
	e.waitEvents(t, 8)
	e.stop(t, syscall.SIGKILL) // crash
	e.start(t)
	for _, l := range lines[8:] {
		e.hook(t, "codex", l)
	}
	e.waitEvents(t, int64(len(lines)))
	var s sessionsResp
	e.get(t, "/api/v1/sessions", &s)
	if len(s.Sessions) != 1 || s.Sessions[0].ToolCount != 5 || s.Sessions[0].ErrorCount != 1 {
		t.Fatalf("after crash+restart %+v", s.Sessions)
	}
	var tr struct {
		Spans []struct {
			ToolCallID string `json:"tool_call_id"`
			Status     string `json:"status"`
		} `json:"spans"`
	}
	e.get(t, "/api/v1/traces/"+s.Sessions[0].TraceID, &tr)
	st := map[string]string{}
	for _, sp := range tr.Spans {
		st[sp.ToolCallID] = sp.Status
	}
	if st["call_A"] != "failed" || st["call_B"] != "completed" || st["call_D"] != "completed" || st["call_E"] != "running" {
		t.Fatalf("span statuses after restart: %v", st)
	}
	var evs struct {
		Events []struct {
			Seq int64 `json:"seq"`
		} `json:"events"`
	}
	e.get(t, "/api/v1/sessions/"+s.Sessions[0].ID+"/events?limit=100", &evs)
	for i := 1; i < len(evs.Events); i++ {
		if evs.Events[i].Seq <= evs.Events[i-1].Seq {
			t.Fatal("sequence not monotonic across restart")
		}
	}
}

// Acceptance: no raw prompts/tool output persisted; redaction before storage.
func TestNoRawContentInDatabaseFiles(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	n := 0
	for _, f := range []struct{ h, name string }{{"claude", "claude-hooks/session-real-2.1.330.jsonl"}, {"claude", "claude-hooks/session-real-mcp-denied-2.1.330.jsonl"}, {"codex", "codex-hooks/session-documented-synthetic.jsonl"}} {
		for _, l := range fixture(t, f.name) {
			e.hook(t, f.h, l)
			n++
		}
	}
	e.waitEvents(t, 40)
	e.stop(t, syscall.SIGTERM)
	var all []byte
	for _, suffix := range []string{"", "-wal", "-shm"} {
		b, _ := os.ReadFile(filepath.Join(e.home, "atlas.db"+suffix))
		all = append(all, b...)
	}
	for _, bad := range []string{"SECRET-", "SYNTHETIC-", "sk-live-abcdef", "ghp_abcdefghij", "Bearer abcdefghijklmnop", "user:pw@", "transcript_path", "hello atlas", "echo: ping"} {
		if bytes.Contains(all, []byte(bad)) {
			t.Errorf("database files contain %q", bad)
		}
	}
	for _, p := range []string{"atlas.db", "token"} {
		fi, err := os.Stat(filepath.Join(e.home, p))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s perms %v %v", p, fi.Mode().Perm(), err)
		}
	}
}

func percentile(ds []time.Duration, p float64) time.Duration {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[int(float64(len(ds)-1)*p)]
}

// Targets (design §14): burst 100 events/s; collector-to-UI p95 ≤ 500ms.
func TestBurstIngestionAndStreamLatency(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, fmt.Sprintf("ws://127.0.0.1:%d/api/v1/stream", e.port), &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + e.token()}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 22)
	const N = 1000
	sent := sync.Map{}
	recvd := make(chan time.Duration, N)
	go func() {
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m struct {
				Type  string `json:"type"`
				Event struct {
					ToolCallID string `json:"tool_call_id"`
				} `json:"event"`
			}
			json.Unmarshal(b, &m)
			if m.Type == "delta" {
				if v, ok := sent.Load(m.Event.ToolCallID); ok {
					recvd <- time.Since(v.(time.Time))
				}
			}
		}
	}()
	// read socket path from the daemon log line is not needed: same derivation as config.
	sock := socketPathFor(e.home)
	start := time.Now()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := 0; i < N; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			id := fmt.Sprintf("call_%d", i)
			raw := fmt.Sprintf(`{"session_id":"burst","turn_id":"t","cwd":"/w","hook_event_name":"PreToolUse","model":"m","permission_mode":"default","tool_name":"Bash","tool_input":{"command":"echo %d"},"tool_use_id":"%s"}`, i, id)
			envl, _ := json.Marshal(map[string]any{"v": 1, "envelope_id": fmt.Sprintf("burst%06d", i), "received_at": time.Now().UTC(), "harness": "codex", "host_id": "local", "raw": json.RawMessage(raw)})
			sent.Store(id, time.Now())
			conn, err := net.Dial("unix", sock)
			if err != nil {
				t.Error(err)
				return
			}
			conn.Write(append(envl, '\n'))
			line, _ := bufio.NewReader(conn).ReadString('\n')
			conn.Close()
			if strings.TrimSpace(line) != "ok" {
				t.Errorf("reply %q", line)
			}
		}(i)
		time.Sleep(4 * time.Millisecond) // ~250/s offered load
	}
	wg.Wait()
	var lat []time.Duration
	for len(lat) < N {
		select {
		case d := <-recvd:
			lat = append(lat, d)
		case <-time.After(10 * time.Second):
			t.Fatalf("received %d/%d deltas", len(lat), N)
		}
	}
	elapsed := time.Since(start)
	rate := float64(N) / elapsed.Seconds()
	p95 := percentile(lat, 0.95)
	t.Logf("burst: %d events in %v (%.0f ev/s); collector→stream latency p50=%v p95=%v", N, elapsed.Round(time.Millisecond), rate, percentile(lat, 0.5), p95)
	if rate < 100 {
		t.Errorf("ingestion rate %.0f ev/s below 100 ev/s target", rate)
	}
	if p95 > 500*time.Millisecond {
		t.Errorf("p95 latency %v above 500ms target", p95)
	}
	// RSS of the daemon (target < 150MB idle; measured here after the burst)
	if runtime.GOOS == "darwin" {
		out, _ := exec.Command("ps", "-o", "rss=", "-p", fmt.Sprint(e.daemon.Process.Pid)).Output()
		t.Logf("atlasd RSS after burst: %s KB", strings.TrimSpace(string(out)))
	}
}

func socketPathFor(home string) string {
	p := filepath.Join(home, "atlasd.sock")
	if len(p) <= 100 {
		return p
	}
	panic("test home too long")
}

// Target (design §14): hook overhead p95 ≤ 50ms (process spawn + send) with the daemon running.
func TestHookOverhead(t *testing.T) {
	e := newEnv(t)
	e.start(t)
	payload := fixture(t, "claude-hooks/session-real-2.1.330.jsonl")[2]
	var ds []time.Duration
	for i := 0; i < 100; i++ {
		_, code, d := e.hook(t, "claude", payload)
		if code != 0 {
			t.Fatal("non-zero exit")
		}
		ds = append(ds, d)
	}
	p50, p95 := percentile(ds, 0.5), percentile(ds, 0.95)
	t.Logf("atlas hook wall time (incl. process spawn): p50=%v p95=%v", p50, p95)
	if p95 > 50*time.Millisecond {
		t.Errorf("hook p95 %v exceeds 50ms target", p95)
	}
}

// Live end-to-end against a real Claude Code install. Opt-in: spends tokens.
// Uses an isolated --settings file; never touches ~/.claude/settings.json.
func TestLiveClaude(t *testing.T) {
	if os.Getenv("ATLAS_E2E_CLAUDE") != "1" {
		t.Skip("set ATLAS_E2E_CLAUDE=1 to run against the installed claude CLI")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not installed")
	}
	e := newEnv(t)
	e.start(t)
	settings := filepath.Join(e.home, "claude-settings.json")
	inst := exec.Command(filepath.Join(binDir, "atlas"), "install-hooks", "--harness", "claude", "--claude-settings", settings, "--atlas-bin", filepath.Join(binDir, "atlas"))
	inst.Env = append(os.Environ(), "ATLAS_HOME="+e.home)
	if out, err := inst.CombinedOutput(); err != nil {
		t.Fatalf("install: %v %s", err, out)
	}
	work := t.TempDir()
	os.WriteFile(filepath.Join(work, "note.txt"), []byte("atlas e2e\n"), 0o644)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "-p", "Use the Read tool to read note.txt, then say done.",
		"--restricted", "--strict-mcp-config", "--no-session-persistence", "--settings", settings,
		"--tools", "Read", "--allowedTools", "Read", "--max-turns", "3", "--model", "haiku")
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "ATLAS_HOME="+e.home)
	cmd.Stdin = strings.NewReader("")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("claude: %v %s", err, out)
	}
	e.waitEvents(t, 6)
	var s sessionsResp
	e.get(t, "/api/v1/sessions?harness=claude", &s)
	if len(s.Sessions) != 1 || s.Sessions[0].ToolCount < 1 {
		t.Fatalf("live sessions %+v", s.Sessions)
	}
	var sr struct {
		Spans []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"spans"`
	}
	e.get(t, "/api/v1/search?tool=Read", &sr)
	if len(sr.Spans) == 0 || sr.Spans[0].Status != "completed" {
		t.Fatalf("Read span %+v", sr.Spans)
	}
}

func TestCLILifecycleOnEphemeralPort(t *testing.T) {
	e := newEnv(t)
	if err := os.WriteFile(filepath.Join(e.home, "config.json"), []byte(`{"addr":"127.0.0.1:0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := func(args ...string) ([]byte, error) {
		cmd := exec.Command(filepath.Join(binDir, "atlas"), args...)
		cmd.Env = append(os.Environ(), "ATLAS_HOME="+e.home)
		return cmd.CombinedOutput()
	}
	t.Cleanup(func() { _, _ = cli("stop") })
	for _, args := range [][]string{{"start"}, {"status"}, {"pause"}, {"resume"}} {
		if out, err := cli(args...); err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
	}
	out, err := cli("open", "--print")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	url, code, ok := strings.Cut(strings.TrimSpace(string(out)), "/#signin=")
	if !ok || code == "" || strings.HasSuffix(url, ":0") {
		t.Fatal("open did not return a one-time URL on the bound port")
	}
	r, err := http.Get(url + "/")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("embedded dashboard: HTTP %d", r.StatusCode)
	}
	if out, err := cli("stop"); err != nil {
		t.Fatalf("stop: %v %s", err, out)
	}
	if _, err := cli("status"); err == nil {
		t.Fatal("status succeeded after daemon stopped")
	}
	if _, err := cli("install-hooks", "--harness", "unknown"); err == nil {
		t.Fatal("install accepted an unknown harness")
	}
}

func TestLongAtlasHomeWorksAcrossTempDirectories(t *testing.T) {
	home := filepath.Join(t.TempDir(), strings.Repeat("local-data-", 12))
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"addr":"127.0.0.1:0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	launcherTemp, terminalTemp := t.TempDir(), t.TempDir()
	run := func(temp, input string, args ...string) ([]byte, error) {
		cmd := exec.Command(filepath.Join(binDir, "atlas"), args...)
		cmd.Env = append(os.Environ(), "ATLAS_HOME="+home, "TMPDIR="+temp)
		cmd.Stdin = strings.NewReader(input)
		return cmd.CombinedOutput()
	}
	if out, err := run(launcherTemp, "", "start"); err != nil {
		t.Fatalf("start: %v %s", err, out)
	}
	t.Cleanup(func() { _, _ = run(launcherTemp, "", "stop") })
	fixture := fixture(t, "codex-hooks/session-documented-synthetic.jsonl")[0]
	if out, err := run(terminalTemp, fixture, "hook", "--harness", "codex"); err != nil || len(out) != 0 {
		t.Fatalf("hook: %v %s", err, out)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		out, err := run(terminalTemp, "", "status")
		if err != nil {
			t.Fatalf("status across TMPDIRs: %v %s", err, out)
		}
		var health struct {
			Storage    struct{ Events int }
			SpoolBytes int `json:"spool_bytes"`
		}
		if err := json.Unmarshal(out, &health); err != nil {
			t.Fatal(err)
		}
		if health.SpoolBytes != 0 {
			t.Fatal("hook spooled instead of reaching the daemon directly")
		}
		if health.Storage.Events == 1 {
			report, err := run(terminalTemp, "", "doctor")
			if err != nil || !strings.Contains(string(report), "codex observed   SessionStart (last ") {
				t.Fatalf("doctor did not report the observed hook: %v\n%s", err, report)
			}
			if out, err := run(terminalTemp, "", "stop"); err != nil {
				t.Fatalf("stop across TMPDIRs: %v %s", err, out)
			}
			return
		}
	}
	t.Fatal("hook event never committed")
}
