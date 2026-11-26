package install

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const existingClaude = `{
  "env": {
    "FOO": "bar"
  },
  "permissions": {
    "allow": ["Bash(ls:*)"]
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "if [ -n \"$X\" ] && [ -f a ]; then echo hi > /dev/null; fi"
          }
        ]
      }
    ],
    "Notification": [
      {
        "hooks": [{ "type": "command", "command": "/usr/local/bin/notify" }]
      }
    ]
  },
  "theme": "dark"
}
`

func TestInstallUninstallRestoresExactBytes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(p, []byte(existingClaude), 0o644); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state.json")
	tgt := Target{Harness: "claude", Path: p}
	res, err := Install(tgt, "/opt/atlas/bin/atlas", state, time.Unix(0, 0))
	if err != nil || res.Action != "installed" {
		t.Fatalf("install: %v %+v", err, res)
	}
	after, _ := os.ReadFile(p)
	s := string(after)
	for _, want := range []string{`"FOO": "bar"`, `echo hi > /dev/null`, `/usr/local/bin/notify`, `"theme": "dark"`, `/opt/atlas/bin/atlas hook --harness claude`} {
		if !strings.Contains(s, want) {
			t.Errorf("installed file missing %q", want)
		}
	}
	if strings.Index(s, `"env"`) > strings.Index(s, `"theme"`) {
		t.Error("key order not preserved")
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode changed to %v", fi.Mode().Perm())
	}
	if res.Backup == "" {
		t.Fatal("no backup")
	}
	if b, _ := os.ReadFile(res.Backup); string(b) != existingClaude {
		t.Error("backup differs from original")
	}
	// idempotent
	res2, err := Install(tgt, "/opt/atlas/bin/atlas", state, time.Unix(1, 0))
	if err != nil || res2.Action != "already_installed" {
		t.Fatalf("second install: %v %+v", err, res2)
	}
	evs, _ := Installed(tgt, "/opt/atlas/bin/atlas", state)
	if len(evs) < 10 {
		t.Errorf("installed events %v", evs)
	}
	un, err := Uninstall(tgt, "atlas", state)
	if err != nil || un.Action != "restored_backup" {
		t.Fatalf("uninstall: %v %+v", err, un)
	}
	final, _ := os.ReadFile(p)
	if !bytes.Equal(final, []byte(existingClaude)) {
		t.Fatalf("uninstall did not restore exact bytes:\n%s", final)
	}
}

func TestUninstallAfterUserEditRemovesOnlyAtlasEntries(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hooks.json")
	os.WriteFile(p, []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"keep-me"}]}]}}`), 0o600)
	state := filepath.Join(dir, "state.json")
	tgt := Target{Harness: "codex", Path: p}
	if _, err := Install(tgt, "/x/atlas", state, time.Now()); err != nil {
		t.Fatal(err)
	}
	// user edits the file after install (adds a new key)
	b, _ := os.ReadFile(p)
	edited := strings.Replace(string(b), `"hooks": {`, `"extra": true, "hooks": {`, 1)
	os.WriteFile(p, []byte(edited), 0o600)
	res, err := Uninstall(tgt, "atlas", state)
	if err != nil || res.Action != "removed_entries" {
		t.Fatalf("uninstall: %v %+v", err, res)
	}
	out, _ := os.ReadFile(p)
	s := string(out)
	if strings.Contains(s, "atlas hook") {
		t.Error("atlas entries remain")
	}
	if !strings.Contains(s, "keep-me") || !strings.Contains(s, `"extra": true`) {
		t.Errorf("user content lost:\n%s", s)
	}
	if strings.Contains(s, "PreToolUse") {
		t.Error("event arrays created by atlas should be removed when empty")
	}
}

func TestInstallCreatesAndUninstallDeletesNewFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "settings.json")
	state := filepath.Join(dir, "state.json")
	tgt := Target{Harness: "claude", Path: p}
	if res, err := Install(tgt, "/a b/atlas", state, time.Now()); err != nil || res.Action != "installed" {
		t.Fatalf("%v %+v", err, res)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `'/a b/atlas' hook --harness claude`) {
		t.Errorf("binary path with space must be shell-quoted:\n%s", b)
	}
	if res, err := Uninstall(tgt, "atlas", state); err != nil || res.Action != "deleted_created_file" {
		t.Fatalf("%v %+v", err, res)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("created file should be removed")
	}
}

func TestInstallRefusesInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	os.WriteFile(p, []byte(`{ not json`), 0o600)
	if _, err := Install(Target{Harness: "claude", Path: p}, "/x/atlas", filepath.Join(dir, "s.json"), time.Now()); err == nil {
		t.Fatal("expected error")
	}
	if b, _ := os.ReadFile(p); string(b) != `{ not json` {
		t.Error("invalid file must not be modified")
	}
}

func TestPlist(t *testing.T) {
	dir := t.TempDir()
	p, err := LaunchdInstall(dir, "/opt/atlas/atlasd", "/Users/x/Library/Application Support/AgentAtlas", "/tmp/a&b.log", false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "<string>"+LaunchdLabel+"</string>") || !strings.Contains(string(b), "a&amp;b.log") {
		t.Errorf("plist:\n%s", b)
	}
	if _, err := LaunchdUninstall(dir, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("plist not removed")
	}
}

// Regression tests for review findings.
func TestReinstallAfterUserEditsUsesNewFacts(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hooks.json")
	state := filepath.Join(dir, "state.json")
	tgt := Target{Harness: "codex", Path: p}
	// 1) first install creates the file
	if _, err := Install(tgt, "/x/atlas", state, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	// 2) user removes Atlas entries and writes own content
	own := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"mine"}]}]},"x":1}` + "\n"
	os.WriteFile(p, []byte(own), 0o600)
	// 3) reinstall, 4) uninstall: must restore the user's file, never delete it
	if _, err := Install(tgt, "/x/atlas", state, time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	res, err := Uninstall(tgt, "atlas", state)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != own {
		t.Fatalf("user content lost (%s): %q %v", res.Action, b, err)
	}
}

func TestReinstallKeepsEditsBetweenInstalls(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	state := filepath.Join(dir, "state.json")
	os.WriteFile(p, []byte(`{"theme":"dark"}`), 0o600)
	tgt := Target{Harness: "claude", Path: p}
	Install(tgt, "/x/atlas", state, time.Unix(1, 0))
	edited := `{"theme":"light","model":"opus"}`
	os.WriteFile(p, []byte(edited), 0o600)
	Install(tgt, "/x/atlas", state, time.Unix(2, 0))
	Uninstall(tgt, "atlas", state)
	if b, _ := os.ReadFile(p); string(b) != edited {
		t.Fatalf("edits between installs lost: %s", b)
	}
}

func TestSymlinkPreserved(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "settings.json")
	os.MkdirAll(filepath.Dir(real), 0o700)
	os.WriteFile(real, []byte(`{"a":1}`), 0o600)
	link := filepath.Join(dir, "settings.json")
	os.Symlink(real, link)
	state := filepath.Join(dir, "state.json")
	tgt := Target{Harness: "claude", Path: link}
	Install(tgt, "/x/atlas", state, time.Now())
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("install replaced the symlink")
	}
	if b, _ := os.ReadFile(real); !strings.Contains(string(b), "hook --harness claude") {
		t.Fatal("link target not updated")
	}
	Uninstall(tgt, "atlas", state)
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("uninstall replaced the symlink")
	}
	if b, _ := os.ReadFile(real); string(b) != `{"a":1}` {
		t.Fatalf("target not restored: %s", b)
	}
}

func TestCommandRepairRenamedBinaryAndMissingBackup(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	state := filepath.Join(dir, "state.json")
	os.WriteFile(p, []byte(`{}`), 0o600)
	tgt := Target{Harness: "claude", Path: p}
	r1, _ := Install(tgt, "/usr/local/bin/aa", state, time.Unix(1, 0)) // binary without "atlas" in its name
	if r2, _ := Install(tgt, "/usr/local/bin/aa", state, time.Unix(2, 0)); r2.Action != "already_installed" {
		t.Fatalf("not idempotent for renamed binary: %s", r2.Action)
	}
	r3, err := Install(tgt, "/opt/new/atlas", state, time.Unix(3, 0))
	if err != nil || r3.Action != "updated_command" {
		t.Fatalf("stale command not repaired: %v %+v", err, r3)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "/usr/local/bin/aa") || strings.Count(string(b), "hook --harness claude") != 13 {
		t.Fatalf("after repair:\n%s", b)
	}
	os.Remove(r1.Backup)
	os.Remove(r3.Backup)
	res, err := Uninstall(tgt, "atlas", state)
	if err != nil {
		t.Fatalf("uninstall with missing backup: %v", err)
	}
	if b, _ := os.ReadFile(p); strings.Contains(string(b), "hook --harness") {
		t.Fatalf("atlas entries remain (%s): %s", res.Action, b)
	}
}

func TestReinstallSameSecondPreservesForeignHookAndUserEdits(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	state := filepath.Join(dir, "state.json")
	tgt := Target{Harness: "claude", Path: p}
	stamp := time.Unix(1, 0)
	foreign := "/opt/other/observer hook --harness claude"
	original := `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"` + foreign + `"}]}]}}`
	if err := os.WriteFile(p, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := Install(tgt, "/opt/old/atlas", state, stamp)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(installed), `"hooks": {`, `"user_edit": true, "hooks": {`, 1)
	if err := os.WriteFile(p, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := Install(tgt, "/opt/new/atlas", state, stamp)
	if err != nil || second.Backup == first.Backup {
		t.Fatalf("same-second reinstall must create a fresh backup: %+v %v", second, err)
	}
	if b, err := os.ReadFile(first.Backup); err != nil || string(b) != original {
		t.Fatalf("original backup was changed: %q %v", b, err)
	}
	if _, err := Uninstall(tgt, "/opt/new/atlas", state); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(p)
	if err != nil || !strings.Contains(string(out), foreign) || !strings.Contains(string(out), `"user_edit": true`) || strings.Contains(string(out), "/opt/new/atlas") || strings.Contains(string(out), "/opt/old/atlas") {
		t.Fatalf("uninstall lost user settings or restored Atlas entries: %s %v", out, err)
	}
}
