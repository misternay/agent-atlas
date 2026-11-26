package event

import (
	"encoding/json"
	"errors"
	"time"
)

// EnvelopeVersion is the shim→daemon wire version.
const EnvelopeVersion = 1

// MaxPayloadBytes caps the raw native hook payload accepted by shim and daemon.
const MaxPayloadBytes = 1 << 20

// Envelope is what the hook shim sends to the daemon over the Unix socket.
// Raw holds the native hook JSON; it exists only in memory and on the local
// socket and is never written to disk by either side.
//
// Events carries already-sanitized events (used when replaying the shim's
// offline spool); in that case Raw is empty.
type Envelope struct {
	V          int             `json:"v"`
	EnvelopeID string          `json:"envelope_id"` // stable id; normalized event ids derive from it
	ReceivedAt time.Time       `json:"received_at"` // shim stamp; native payloads carry no timestamp
	Harness    string          `json:"harness"`
	HostID     string          `json:"host_id"`
	Raw        json.RawMessage `json:"raw,omitempty"`
	Events     []Event         `json:"events,omitempty"`
	Dropped    int64           `json:"dropped,omitempty"` // shim-side drops being reported
}

// Validate checks envelope-level invariants.
func (e *Envelope) Validate() error {
	if e.V != EnvelopeVersion {
		return errors.New("unsupported envelope version")
	}
	if e.EnvelopeID == "" || len(e.EnvelopeID) > 64 {
		return errors.New("bad envelope id")
	}
	if e.Harness != HarnessClaude && e.Harness != HarnessCodex {
		return errors.New("unknown harness")
	}
	if len(e.Raw) > MaxPayloadBytes {
		return errors.New("payload too large")
	}
	if len(e.Raw) == 0 && len(e.Events) == 0 && e.Dropped == 0 {
		return errors.New("empty envelope")
	}
	if e.ReceivedAt.IsZero() {
		return errors.New("missing received_at")
	}
	return nil
}
