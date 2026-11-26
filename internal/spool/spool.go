// Package spool is the bounded offline buffer used by the hook shim when the
// daemon is unreachable. It only ever holds envelopes whose Events are already
// sanitized (Raw must be empty). Each envelope is one file, written atomically,
// so concurrent shims never interleave.
package spool

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentatlas/internal/event"
)

// ErrFull is returned when the spool is at capacity.
var ErrFull = errors.New("spool full")

// ErrRaw is returned if a caller tries to spool raw payload bytes.
var ErrRaw = errors.New("refusing to spool raw payload")

func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// Size returns the total bytes of spooled envelopes.
func Size(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var n int64
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			if fi, err := e.Info(); err == nil {
				n += fi.Size()
			}
		}
	}
	return n
}

// Append writes one sanitized envelope. It fails with ErrFull when adding it
// would exceed maxBytes; the caller should then RecordDrop.
func Append(dir string, env *event.Envelope, maxBytes int64) error {
	if len(env.Raw) > 0 {
		return ErrRaw
	}
	if err := ensureDir(dir); err != nil {
		return err
	}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	if Size(dir)+int64(len(b)) > maxBytes {
		return ErrFull
	}
	name := filepath.Join(dir, safeName(env.EnvelopeID)+".json")
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}

// dropCounter is a single file grown by one byte per dropped event. O_APPEND
// writes of one byte are atomic, so concurrent shims never lose counts, and the
// directory never grows with one entry per drop.
const dropCounter = "drops.count"

// maxDropCounter bounds the counter file (beyond this, drops are still lost but
// no longer counted individually; the gap is reported as at least this many).
const maxDropCounter = 1 << 20

// RecordDrop counts one dropped event so the daemon can report a telemetry gap.
func RecordDrop(dir, _ string) error {
	if err := ensureDir(dir); err != nil {
		return err
	}
	p := filepath.Join(dir, dropCounter)
	if fi, err := os.Stat(p); err == nil && fi.Size() >= maxDropCounter {
		return nil
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write([]byte{'.'})
	return err
}

func safeName(id string) string {
	var b strings.Builder
	for _, c := range id {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteRune(c)
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}

// Drain calls fn for each spooled envelope in name (≈time) order and deletes
// the file when fn succeeds. It also counts and removes drop markers.
func Drain(dir string, fn func(*event.Envelope) error) (ingested int, drops int64, err error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		p := filepath.Join(dir, n)
		switch {
		case n == dropCounter:
			// Rename first so increments racing with the drain land in a new file.
			taken := p + "." + time.Now().Format("150405.000000000")
			if os.Rename(p, taken) == nil {
				if fi, err := os.Stat(taken); err == nil {
					drops += fi.Size()
				}
				_ = os.Remove(taken)
			}
		case strings.HasPrefix(n, dropCounter+"."):
			if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) > time.Minute {
				drops += fi.Size()
				_ = os.Remove(p)
			}
		case strings.HasPrefix(n, "drop-"): // legacy markers
			if os.Remove(p) == nil {
				drops++
			}
		case strings.HasSuffix(n, ".json"):
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				continue
			}
			var env event.Envelope
			if json.Unmarshal(b, &env) != nil || len(env.Raw) > 0 {
				_ = os.Remove(p) // corrupt or unsafe: discard, count as drop
				drops++
				continue
			}
			if ferr := fn(&env); ferr != nil {
				return ingested, drops, ferr
			}
			_ = os.Remove(p)
			ingested++
		case strings.HasSuffix(n, ".tmp"):
			if fi, e := os.Stat(p); e == nil && time.Since(fi.ModTime()) > time.Minute {
				_ = os.Remove(p)
			}
		}
	}
	return ingested, drops, nil
}
