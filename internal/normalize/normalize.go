// Package normalize turns a shim envelope into validated, sanitized events. It
// is shared by the daemon (normal path) and the hook shim (offline spool path),
// so redaction always happens before anything touches disk.
package normalize

import (
	"fmt"

	"agentatlas/internal/adapters"
	"agentatlas/internal/adapters/claude"
	"agentatlas/internal/adapters/codex"
	"agentatlas/internal/event"
	"agentatlas/internal/security"
)

var registry = map[string]adapters.Adapter{
	event.HarnessClaude: claude.New(),
	event.HarnessCodex:  codex.New(),
}

// Adapter returns the adapter for a harness.
func Adapter(harness string) (adapters.Adapter, bool) {
	a, ok := registry[harness]
	return a, ok
}

// Adapters returns all registered adapters.
func Adapters() []adapters.Adapter {
	return []adapters.Adapter{registry[event.HarnessClaude], registry[event.HarnessCodex]}
}

// Envelope normalizes and sanitizes an envelope. Raw payload bytes are only
// read in memory. Already-sanitized spool events are re-sanitized and
// re-validated (defense in depth).
func Envelope(env *event.Envelope) ([]event.Event, error) {
	if err := env.Validate(); err != nil {
		return nil, err
	}
	var evs []event.Event
	if len(env.Raw) > 0 {
		a, ok := registry[env.Harness]
		if !ok {
			return nil, fmt.Errorf("no adapter for %q", env.Harness)
		}
		out, err := a.Normalize(env.Raw, adapters.Meta{EnvelopeID: env.EnvelopeID, ReceivedAt: env.ReceivedAt, HostID: env.HostID})
		if err != nil {
			return nil, err
		}
		evs = out
	} else {
		evs = append(evs, env.Events...)
	}
	clean := evs[:0]
	for i := range evs {
		if evs[i].HostID == "" {
			evs[i].HostID = env.HostID
		}
		e := security.SanitizeEvent(&evs[i])
		if err := e.Validate(); err != nil {
			return nil, fmt.Errorf("event %d invalid: %w", i, err)
		}
		clean = append(clean, *e)
	}
	return clean, nil
}
