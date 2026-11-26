// Package sqlite is the authoritative local store (design §7): SQLite in WAL
// mode with a single serialized writer and a separate read pool.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"agentatlas/internal/correlate"
	"agentatlas/internal/event"
	"agentatlas/migrations"
)

// Store wraps the writer and reader connections.
type Store struct {
	path string
	w    *sql.DB // exactly one connection: all writes are serialized
	r    *sql.DB // read pool
	mu   sync.Mutex
}

func dsn(path string, readOnly bool) string {
	q := "_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	if readOnly {
		q = "_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)&_pragma=query_only(ON)"
	}
	return "file:" + path + "?" + q
}

// Open opens (creating if needed) the database at path with 0600 permissions
// and applies migrations.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		f.Close()
	}
	_ = os.Chmod(path, 0o600)
	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	s := &Store{path: path, w: w}
	if err := s.migrate(); err != nil {
		w.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	r, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	s.r = r
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Chmod(path+suffix, 0o600)
	}
	return s, nil
}

// Close closes both pools.
func (s *Store) Close() error {
	var errs []error
	if s.r != nil {
		errs = append(errs, s.r.Close())
	}
	errs = append(errs, s.w.Close())
	return errors.Join(errs...)
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

func (s *Store) migrate() error {
	if _, err := s.w.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		var exists int
		if err := s.w.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, n).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			continue
		}
		body, err := migrations.FS.ReadFile(n)
		if err != nil {
			return err
		}
		tx, err := s.w.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("%s: %w", n, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, n, time.Now().UnixMicro()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func marshalAttrs(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func unmarshalAttrs(s string) map[string]string {
	if s == "" || s == "{}" {
		return nil
	}
	var m map[string]string
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

// Commit persists a batch of sanitized events in one transaction, projecting
// each new event through proj. Duplicates (same event_id) are skipped. It
// returns the deltas of committed events in sequence order. On error nothing is
// committed and the projector cache is reset.
func (s *Store) Commit(ctx context.Context, evs []event.Event, proj *correlate.Projector) ([]correlate.Delta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	deltas, err := s.commitTx(ctx, tx, evs, proj)
	if err != nil {
		tx.Rollback()
		proj.Reset()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		proj.Reset()
		return nil, err
	}
	return deltas, nil
}

func (s *Store) commitTx(ctx context.Context, tx *sql.Tx, evs []event.Event, proj *correlate.Projector) ([]correlate.Delta, error) {
	var clearedBefore int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'cleared_before_us'), 0)`).Scan(&clearedBefore); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
		return nil, err
	}
	insEvent, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO events (event_id, session_id, type, harness, occurred_at, tool_call_id, metadata_json) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, err
	}
	defer insEvent.Close()
	var deltas []correlate.Delta
	for i := range evs {
		e := evs[i]
		if clearedBefore != 0 && e.Timestamp.UnixMicro() <= clearedBefore {
			continue // queued or spooled before the most recent clear
		}
		var dup int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE event_id = ?`, e.EventID).Scan(&dup); err != nil {
			return nil, err
		}
		if dup > 0 {
			continue // duplicate event_id: already committed (checked first so no sequence number is burned)
		}
		meta, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		var sessionID any
		if e.SessionID != "" && e.Type != event.TypeTelemetryGap {
			sessionID = correlate.SessionID(e.HostID, e.Harness, e.SessionID)
		}
		res, err := insEvent.ExecContext(ctx, e.EventID, sessionID, e.Type, e.Harness, e.Timestamp.UnixMicro(), e.ToolCallID, string(meta))
		if err != nil {
			return nil, err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			continue // duplicate event_id: already committed
		}
		seq, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}
		d := proj.Apply(seq, e)
		if err := s.persistDelta(ctx, tx, d); err != nil {
			return nil, err
		}
		if d.SpanID != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE events SET span_id = ? WHERE seq = ?`, d.SpanID, seq); err != nil {
				return nil, err
			}
		}
		deltas = append(deltas, d)
	}
	return deltas, nil
}

func (s *Store) persistDelta(ctx context.Context, tx *sql.Tx, d correlate.Delta) error {
	if ss := d.Session; ss != nil {
		_, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, host_id, harness, source_session_id, workspace_path, status, start_observed, started_at, ended_at, last_event_at, trace_id, root_span_id, tool_count, error_count, denied_count, unknown_count, model, attrs_json)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET workspace_path=excluded.workspace_path, status=excluded.status, start_observed=excluded.start_observed, started_at=excluded.started_at, ended_at=excluded.ended_at, last_event_at=excluded.last_event_at, tool_count=excluded.tool_count, error_count=excluded.error_count, denied_count=excluded.denied_count, unknown_count=excluded.unknown_count, model=excluded.model, attrs_json=excluded.attrs_json`,
			ss.ID, ss.HostID, ss.Harness, ss.SourceSessionID, ss.Workspace, ss.Status, boolInt(ss.StartObserved), ss.StartedAt, ss.EndedAt, ss.LastEventAt, ss.TraceID, ss.RootSpanID, ss.ToolCount, ss.ErrorCount, ss.DeniedCount, ss.UnknownCount, ss.Model, marshalAttrs(ss.Attrs))
		if err != nil {
			return fmt.Errorf("upsert session: %w", err)
		}
	}
	for _, sp := range d.Spans {
		_, err := tx.ExecContext(ctx, `INSERT INTO spans (id, trace_id, session_id, parent_span_id, kind, name, tool_category, operation, target_kind, target_label, tool_call_id, turn_id, status, status_evidence, outcome_basis, started_at, ended_at, duration_us, start_seq, end_seq, last_seq, attrs_json)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET parent_span_id=excluded.parent_span_id, name=excluded.name, tool_category=excluded.tool_category, operation=excluded.operation, target_kind=excluded.target_kind, target_label=excluded.target_label, status=excluded.status, status_evidence=excluded.status_evidence, outcome_basis=excluded.outcome_basis, started_at=excluded.started_at, ended_at=excluded.ended_at, duration_us=excluded.duration_us, start_seq=excluded.start_seq, end_seq=excluded.end_seq, last_seq=excluded.last_seq, attrs_json=excluded.attrs_json`,
			sp.ID, sp.TraceID, sp.SessionID, sp.ParentSpanID, sp.Kind, sp.Name, sp.ToolCategory, sp.Operation, sp.TargetKind, sp.TargetLabel, sp.ToolCallID, sp.TurnID, sp.Status, sp.StatusEvidence, sp.OutcomeBasis, sp.StartedAt, sp.EndedAt, sp.DurationUS, sp.StartSeq, sp.EndSeq, sp.LastSeq, marshalAttrs(sp.Attrs))
		if err != nil {
			return fmt.Errorf("upsert span: %w", err)
		}
	}
	for _, l := range d.Links {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO resources (id, kind, label) VALUES (?,?,?)`, l.ResourceID, l.Kind, l.Label); err != nil {
			return fmt.Errorf("insert resource: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO span_resources (span_id, resource_id, relation, evidence, event_seq) VALUES (?,?,?,?,?)
ON CONFLICT(span_id, resource_id, relation) DO UPDATE SET evidence=excluded.evidence, event_seq=excluded.event_seq`, l.SpanID, l.ResourceID, l.Relation, l.Evidence, l.EventSeq); err != nil {
			return fmt.Errorf("upsert link: %w", err)
		}
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// --- correlate.Loader implementation (reads committed rows) ---

const sessionCols = `id, host_id, harness, source_session_id, workspace_path, status, start_observed, started_at, ended_at, last_event_at, trace_id, root_span_id, tool_count, error_count, denied_count, unknown_count, model, attrs_json`

type scanner interface{ Scan(...any) error }

func scanSession(r scanner) (*correlate.Session, error) {
	var ss correlate.Session
	var startObs int
	var ended sql.NullInt64
	var attrs string
	if err := r.Scan(&ss.ID, &ss.HostID, &ss.Harness, &ss.SourceSessionID, &ss.Workspace, &ss.Status, &startObs, &ss.StartedAt, &ended, &ss.LastEventAt, &ss.TraceID, &ss.RootSpanID, &ss.ToolCount, &ss.ErrorCount, &ss.DeniedCount, &ss.UnknownCount, &ss.Model, &attrs); err != nil {
		return nil, err
	}
	ss.StartObserved = startObs == 1
	if ended.Valid {
		v := ended.Int64
		ss.EndedAt = &v
	}
	ss.Attrs = unmarshalAttrs(attrs)
	return &ss, nil
}

const spanCols = `id, trace_id, session_id, parent_span_id, kind, name, tool_category, operation, target_kind, target_label, tool_call_id, turn_id, status, status_evidence, outcome_basis, started_at, ended_at, duration_us, start_seq, end_seq, last_seq, attrs_json`

func scanSpan(r scanner) (*correlate.Span, error) {
	var sp correlate.Span
	var ended, dur sql.NullInt64
	var attrs string
	if err := r.Scan(&sp.ID, &sp.TraceID, &sp.SessionID, &sp.ParentSpanID, &sp.Kind, &sp.Name, &sp.ToolCategory, &sp.Operation, &sp.TargetKind, &sp.TargetLabel, &sp.ToolCallID, &sp.TurnID, &sp.Status, &sp.StatusEvidence, &sp.OutcomeBasis, &sp.StartedAt, &ended, &dur, &sp.StartSeq, &sp.EndSeq, &sp.LastSeq, &attrs); err != nil {
		return nil, err
	}
	if ended.Valid {
		v := ended.Int64
		sp.EndedAt = &v
	}
	if dur.Valid {
		v := dur.Int64
		sp.DurationUS = &v
	}
	sp.Attrs = unmarshalAttrs(attrs)
	return &sp, nil
}

// LoadSession implements correlate.Loader.
func (s *Store) LoadSession(id string) (*correlate.Session, error) {
	ss, err := scanSession(s.r.QueryRow(`SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return ss, err
}

// LoadSpan implements correlate.Loader.
func (s *Store) LoadSpan(id string) (*correlate.Span, error) {
	sp, err := scanSpan(s.r.QueryRow(`SELECT `+spanCols+` FROM spans WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return sp, err
}

// LoadOpenSpans implements correlate.Loader.
func (s *Store) LoadOpenSpans(sessionID string) ([]correlate.Span, error) {
	return s.querySpans(s.r, `SELECT `+spanCols+` FROM spans WHERE session_id = ? AND status IN ('running','stale') ORDER BY started_at`, sessionID)
}

// LoadLinks implements correlate.Loader.
func (s *Store) LoadLinks(spanID string) ([]correlate.Link, error) {
	return s.queryLinks(s.r, `SELECT sr.span_id, sr.resource_id, r.kind, r.label, sr.relation, sr.evidence, sr.event_seq FROM span_resources sr JOIN resources r ON r.id = sr.resource_id WHERE sr.span_id = ?`, spanID)
}

type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func (s *Store) querySpans(q querier, query string, args ...any) ([]correlate.Span, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []correlate.Span
	for rows.Next() {
		sp, err := scanSpan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sp)
	}
	return out, rows.Err()
}

func (s *Store) queryLinks(q querier, query string, args ...any) ([]correlate.Link, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []correlate.Link
	for rows.Next() {
		var l correlate.Link
		if err := rows.Scan(&l.SpanID, &l.ResourceID, &l.Kind, &l.Label, &l.Relation, &l.Evidence, &l.EventSeq); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// RunningToolSpansBefore returns committed running tool spans started before cutoff.
func (s *Store) RunningToolSpansBefore(cutoffUS int64, limit int) ([]correlate.Span, error) {
	return s.querySpans(s.r, `SELECT `+spanCols+` FROM spans WHERE kind = 'tool' AND status = 'running' AND started_at < ? ORDER BY started_at LIMIT ?`, cutoffUS, limit)
}
