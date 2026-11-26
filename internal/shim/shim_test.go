package shim

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentatlas/internal/config"
	"agentatlas/internal/event"
	"agentatlas/internal/spool"
)

const payload = `{"session_id":"S1","cwd":"/w","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"cat SECRET-FILE.txt"},"tool_use_id":"t1","prompt":"SECRET-PROMPT"}`

func shortHome(t *testing.T) string {
	d, err := os.MkdirTemp("", "ash")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestDaemonDownSpoolsOnlySanitizedEvents(t *testing.T) {
	home := shortHome(t)
	out := Run("claude", strings.NewReader(payload), home)
	if out != Spooled {
		t.Fatalf("outcome %s", out)
	}
	p := config.PathsFor(home)
	entries, _ := os.ReadDir(p.Spool)
	if len(entries) != 1 {
		t.Fatalf("spool entries %d", len(entries))
	}
	b, _ := os.ReadFile(filepath.Join(p.Spool, entries[0].Name()))
	if strings.Contains(string(b), "SECRET-PROMPT") || strings.Contains(string(b), `"raw"`) {
		t.Fatalf("raw payload written to spool: %s", b)
	}
	fi, _ := os.Stat(filepath.Join(p.Spool, entries[0].Name()))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("spool file perms %v", fi.Mode().Perm())
	}
	if di, _ := os.Stat(p.Spool); di.Mode().Perm() != 0o700 {
		t.Errorf("spool dir perms %v", di.Mode().Perm())
	}
	n, drops, err := spool.Drain(p.Spool, func(env *event.Envelope) error {
		if len(env.Events) != 1 || env.Events[0].Type != event.TypeToolStarted {
			t.Errorf("bad spooled env %+v", env)
		}
		return nil
	})
	if err != nil || n != 1 || drops != 0 {
		t.Fatalf("drain %d %d %v", n, drops, err)
	}
}

func TestSpoolFullRecordsDrop(t *testing.T) {
	home := shortHome(t)
	p := config.PathsFor(home)
	os.WriteFile(p.Config, []byte(`{"spool_max_bytes": 10}`), 0o600)
	if out := Run("claude", strings.NewReader(payload), home); out != Dropped {
		t.Fatalf("outcome %s", out)
	}
	_, drops, _ := spool.Drain(p.Spool, func(*event.Envelope) error { return nil })
	if drops != 1 {
		t.Fatalf("drops %d", drops)
	}
}

func TestSendsToDaemonAndHonorsReplies(t *testing.T) {
	home := shortHome(t)
	p := config.PathsFor(home)
	ln, err := net.Listen("unix", p.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := os.Chmod(p.Socket, 0o600); err != nil {
		t.Fatal(err)
	}
	replies := []string{"ok", "paused", "rejected", "busy"}
	got := make(chan event.Envelope, 4)
	go func() {
		for _, r := range replies {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadBytes('\n')
			var env event.Envelope
			json.Unmarshal(line, &env)
			got <- env
			c.Write([]byte(r + "\n"))
			c.Close()
		}
	}()
	want := []Outcome{Sent, Paused, Rejected, Spooled}
	for i, w := range want {
		if out := Run("claude", strings.NewReader(payload), home); out != w {
			t.Errorf("reply %q: outcome %s want %s", replies[i], out, w)
		}
	}
	env := <-got
	if env.Harness != "claude" || env.EnvelopeID == "" || env.ReceivedAt.IsZero() || len(env.Raw) == 0 {
		t.Errorf("bad envelope %+v", env)
	}
}

func TestPausedMarkerAndBadInput(t *testing.T) {
	home := shortHome(t)
	os.WriteFile(filepath.Join(home, PausedFile), nil, 0o600)
	if out := Run("claude", strings.NewReader(payload), home); out != Paused {
		t.Fatalf("outcome %s", out)
	}
	os.Remove(filepath.Join(home, PausedFile))
	if out := Run("claude", strings.NewReader("not json"), home); out != Rejected {
		t.Fatalf("outcome %s", out)
	}
	if out := Run("nope", strings.NewReader(payload), home); out != BadHarness {
		t.Fatalf("outcome %s", out)
	}
	if out := Run("claude", strings.NewReader(strings.Repeat("x", event.MaxPayloadBytes+10)), home); out != Dropped {
		t.Fatalf("oversize outcome %s", out)
	}
	if entries, _ := os.ReadDir(config.PathsFor(home).Spool); len(entries) != 1 || entries[0].Name() != "drops.count" {
		t.Fatalf("expected only the drop counter, got %v", entries)
	}
}

type slowReader struct{}

func (slowReader) Read([]byte) (int, error) { select {} }

func TestStdinTimeoutIsCountedAsDrop(t *testing.T) {
	home := shortHome(t)
	old := ReadTimeout
	ReadTimeout = 50 * 1e6
	defer func() { ReadTimeout = old }()
	if out := Run("claude", slowReader{}, home); out != Dropped {
		t.Fatalf("outcome %s", out)
	}
	_, drops, _ := spool.Drain(config.PathsFor(home).Spool, func(*event.Envelope) error { return nil })
	if drops != 1 {
		t.Fatalf("timeout not counted: %d", drops)
	}
}

func TestManyDropsUseOneFile(t *testing.T) {
	dir := shortHome(t)
	for i := 0; i < 500; i++ {
		spool.RecordDrop(dir, "x")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("drop accounting created %d entries", len(entries))
	}
	_, drops, _ := spool.Drain(dir, func(*event.Envelope) error { return nil })
	if drops != 500 {
		t.Fatalf("drops %d", drops)
	}
}
