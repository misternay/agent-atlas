package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"agentatlas/internal/config"
	"agentatlas/internal/ingest"
	"agentatlas/internal/storage/sqlite"
)

// retention enforces the age limit and the database size target, checkpoints
// the WAL, and records what was removed so the UI can tell the user.
type retention struct {
	store *sqlite.Store
	pipe  *ingest.Pipeline
	cfg   config.Config
	log   *slog.Logger
	// onDeleted is called after rows were removed (bumps generation, tells clients to resync).
	onDeleted func(context.Context)
	mu        sync.Mutex
	last      map[string]any
}

func (r *retention) run(ctx context.Context) {
	var removed sqlite.PruneResult
	// Deletions run on the writer goroutine so the projector cache is reset
	// before any further commit (no resurrected rows).
	err := r.pipe.Exclusive(ctx, func() error {
		var err error
		removed, err = r.prune(ctx)
		return err
	})
	if err != nil {
		r.log.Warn("retention prune", "err", err)
		return
	}
	if (removed.Sessions > 0 || removed.Events > 0) && r.onDeleted != nil {
		r.onDeleted(ctx)
	}
	r.record(removed)
}

func (r *retention) prune(ctx context.Context) (sqlite.PruneResult, error) {
	cutoff := time.Now().AddDate(0, 0, -r.cfg.RetentionDays).UnixMicro()
	res, err := r.store.PruneBefore(ctx, cutoff)
	if err != nil {
		return res, err
	}
	removed := res
	// Size cap: prune least-recently-active sessions in steps until under target.
	for i := 0; i < 50 && r.store.TotalBytes() > r.cfg.MaxDBBytes; i++ {
		_ = r.store.Checkpoint(ctx)
		if r.store.TotalBytes() <= r.cfg.MaxDBBytes {
			break
		}
		// Remove ~10% of sessions per step (at least one), oldest first, never
		// sessions active in the last 10 minutes.
		n := max(1, r.store.SessionCount(ctx)/10)
		pr, err := r.store.PruneOldestSessions(ctx, n, time.Now().Add(-10*time.Minute).UnixMicro())
		if err != nil || pr.Sessions == 0 {
			break
		}
		removed.Sessions += pr.Sessions
		removed.Events += pr.Events
		_ = r.store.Vacuum(ctx)
	}
	if err := r.store.Checkpoint(ctx); err != nil {
		r.log.Warn("wal checkpoint", "err", err)
	}
	return removed, nil
}

func (r *retention) record(removed sqlite.PruneResult) {
	r.mu.Lock()
	r.last = map[string]any{
		"ran_at":           time.Now().UTC().Format(time.RFC3339),
		"retention_days":   r.cfg.RetentionDays,
		"max_db_bytes":     r.cfg.MaxDBBytes,
		"removed_sessions": removed.Sessions,
		"removed_events":   removed.Events,
		"db_bytes":         r.store.TotalBytes(),
	}
	r.mu.Unlock()
	if removed.Sessions > 0 || removed.Events > 0 {
		r.log.Info("retention removed history", "sessions", removed.Sessions, "events", removed.Events)
	}
}

func (r *retention) notes() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}
