// Package shim implements `atlas hook`: it receives a native hook payload on
// stdin and forwards it to atlasd over the Unix socket. It never writes to
// stdout, never decides approvals, and always exits 0 — observability must not
// block or alter agent work (design §4.4).
package shim

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentatlas/internal/config"
	"agentatlas/internal/event"
	"agentatlas/internal/normalize"
	"agentatlas/internal/spool"
)

// Timeouts (kept short so a missing daemon costs little).
var (
	ReadTimeout  = 1 * time.Second
	DialTimeout  = 150 * time.Millisecond
	ReplyTimeout = 400 * time.Millisecond
)

// Outcome describes what the shim did (for tests and debug logging).
type Outcome string

// Outcomes.
const (
	Sent       Outcome = "sent"
	Spooled    Outcome = "spooled"
	Dropped    Outcome = "dropped"
	Paused     Outcome = "paused"
	Rejected   Outcome = "rejected"
	NoInput    Outcome = "no_input"
	BadHarness Outcome = "bad_harness"
)

// PausedFile is created by atlasd while collection is paused.
const PausedFile = "paused"

// Run handles one hook invocation. It recovers from panics; callers exit 0.
func Run(harness string, stdin io.Reader, home string) (out Outcome) {
	defer func() {
		if r := recover(); r != nil {
			out = Dropped
		}
		debugLog(home, harness, out)
	}()
	if harness != event.HarnessClaude && harness != event.HarnessCodex {
		return BadHarness
	}
	paths := config.PathsFor(home)
	if _, err := os.Stat(filepath.Join(home, PausedFile)); err == nil {
		return Paused
	}
	raw, res := readBounded(stdin, event.MaxPayloadBytes, ReadTimeout)
	if res != readOK {
		// Oversize or too slow: the event is lost; record it as a telemetry gap.
		_ = spool.RecordDrop(paths.Spool, event.NewID(string(res)))
		return Dropped
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return NoInput
	}
	cfg, _ := config.Load(paths)
	env := &event.Envelope{
		V: event.EnvelopeVersion, EnvelopeID: event.NewID(""), ReceivedAt: time.Now().UTC(),
		Harness: harness, HostID: cfg.HostID, Raw: json.RawMessage(raw),
	}
	if !json.Valid(raw) {
		return Rejected
	}
	switch send(paths.Socket, env) {
	case "ok":
		return Sent
	case "rejected":
		return Rejected
	case "paused":
		return Paused
	}
	// Daemon unavailable or busy: sanitize locally, then spool only sanitized events.
	evs, err := normalize.Envelope(env)
	env.Raw = nil
	raw = nil
	if err != nil {
		return Rejected
	}
	if len(evs) == 0 {
		return Sent // nothing recordable (e.g. Notification hook)
	}
	senv := &event.Envelope{V: event.EnvelopeVersion, EnvelopeID: env.EnvelopeID, ReceivedAt: env.ReceivedAt, Harness: harness, HostID: evs[0].HostID, Events: evs}
	if err := spool.Append(paths.Spool, senv, cfg.SpoolMaxBytes); err != nil {
		_ = spool.RecordDrop(paths.Spool, env.EnvelopeID)
		return Dropped
	}
	return Spooled
}

type readResult string

const (
	readOK       readResult = "ok"
	readOversize readResult = "oversize"
	readTimeout  readResult = "timeout"
)

// readBounded reads at most max bytes within timeout.
func readBounded(r io.Reader, max int, timeout time.Duration) ([]byte, readResult) {
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
		ch <- res{b, err}
	}()
	select {
	case x := <-ch:
		if len(x.b) > max {
			return nil, readOversize
		}
		return x.b, readOK
	case <-time.After(timeout):
		return nil, readTimeout
	}
}

// send delivers the envelope and returns the daemon's reply code, or "" when
// the daemon is unreachable or did not answer in time.
func send(socket string, env *event.Envelope) string {
	if err := config.CheckPrivateSocket(socket); err != nil {
		return ""
	}
	c, err := net.DialTimeout("unix", socket, DialTimeout)
	if err != nil {
		return ""
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(DialTimeout + ReplyTimeout + 200*time.Millisecond))
	b, err := json.Marshal(env)
	if err != nil {
		return ""
	}
	if _, err := c.Write(append(b, '\n')); err != nil {
		return ""
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	return strings.TrimSpace(line)
}

// debugLog appends a one-line outcome (no payload data) when ATLAS_HOOK_DEBUG=1.
func debugLog(home, harness string, out Outcome) {
	if os.Getenv("ATLAS_HOOK_DEBUG") != "1" {
		return
	}
	f, err := os.OpenFile(filepath.Join(home, "hook-debug.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(time.Now().UTC().Format(time.RFC3339Nano) + " " + harness + " " + string(out) + "\n")
}
