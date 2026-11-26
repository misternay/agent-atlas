package adapters_test

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agentatlas/internal/adapters"
	"agentatlas/internal/event"
	"agentatlas/internal/normalize"
	"agentatlas/internal/security"
)

// LoadFixture reads a JSONL fixture of native payloads.
func loadFixture(t *testing.T, name string) [][]byte {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if len(strings.TrimSpace(sc.Text())) > 0 {
			out = append(out, append([]byte(nil), sc.Bytes()...))
		}
	}
	return out
}

func normalizeAll(t *testing.T, harness string, payloads [][]byte) []event.Event {
	t.Helper()
	var all []event.Event
	base := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	for i, p := range payloads {
		env := &event.Envelope{V: 1, EnvelopeID: "env" + string(rune('a'+i%26)) + strings.Repeat("x", i/26), ReceivedAt: base.Add(time.Duration(i) * time.Second), Harness: harness, HostID: "test-host", Raw: p}
		evs, err := normalize.Envelope(env)
		if err != nil {
			t.Fatalf("payload %d: %v", i, err)
		}
		all = append(all, evs...)
	}
	return all
}

func assertNoLeaks(t *testing.T, evs []event.Event) {
	t.Helper()
	b, _ := json.Marshal(evs)
	s := string(b)
	for _, bad := range []string{"SECRET-", "SYNTHETIC-", "/Users/dev", "ghp_abcdef", "sk-live", "Bearer abc", "user:pw", "transcript_path", ".claude/projects"} {
		if strings.Contains(s, bad) {
			t.Errorf("sanitized events leak %q", bad)
		}
	}
}

func TestClaudeRealFixture(t *testing.T) {
	defer security.SetHomeForTest("/Users/dev")()
	evs := normalizeAll(t, event.HarnessClaude, loadFixture(t, "claude-hooks/session-real-2.1.330.jsonl"))
	assertNoLeaks(t, evs)
	count := map[string]int{}
	for _, e := range evs {
		count[e.Type]++
		if e.Workspace != "~/projects/demo" {
			t.Errorf("workspace %q", e.Workspace)
		}
	}
	if count[event.TypeToolStarted] != 6 || count[event.TypeToolCompleted] != 3 || count[event.TypeToolFailed] != 1 {
		t.Errorf("unexpected counts %v", count)
	}
	if count[event.TypeSessionStarted] != 1 || count[event.TypeSessionEnded] != 1 || count[event.TypeTurnStarted] != 1 || count[event.TypeTurnEnded] != 1 {
		t.Errorf("lifecycle counts %v", count)
	}
	var denied, unknown int
	for _, e := range evs {
		if e.Type == event.TypeToolBatchReported {
			switch e.Status {
			case event.StatusDenied:
				denied++
				if e.Evidence != event.EvidenceInferred {
					t.Errorf("hook-block denial must be inferred, got %s", e.Evidence)
				}
			case event.StatusUnknown:
				unknown++
			}
		}
		if e.Type == event.TypeToolStarted && e.Tool.Name == "Read" {
			if len(e.Resources) != 1 || e.Resources[0].Label != "sample.txt" || e.Resources[0].Relation != event.RelReads || e.Resources[0].Evidence != event.EvidenceObserved {
				t.Errorf("read resources %+v", e.Resources)
			}
		}
		if e.Type == event.TypeToolStarted && e.Tool.Name == "Bash" && e.Target != nil && strings.HasPrefix(e.Target.Label, "echo") {
			if e.Target.Label != "echo" {
				t.Errorf("bash label must not include args: %q", e.Target.Label)
			}
		}
		if e.Type == event.TypeTurnStarted && e.TurnID == "" {
			t.Error("turn id (prompt_id) missing")
		}
	}
	if denied != 2 {
		t.Errorf("want 2 hook-blocked denials, got %d", denied)
	}
}

func TestClaudeMCPAndPermissionDenied(t *testing.T) {
	defer security.SetHomeForTest("/Users/dev")()
	evs := normalizeAll(t, event.HarnessClaude, loadFixture(t, "claude-hooks/session-real-mcp-denied-2.1.330.jsonl"))
	assertNoLeaks(t, evs)
	var mcp bool
	bases := map[string]bool{}
	for _, e := range evs {
		if e.Type == event.TypeToolStarted && e.Tool.Category == event.CategoryMCP {
			mcp = true
			if e.Tool.MCPServer != "atlasecho" || e.Tool.MCPTool != "echo" {
				t.Errorf("mcp parse %+v", e.Tool)
			}
			if len(e.Resources) == 0 || e.Resources[0].Kind != event.ResourceMCP || e.Resources[0].Relation != event.RelConnectsTo {
				t.Errorf("mcp resource %+v", e.Resources)
			}
		}
		if e.Type == event.TypeToolBatchReported && e.Status == event.StatusDenied {
			bases[e.Attrs["outcome_basis"]] = true
		}
	}
	if !mcp {
		t.Error("no MCP tool event")
	}
	if !bases["batch_permission_denied_text"] || !bases["batch_hook_block_text"] {
		t.Errorf("denial bases %v", bases)
	}
}

func TestCodexDocumentedFixture(t *testing.T) {
	defer security.SetHomeForTest("/Users/dev")()
	evs := normalizeAll(t, event.HarnessCodex, loadFixture(t, "codex-hooks/session-documented-synthetic.jsonl"))
	assertNoLeaks(t, evs)
	byCall := map[string][]event.Event{}
	for _, e := range evs {
		if e.ToolCallID != "" {
			byCall[e.ToolCallID] = append(byCall[e.ToolCallID], e)
		}
		if e.TurnID == "" && e.Type != event.TypeSessionStarted {
			t.Errorf("%s missing turn id", e.Type)
		}
	}
	if a := byCall["call_A"]; len(a) != 2 || a[1].Type != event.TypeToolFailed || a[1].Evidence != event.EvidenceDerived || a[1].Attrs["exit_code"] != "1" {
		t.Errorf("call_A %+v", a)
	}
	if b := byCall["call_B"]; len(b) != 2 || b[1].Type != event.TypeToolCompleted {
		t.Errorf("call_B %+v", b)
	} else {
		found := false
		for _, r := range b[0].Resources {
			if r.Label == "src/auth.ts" && r.Evidence == event.EvidenceInferred && r.Relation == event.RelReads {
				found = true
			}
		}
		if !found {
			t.Errorf("cat must yield inferred read of src/auth.ts: %+v", b[0].Resources)
		}
	}
	if c := byCall["call_C"]; len(c) != 2 || len(c[0].Resources) != 2 || c[0].Resources[0].Evidence != event.EvidenceObserved {
		t.Errorf("apply_patch %+v", c)
	}
	if e := byCall["call_E"]; len(e) != 1 || e[0].Target.Label != "curl" {
		t.Errorf("secret-laden command label %+v", e)
	}
}

func TestAnalyzeCommand(t *testing.T) {
	cases := []struct {
		cmd, label string
		res        []string
	}{
		{"npm test", "npm test", nil},
		{"go test ./... -run X", "go test", nil},
		{"FOO=bar rtk git status", "git status", nil},
		{"cat a.txt b/c.go | grep x > out.log", "cat …", []string{"reads:a.txt", "reads:b/c.go", "writes:out.log"}},
		{"bash -lc 'sed -n 1,5p main.go'", "sed", []string{"reads:main.go"}},
		{"echo hi 2>/dev/null", "echo", nil},
		{"python3 -c \"print(1)\"", "python3", nil},
	}
	for _, c := range cases {
		label, res := adapters.AnalyzeCommand(c.cmd, "/w")
		if label != c.label {
			t.Errorf("%q label=%q want %q", c.cmd, label, c.label)
		}
		var got []string
		for _, r := range res {
			if r.Evidence != event.EvidenceInferred {
				t.Errorf("command resources must be inferred")
			}
			got = append(got, r.Relation+":"+r.Label)
		}
		if strings.Join(got, ",") != strings.Join(c.res, ",") {
			t.Errorf("%q res=%v want %v", c.cmd, got, c.res)
		}
	}
}

// Regression tests for review findings: arguments must never become file labels.
func TestCommandArgumentsNeverLeakAsFiles(t *testing.T) {
	defer security.SetHomeForTest("/Users/alice")()
	cases := []struct {
		cmd     string
		allowed []string // the only labels that may appear
	}{
		{"cat > notes.md <<'EOF'\nfile contents for customer Jane Doe SSN 123-45-6789\nhead secret.txt\nEOF", []string{"notes.md"}},
		{"cat <<ONE <<-'TWO' > notes.md\nhead internal-a.txt\nONE\nhead internal-b.txt\n\tTWO\ncat public.md", []string{"notes.md", "public.md"}},
		{"cat <<EOF > notes.md\n EOF\nhead internal.txt\nEOF", []string{"notes.md"}},
		{"rg -n \"jane.doe@acme.com\"", nil},
		{"grep -ri hunter2", nil},
		{"cat x.txt | grep -i hunter2", []string{"x.txt"}},
		{"grep -e hunter2 src/a.go", []string{"src/a.go"}},
		{"grep hunter2 src/a.go", []string{"src/a.go"}},
		{"sed -n 's/password=abc/x/'", nil},
		{"head -n 5 f.txt", []string{"f.txt"}},
		{"jq -n --arg email jane.doe@acme.com '{e:$email}'", nil},
		{"cat https://bob:pa55w0rd@example.com/x", nil},
		{"tee https://u:p@h/x", nil},
	}
	for _, c := range cases {
		_, res := adapters.AnalyzeCommand(c.cmd, "/Users/alice/proj")
		var got []string
		for _, r := range res {
			got = append(got, r.Label)
		}
		if strings.Join(got, ",") != strings.Join(c.allowed, ",") {
			t.Errorf("%q: resources %v, want only %v", c.cmd, got, c.allowed)
		}
		b, _ := json.Marshal(res)
		for _, bad := range []string{"Jane", "hunter2", "jane.doe", "pa55w0rd", "password", "123-45"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%q leaked %q in %s", c.cmd, bad, b)
			}
		}
	}
	// case-variant home paths collapse too (macOS volumes are case-insensitive)
	ti := adapters.ClassifyTool("Read", json.RawMessage(`{"file_path":"/Users/ALICE/proj/x.go"}`), "/Users/alice/proj")
	if ti.Target.Label != "x.go" {
		t.Errorf("case-variant path label %q", ti.Target.Label)
	}
	ti = adapters.ClassifyTool("Read", json.RawMessage(`{"file_path":"/Users/ALICE/.ssh/id"}`), "/Users/alice/proj")
	if runtime.GOOS == "darwin" && strings.Contains(strings.ToLower(ti.Target.Label), "alice") {
		t.Errorf("username leaked: %q", ti.Target.Label)
	}
}

func TestSecondReviewInferenceRegressions(t *testing.T) {
	cases := []struct {
		cmd  string
		want []string
	}{
		{"cat << 'EOF' > notes.txt\nhello\nEOF", []string{"writes:notes.txt"}},
		{"cat > notes.txt << EOF\nhello\nEOF", []string{"writes:notes.txt"}},
		{"tee out.txt << EOF\nx\nEOF", []string{"writes:out.txt"}},
		{"sed -i '' 's/oldpassword/newvalue/' config.yml", []string{"reads:config.yml"}},
		{"tr a b <<< hello\ncat secret.txt", []string{"reads:secret.txt"}},
		{"git commit -m \"use << operator\"\ncat secret.txt", []string{"reads:secret.txt"}},
		{"rg --color never foo src/", []string{"reads:src"}},
		{"rg -M 200 customer_jane src/", []string{"reads:src"}},
		{"shasum -a 256 f.bin", []string{"reads:f.bin"}},
	}
	for _, c := range cases {
		_, res := adapters.AnalyzeCommand(c.cmd, "/w")
		var got []string
		for _, r := range res {
			got = append(got, r.Relation+":"+r.Label)
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%q: got %v want %v", c.cmd, got, c.want)
		}
	}
}
