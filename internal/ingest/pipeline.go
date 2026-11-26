// Package ingest receives envelopes from hook shims over a Unix socket,
// normalizes and sanitizes them, and commits them through a single writer
// before publishing deltas (write-before-publish, design §3.2).
package ingest

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"agentatlas/internal/correlate"
	"agentatlas/internal/event"
	"agentatlas/internal/security"
	"agentatlas/internal/storage/sqlite"
)

// ErrBusy means the ingest queue is full (backpressure).
var ErrBusy = errors.New("ingest queue full")

// Stats are ingest counters exposed via /api/v1/health.
type Stats struct {
	EnvelopesReceived atomic.Int64
	EventsCommitted   atomic.Int64
	Rejected          atomic.Int64
	Busy              atomic.Int64
	Dropped           atomic.Int64 // reported gaps (shim drops + daemon drops)
	SpoolIngested     atomic.Int64
	CommitErrors      atomic.Int64
	LastCommitUS      atomic.Int64
}

// Snapshot returns a plain copy of the counters.
func (s *Stats) Snapshot() map[string]int64 {
	return map[string]int64{
		"envelopes_received": s.EnvelopesReceived.Load(),
		"events_committed":   s.EventsCommitted.Load(),
		"rejected":           s.Rejected.Load(),
		"busy":               s.Busy.Load(),
		"dropped":            s.Dropped.Load(),
		"spool_ingested":     s.SpoolIngested.Load(),
		"commit_errors":      s.CommitErrors.Load(),
		"last_commit_us":     s.LastCommitUS.Load(),
	}
}

// Pipeline owns the single writer.
type Pipeline struct {
	store      *sqlite.Store
	proj       *correlate.Projector
	queue      chan []event.Event
	publish    func([]correlate.Delta)
	hostID     string
	Stats      Stats
	paused     atomic.Bool
	pendingGap atomic.Int64
	gapReason  atomic.Value
	excl       chan exclusiveReq
	log        *slog.Logger
}

type exclusiveReq struct {
	fn   func() error
	done chan error
}

// Exclusive runs fn on the writer goroutine after flushing the current batch,
// then resets the projector cache before any further commit. Use it
// for operations that delete rows (clear history, retention) so cached rows can
// never resurrect deleted sessions.
func (p *Pipeline) Exclusive(ctx context.Context, fn func() error) error {
	r := exclusiveReq{fn: fn, done: make(chan error, 1)}
	select {
	case p.excl <- r:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-r.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NewPipeline creates a pipeline. publish is called after each commit with the
// committed deltas in sequence order.
func NewPipeline(store *sqlite.Store, hostID string, publish func([]correlate.Delta), queueSize int, log *slog.Logger) *Pipeline {
	if queueSize <= 0 {
		queueSize = 4096
	}
	if log == nil {
		log = slog.Default()
	}
	p := &Pipeline{store: store, proj: correlate.New(store), queue: make(chan []event.Event, queueSize), publish: publish, hostID: security.CleanID(hostID), log: log, excl: make(chan exclusiveReq)}
	p.gapReason.Store("")
	return p
}

// SetPaused toggles collection pause. While paused, envelopes are refused and
// shims drop them (nothing is spooled), matching the user's intent.
func (p *Pipeline) SetPaused(v bool) { p.paused.Store(v) }

// Paused reports whether collection is paused.
func (p *Pipeline) Paused() bool { return p.paused.Load() }

// Submit enqueues without blocking.
func (p *Pipeline) Submit(evs []event.Event) error {
	if len(evs) == 0 {
		return nil
	}
	select {
	case p.queue <- evs:
		return nil
	default:
		p.Stats.Busy.Add(1)
		return ErrBusy
	}
}

// SubmitWait enqueues, blocking until accepted or ctx ends.
func (p *Pipeline) SubmitWait(ctx context.Context, evs []event.Event) error {
	if len(evs) == 0 {
		return nil
	}
	select {
	case p.queue <- evs:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RecordGap notes n lost events; a telemetry.gap event is committed shortly.
func (p *Pipeline) RecordGap(n int64, reason string) {
	if n <= 0 {
		return
	}
	p.Stats.Dropped.Add(n)
	p.pendingGap.Add(n)
	p.gapReason.Store(reason)
}

// Run is the writer loop: it batches queued events (≤256 or 25ms), commits
// them in one transaction, then publishes deltas.
func (p *Pipeline) Run(ctx context.Context) {
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	evict := time.NewTicker(5 * time.Minute)
	defer evict.Stop()
	var batch []event.Event
	var backoff time.Duration
	var backoffUntil time.Time
	flush := func() {
		// After a commit failure, back off (up to 30s) instead of retrying every
		// tick; bound what accumulates meanwhile and count the excess as a gap.
		if time.Now().Before(backoffUntil) {
			if len(batch) > 10000 {
				p.RecordGap(int64(len(batch)), "commit_backoff_overflow")
				batch = batch[:0]
			}
			return
		}
		var gapN int64
		gapReason := ""
		if n := p.pendingGap.Swap(0); n > 0 {
			reason, _ := p.gapReason.Load().(string)
			gapN, gapReason = n, reason
			batch = append(batch, event.Event{
				SchemaVersion: event.SchemaVersion, EventID: event.NewID("gap_"), Timestamp: time.Now().UTC(),
				HostID: p.hostID, Harness: event.HarnessAtlas, Type: event.TypeTelemetryGap, SourceEvent: "atlas.gap",
				Evidence: event.EvidenceObserved, Privacy: event.PrivacyMetadataOnly,
				Attrs: map[string]string{"gap_count": strconv.FormatInt(n, 10), "gap_reason": reason},
			})
		}
		if len(batch) == 0 {
			return
		}
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		deltas, err := p.store.Commit(cctx, batch, p.proj)
		cancel()
		if err != nil {
			p.Stats.CommitErrors.Add(1)
			backoff = min(max(2*backoff, 100*time.Millisecond), 30*time.Second)
			backoffUntil = time.Now().Add(backoff)
			p.log.Error("commit failed", "err", err, "events", len(batch), "retry_in", backoff)
			// Count the lost batch as a gap rather than retrying forever, and
			// keep the pending gap that rode along in this batch.
			lost := int64(len(batch))
			if gapN > 0 {
				lost-- // the telemetry.gap event itself
				p.pendingGap.Add(gapN)
				p.gapReason.Store(gapReason)
			}
			if lost > 0 {
				p.Stats.Dropped.Add(lost)
				p.pendingGap.Add(lost)
				p.gapReason.Store("commit_error")
			}
			batch = batch[:0]
			return
		}
		backoff, backoffUntil = 0, time.Time{}
		p.Stats.EventsCommitted.Add(int64(len(deltas)))
		p.Stats.LastCommitUS.Store(time.Now().UnixMicro())
		batch = batch[:0]
		if len(deltas) > 0 && p.publish != nil {
			p.publish(deltas)
		}
	}
	for {
		select {
		case <-ctx.Done():
			// drain what is already queued
			for {
				select {
				case evs := <-p.queue:
					batch = append(batch, evs...)
				default:
					flush()
					return
				}
			}
		case evs := <-p.queue:
			batch = append(batch, evs...)
			if len(batch) >= 256 {
				flush()
			}
		case <-tick.C:
			flush()
		case r := <-p.excl:
			flush()
			err := r.fn()
			p.proj.Reset()
			r.done <- err
		case <-evict.C:
			// The writer goroutine is the only projector mutator, so evict here.
			p.proj.Evict(time.Now().Add(-30 * time.Minute).UnixMicro())
		}
	}
}

// SweepStale marks running tool spans older than threshold as stale by
// committing synthetic tool.stale events (so replay stays deterministic).
func (p *Pipeline) SweepStale(ctx context.Context, threshold time.Duration) (int, error) {
	now := time.Now().UTC()
	cutoff := now.Add(-threshold).UnixMicro()
	spans, err := p.store.RunningToolSpansBefore(cutoff, 500)
	if err != nil {
		return 0, err
	}
	var evs []event.Event
	for _, sp := range spans {
		if sp.ToolCallID == "" {
			continue
		}
		sess, err := p.store.GetSession(ctx, sp.SessionID)
		if err != nil || sess == nil {
			continue
		}
		evs = append(evs, event.Event{
			SchemaVersion: event.SchemaVersion, EventID: "stale_" + sp.ID + "_" + strconv.FormatInt(sp.LastSeq, 10),
			Timestamp: now, HostID: sess.HostID, Harness: sess.Harness, SessionID: sess.SourceSessionID,
			TurnID: sp.TurnID, ToolCallID: sp.ToolCallID, Type: event.TypeToolStale, SourceEvent: "atlas.stale_sweep",
			Status: event.StatusStale, Evidence: event.EvidenceDerived, Privacy: event.PrivacyMetadataOnly,
			Attrs: map[string]string{"stale_after_s": strconv.Itoa(int(threshold.Seconds()))},
		})
	}
	if len(evs) == 0 {
		return 0, nil
	}
	return len(evs), p.SubmitWait(ctx, evs)
}
