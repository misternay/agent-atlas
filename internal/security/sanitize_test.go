package security

import (
	"strings"
	"testing"
	"time"

	"agentatlas/internal/event"
)

func TestRedactSecrets(t *testing.T) {
	cases := map[string]string{
		"curl -H 'Authorization: Bearer abcdefghijklmnop' x":                                               "abcdefghijklmnop",
		"export OPENAI_API_KEY=sk-proj-abcdefghijklmnopqrstu":                                              "sk-proj-abcdefghijklmnopqrstu",
		"git clone https://user:hunter2@github.com/x/y":                                                    "hunter2",
		"psql --password=s3cr3t-value db":                                                                  "s3cr3t-value",
		"token ghp_abcdefghijklmnopqrstuvwxyz0123456789":                                                   "ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"aws AKIAABCDEFGHIJKLMNOP":                                                                         "AKIAABCDEFGHIJKLMNOP",
		"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U": "eyJhbGci",
		"blob QWxhZGRpbjpvcGVuIHNlc2FtZQ0123456789abcdefghijkl":                                            "QWxhZGRpbjpvcGVuIHNlc2FtZQ0123456789abcdefghijkl",
	}
	for in, secret := range cases {
		out := RedactSecrets(in)
		if strings.Contains(out, secret) || !strings.Contains(out, "[REDACTED]") {
			t.Errorf("%q -> %q still leaks %q", in, out, secret)
		}
	}
	keep := "src/components/very/deep/path/to/SomeComponentFileName.tsx"
	if RedactSecrets(keep) != keep {
		t.Errorf("paths must not be redacted: %q", RedactSecrets(keep))
	}
}

func TestPathsAndBounds(t *testing.T) {
	defer SetHomeForTest("/Users/alice")()
	if got := FileLabel("/Users/alice/proj/src/a.go", "/Users/alice/proj"); got != "src/a.go" {
		t.Errorf("relative: %q", got)
	}
	if got := FileLabel("/Users/alice/.ssh/config", "/Users/alice/proj"); got != "~/.ssh/config" {
		t.Errorf("home collapse: %q", got)
	}
	if got := Workspace("/Users/alice/proj"); got != "~/proj" {
		t.Errorf("workspace: %q", got)
	}
	if got := CollapseHome("x -Users-alice-proj y"); strings.Contains(got, "alice") {
		t.Errorf("encoded home path leaked: %q", got)
	}
	if got := URLHost("https://user:pw@API.example.com:8443/v1?q=secret"); got != "api.example.com" {
		t.Errorf("url host: %q", got)
	}
	long := strings.Repeat("a", 500)
	if l := len([]rune(CleanLabel(long))); l > MaxLabelLen {
		t.Errorf("label not bounded: %d", l)
	}
	if id := CleanID(strings.Repeat("x", 300)); len(id) > MaxIDLen {
		t.Errorf("id not bounded: %d", len(id))
	}
}

func TestSanitizeEventAllowlist(t *testing.T) {
	defer SetHomeForTest("/Users/alice")()
	e := &event.Event{
		SchemaVersion: 1, EventID: "e1", Timestamp: time.Now(), Harness: "claude", SessionID: "s", Type: event.TypeToolStarted,
		Evidence: "made-up", Privacy: "raw",
		Workspace: "/Users/alice/proj",
		Target:    &event.Target{Kind: "process", Label: "deploy --token abc123secret"},
		Attrs:     map[string]string{"model": "m", "prompt": "SECRET PROMPT", "env": "PATH=/x"},
		Resources: []event.Resource{{Kind: "file", Label: "/Users/alice/x", Relation: "reads", Evidence: "bogus"}, {Kind: "file", Label: "/Users/alice/x", Relation: "reads", Evidence: "bogus"}},
	}
	SanitizeEvent(e)
	if e.Privacy != event.PrivacyMetadataOnly || e.Evidence != event.EvidenceUnknown {
		t.Errorf("privacy/evidence %q %q", e.Privacy, e.Evidence)
	}
	if _, ok := e.Attrs["prompt"]; ok {
		t.Error("non-allowlisted attr kept")
	}
	if _, ok := e.Attrs["env"]; ok {
		t.Error("non-allowlisted attr kept")
	}
	if strings.Contains(e.Target.Label, "abc123secret") {
		t.Errorf("secret in target: %q", e.Target.Label)
	}
	if e.Workspace != "~/proj" || len(e.Resources) != 1 || e.Resources[0].Label != "~/x" || e.Resources[0].Evidence != event.EvidenceUnknown {
		t.Errorf("sanitized %+v", e)
	}
}

func TestMoreSecretFormats(t *testing.T) {
	for _, s := range []string{"hf_abcdefghijklmnopqrstuvwxyzABCD12", "ASIAIOSFODNN7EXAMPLE", "sk_live_51Habcdefghijklmnop", "ya29.a0AfH6abcdefghijk", "SG.abcdefghijklmnopqr.abcdefghijklmnopqrst", "AccountKey=abc123def456==", "npm_abcdefghijklmnopqrstuvwxyz0123456789"} {
		if out := RedactSecrets("x " + s + " y"); strings.Contains(out, s) {
			t.Errorf("not redacted: %q -> %q", s, out)
		}
	}
	out := RedactSecrets("curl -H 'Authorization: token ghp_x1234567' u")
	if strings.Contains(out, "ghp_x1234567") {
		t.Errorf("authorization credential kept: %q", out)
	}
	if got := FileLabel("https:/bob:pa55w0rd@example.com/x", "/w"); strings.Contains(got, "pa55w0rd") {
		t.Errorf("cleaned URL credentials leaked: %q", got)
	}
}

func TestHomeCollapseBoundariesAndKeywordWorkspaces(t *testing.T) {
	defer SetHomeForTest("/home/dev")()
	for in, want := range map[string]string{
		"/home/developer/project/main.go": "/home/developer/project/main.go",
		"/home/devops/app":                "/home/devops/app",
		"/Volumes/Backup/home/dev/x":      "/Volumes/Backup/home/dev/x",
		"-home-devices-list":              "-home-devices-list",
		"my-home-dev-notes.md":            "my-home-dev-notes.md",
		"/home/dev/x.go":                  "~/x.go",
		"cd /home/dev && ls":              "cd ~ && ls",
		"p/-home-dev-proj/1.jsonl":        "p/~-proj/1.jsonl",
	} {
		if got := CollapseHome(in); got != want {
			t.Errorf("CollapseHome(%q) = %q, want %q", in, got, want)
		}
	}
	if got := FileLabel("/home/dev/Secret Santa/src/main.go", "/home/dev/Secret Santa"); got != "src/main.go" {
		t.Errorf("keyword workspace: %q", got)
	}
}

func TestCollapseHomeConcurrent(t *testing.T) {
	defer SetHomeForTest("/home/dev")()
	done := make(chan bool)
	for i := 0; i < 8; i++ {
		go func() { CollapseHome("/home/dev/x"); done <- true }()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
