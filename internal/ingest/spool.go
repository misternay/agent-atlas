package ingest

import (
	"context"

	"agentatlas/internal/event"
	"agentatlas/internal/normalize"
	"agentatlas/internal/spool"
)

// DrainSpool ingests envelopes the shim spooled while the daemon was down.
// Spooled events are already sanitized; they are re-sanitized and re-validated.
func (p *Pipeline) DrainSpool(ctx context.Context, dir string) (int, error) {
	n, drops, err := spool.Drain(dir, func(env *event.Envelope) error {
		evs, nerr := normalize.Envelope(env)
		if nerr != nil {
			p.Stats.Rejected.Add(1)
			return nil // discard invalid spool entries
		}
		if env.Dropped > 0 {
			p.RecordGap(env.Dropped, "shim_drop")
		}
		return p.SubmitWait(ctx, evs)
	})
	if drops > 0 {
		p.RecordGap(drops, "spool_full_or_shim_drop")
	}
	p.Stats.SpoolIngested.Add(int64(n))
	return n, err
}
