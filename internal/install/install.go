// Package install merges Atlas hook entries into Claude Code and Codex hook
// configuration and removes them again (design §4.3). It backs files up first,
// never replaces unrelated hooks, is idempotent, and on uninstall restores the
// exact original bytes when the file was not edited since installation.
package install

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"agentatlas/internal/adapters"
	"agentatlas/internal/event"
	"agentatlas/internal/normalize"
)

// HookTimeoutSecs is the per-hook timeout written to harness config.
const HookTimeoutSecs = 5

// Target describes one harness config file.
type Target struct {
	Harness string // claude | codex
	Path    string // settings.json (Claude) or hooks.json (Codex)
}

// DefaultTargets returns the user-level config files.
func DefaultTargets() []Target {
	home, _ := os.UserHomeDir()
	claudeHome := os.Getenv("CLAUDE_CONFIG_DIR")
	if claudeHome == "" {
		claudeHome = filepath.Join(home, ".claude")
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	return []Target{
		{Harness: event.HarnessClaude, Path: filepath.Join(claudeHome, "settings.json")},
		{Harness: event.HarnessCodex, Path: filepath.Join(codexHome, "hooks.json")},
	}
}

// Command returns the hook command line for a harness.
func Command(atlasBin, harness string) string {
	return shellQuote(atlasBin) + " hook --harness " + harness
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " '\"\\$`!&|;<>()*?[]{}~#\t\n") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func ownedCommand(command, current, recorded string) bool {
	return command != "" && (command == current || command == recorded)
}

// realPath resolves symlinks so writes go to the link target and the link
// itself (e.g. into a dotfiles repo) is preserved.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// State records installs so uninstall can restore precisely.
type State struct {
	Installs map[string]Record `json:"installs"` // keyed by config path
}

// Record is one install.
type Record struct {
	Harness       string   `json:"harness"`
	Path          string   `json:"path"`
	Backup        string   `json:"backup,omitempty"`
	BackupClean   bool     `json:"backup_clean,omitempty"`
	CreatedFile   bool     `json:"created_file"`
	CreatedHooks  bool     `json:"created_hooks_key"`
	CreatedEvents []string `json:"created_events"`
	WrittenSHA256 string   `json:"written_sha256"`
	InstalledAt   string   `json:"installed_at"`
	Command       string   `json:"command"`
}

func loadState(p string) (State, error) {
	var st State
	b, err := os.ReadFile(p)
	if err == nil {
		err = json.Unmarshal(b, &st)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return st, fmt.Errorf("read install state: %w", err)
	}
	if st.Installs == nil {
		st.Installs = map[string]Record{}
	}
	return st, nil
}

func saveState(p string, st State) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	return atomicWrite(p, append(b, '\n'), 0o600)
}

func lockState(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// Result reports what an operation did.
type Result struct {
	Harness string   `json:"harness"`
	Path    string   `json:"path"`
	Action  string   `json:"action"` // installed | already_installed | restored_backup | removed_entries | not_installed | deleted_created_file
	Backup  string   `json:"backup,omitempty"`
	Events  []string `json:"events,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

func capsFor(h string) (adapters.Capabilities, error) {
	a, ok := normalize.Adapter(h)
	if !ok {
		return adapters.Capabilities{}, fmt.Errorf("unknown harness %q", h)
	}
	return a.Capabilities(), nil
}

// Install merges Atlas hooks into t.Path. statePath is the Atlas install-state file.
func Install(t Target, atlasBin, statePath string, now time.Time) (Result, error) {
	res := Result{Harness: t.Harness, Path: t.Path}
	caps, err := capsFor(t.Harness)
	if err != nil {
		return res, err
	}
	unlock, err := lockState(statePath)
	if err != nil {
		return res, err
	}
	defer unlock()
	st, err := loadState(statePath)
	if err != nil {
		return res, err
	}
	prev, had := st.Installs[t.Path]
	target := realPath(t.Path)
	orig, err := os.ReadFile(target)
	created := false
	var root *Value
	mode := fs.FileMode(0o600)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		created = true
		root = ObjVal(NewObject())
	case err != nil:
		return res, err
	default:
		if fi, e := os.Stat(target); e == nil {
			mode = fi.Mode().Perm()
		}
		if len(strings.TrimSpace(string(orig))) == 0 {
			root = ObjVal(NewObject())
		} else if root, err = Parse(orig); err != nil {
			return res, fmt.Errorf("%s is not valid JSON; not modifying it: %w", t.Path, err)
		}
	}
	if !root.IsObj() {
		return res, fmt.Errorf("%s: top-level JSON is not an object; not modifying it", t.Path)
	}
	cmd := Command(atlasBin, t.Harness)
	isOwned := func(s string) bool { return ownedCommand(s, cmd, prev.Command) }
	hadOwnedHooks := len(hookEvents(root, isOwned)) > 0
	hooksV, ok := root.Obj.Get("hooks")
	createdHooks := false
	if !ok {
		hooksV = ObjVal(NewObject())
		root.Obj.Set("hooks", hooksV)
		createdHooks = true
	} else if !hooksV.IsObj() {
		return res, fmt.Errorf("%s: \"hooks\" is not an object; not modifying it", t.Path)
	}
	matcherEvents := map[string]bool{}
	for _, e := range caps.ToolMatcherEvents {
		matcherEvents[e] = true
	}
	var added, createdEvents, updated []string
	for _, ev := range caps.HookEvents {
		arrV, ok := hooksV.Obj.Get(ev)
		if !ok {
			arrV = ArrVal([]*Value{})
			hooksV.Obj.Set(ev, arrV)
			createdEvents = append(createdEvents, ev)
		} else if !arrV.IsArr() {
			return res, fmt.Errorf("%s: hooks.%s is not an array; not modifying it", t.Path, ev)
		}
		if hasAtlas(arrV, isOwned) {
			// Repair a stale command (e.g. binary moved after an upgrade).
			if rewriteAtlas(arrV, isOwned, cmd) {
				updated = append(updated, ev)
			}
			continue
		}
		hook := NewObject()
		hook.Set("type", StrVal("command"))
		hook.Set("command", StrVal(cmd))
		timeout := HookTimeoutSecs
		if t.Harness == event.HarnessCodex && (ev == "SessionEnd" || ev == "Interrupt") {
			timeout = 3
		}
		hook.Set("timeout", NumVal(timeout))
		group := NewObject()
		if matcherEvents[ev] && t.Harness == event.HarnessClaude {
			group.Set("matcher", StrVal("*"))
		}
		group.Set("hooks", ArrVal([]*Value{ObjVal(hook)}))
		arrV.Arr = append(arrV.Arr, ObjVal(group))
		added = append(added, ev)
	}
	if len(added) == 0 && len(updated) == 0 {
		res.Action = "already_installed"
		return res, nil
	}
	out := Marshal(root)
	backup := ""
	if !created {
		backup, err = writeBackup(t.Path, orig, now)
		if err != nil {
			return res, fmt.Errorf("backup: %w", err)
		}
	}
	if err := atomicWrite(target, out, mode); err != nil {
		return res, err
	}
	rec := Record{Harness: t.Harness, Path: t.Path, Backup: backup, BackupClean: !hadOwnedHooks, CreatedFile: created, CreatedHooks: createdHooks,
		CreatedEvents: createdEvents, WrittenSHA256: sum(out), InstalledAt: now.UTC().Format(time.RFC3339), Command: cmd}
	if hadOwnedHooks {
		// Keep the backup for inspection, but restoring it would restore hooks too.
		rec.Backup = ""
	}
	// Keep the older install's facts only if the file is still exactly what
	// Atlas last wrote; otherwise the user changed it and this install's backup
	// (and creation facts) are the ones that must be restored.
	if had && !created && sum(orig) == prev.WrittenSHA256 {
		rec.Backup, rec.BackupClean, rec.CreatedFile = prev.Backup, prev.BackupClean, prev.CreatedFile
	}
	if had && hadOwnedHooks {
		rec.CreatedHooks = prev.CreatedHooks || createdHooks
		rec.CreatedEvents = append(prev.CreatedEvents, createdEvents...)
	}
	st.Installs[t.Path] = rec
	if err := saveState(statePath, st); err != nil {
		if created {
			return res, errors.Join(err, os.Remove(target))
		}
		return res, errors.Join(err, atomicWrite(target, orig, mode))
	}
	res.Action, res.Backup, res.Events = "installed", backup, added
	if len(added) == 0 {
		res.Action, res.Events = "updated_command", updated
	}
	if t.Harness == event.HarnessCodex {
		res.Notes = append(res.Notes, "Codex requires new hooks to be reviewed/trusted before they run: open Codex and approve the Atlas hooks when prompted.")
	}
	return res, nil
}

// rewriteAtlas replaces Atlas hook commands that differ from want.
func rewriteAtlas(arr *Value, isOwned func(string) bool, want string) bool {
	changed := false
	for _, g := range arr.Arr {
		if !g.IsObj() {
			continue
		}
		hs, ok := g.Obj.Get("hooks")
		if !ok || !hs.IsArr() {
			continue
		}
		for _, h := range hs.Arr {
			if !h.IsObj() {
				continue
			}
			if c, ok := h.Obj.Get("command"); ok && isOwned(c.Str()) && c.Str() != want {
				h.Obj.Set("command", StrVal(want))
				changed = true
			}
		}
	}
	return changed
}

func hasAtlas(arr *Value, isOwned func(string) bool) bool {
	for _, g := range arr.Arr {
		if !g.IsObj() {
			continue
		}
		hs, ok := g.Obj.Get("hooks")
		if !ok || !hs.IsArr() {
			continue
		}
		for _, h := range hs.Arr {
			if h.IsObj() {
				if c, ok := h.Obj.Get("command"); ok && isOwned(c.Str()) {
					return true
				}
			}
		}
	}
	return false
}

// Installed reports which hook events currently contain an Atlas entry.
func Installed(t Target, atlasBin, statePath string) ([]string, error) {
	st, err := loadState(statePath)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(realPath(t.Path))
	if err != nil {
		return nil, err
	}
	root, err := Parse(b)
	if err != nil || !root.IsObj() {
		return nil, fmt.Errorf("%s: unreadable", t.Path)
	}
	current, recorded := Command(atlasBin, t.Harness), st.Installs[t.Path].Command
	return hookEvents(root, func(s string) bool { return ownedCommand(s, current, recorded) }), nil
}

func hookEvents(root *Value, isOwned func(string) bool) []string {
	hooksV, ok := root.Obj.Get("hooks")
	if !ok || !hooksV.IsObj() {
		return nil
	}
	var out []string
	for _, k := range hooksV.Obj.Keys {
		if v := hooksV.Obj.Vals[k]; v.IsArr() && hasAtlas(v, isOwned) {
			out = append(out, k)
		}
	}
	return out
}

// Uninstall removes Atlas hooks. If the file is byte-identical to what Atlas
// wrote, the backup is restored verbatim; otherwise only Atlas entries (and
// containers Atlas created that are now empty) are removed.
func Uninstall(t Target, atlasBin, statePath string) (Result, error) {
	res := Result{Harness: t.Harness, Path: t.Path}
	unlock, err := lockState(statePath)
	if err != nil {
		return res, err
	}
	defer unlock()
	st, err := loadState(statePath)
	if err != nil {
		return res, err
	}
	rec, had := st.Installs[t.Path]
	current := Command(atlasBin, t.Harness)
	target := realPath(t.Path)
	cur, err := os.ReadFile(target)
	if errors.Is(err, fs.ErrNotExist) {
		res.Action = "not_installed"
		return res, forgetInstall(statePath, st, t.Path)
	}
	if err != nil {
		return res, err
	}
	if had && sum(cur) == rec.WrittenSHA256 {
		var orig []byte
		var berr error
		if !rec.CreatedFile && rec.Backup != "" {
			orig, berr = os.ReadFile(rec.Backup)
		}
		switch {
		case rec.CreatedFile:
			if err := os.Remove(target); err != nil {
				return res, err
			}
			res.Action = "deleted_created_file"
			return res, forgetInstall(statePath, st, t.Path)
		case rec.Backup != "" && berr == nil && (rec.BackupClean || legacyBackupClean(orig, t.Harness)):
			mode := fs.FileMode(0o600)
			if fi, e := os.Stat(target); e == nil {
				mode = fi.Mode().Perm()
			}
			if err := atomicWrite(target, orig, mode); err != nil {
				return res, err
			}
			res.Action, res.Backup = "restored_backup", rec.Backup
			return res, forgetInstall(statePath, st, t.Path)
		}
		// A missing or unsafe backup cannot be restored verbatim.
		res.Notes = append(res.Notes, "no hook-free backup available; removing Atlas entries instead")
	}
	root, err := Parse(cur)
	if err != nil || !root.IsObj() {
		return res, fmt.Errorf("%s is not valid JSON; not modifying it", t.Path)
	}
	hooksV, ok := root.Obj.Get("hooks")
	if !ok || !hooksV.IsObj() {
		res.Action = "not_installed"
		return res, forgetInstall(statePath, st, t.Path)
	}
	createdEv := map[string]bool{}
	for _, e := range rec.CreatedEvents {
		createdEv[e] = true
	}
	removed := 0
	for _, ev := range append([]string(nil), hooksV.Obj.Keys...) {
		arr := hooksV.Obj.Vals[ev]
		if !arr.IsArr() {
			continue
		}
		var keep []*Value
		for _, g := range arr.Arr {
			if g.IsObj() {
				if hs, ok := g.Obj.Get("hooks"); ok && hs.IsArr() {
					var kh []*Value
					for _, h := range hs.Arr {
						if h.IsObj() {
							if c, ok := h.Obj.Get("command"); ok && ownedCommand(c.Str(), current, rec.Command) {
								removed++
								continue
							}
						}
						kh = append(kh, h)
					}
					if len(kh) == 0 && len(hs.Arr) > 0 {
						continue // group held only Atlas hooks
					}
					hs.Arr = kh
				}
			}
			keep = append(keep, g)
		}
		if keep == nil {
			keep = []*Value{}
		}
		arr.Arr = keep
		if len(keep) == 0 && createdEv[ev] {
			hooksV.Obj.Delete(ev)
		}
	}
	if len(hooksV.Obj.Keys) == 0 && rec.CreatedHooks {
		root.Obj.Delete("hooks")
	}
	if removed == 0 {
		res.Action = "not_installed"
		return res, forgetInstall(statePath, st, t.Path)
	}
	backup, err := writeBackup(t.Path, cur, time.Now())
	if err != nil {
		return res, err
	}
	mode := fs.FileMode(0o600)
	if fi, e := os.Stat(target); e == nil {
		mode = fi.Mode().Perm()
	}
	if err := atomicWrite(target, Marshal(root), mode); err != nil {
		return res, err
	}
	res.Action, res.Backup = "removed_entries", backup
	return res, forgetInstall(statePath, st, t.Path)
}

func forgetInstall(statePath string, st State, target string) error {
	if _, ok := st.Installs[target]; !ok {
		return nil
	}
	delete(st.Installs, target)
	return saveState(statePath, st)
}

func legacyBackupClean(data []byte, harness string) bool {
	if len(strings.TrimSpace(string(data))) == 0 {
		return true
	}
	root, err := Parse(data)
	return err == nil && root.IsObj() && len(hookEvents(root, func(s string) bool {
		return strings.HasSuffix(strings.TrimSpace(s), " hook --harness "+harness)
	})) == 0
}

func writeBackup(path string, data []byte, now time.Time) (string, error) {
	pattern := filepath.Base(path) + ".atlas-backup-" + now.UTC().Format("20060102T150405Z") + "-*"
	return writeTemp(filepath.Dir(path), pattern, data, 0o600)
}

func atomicWrite(path string, b []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := writeTemp(filepath.Dir(path), ".atlas-tmp-*", b, mode)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, path)
}

func writeTemp(dir, pattern string, data []byte, mode fs.FileMode) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Chmod(mode), f.Close()); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
