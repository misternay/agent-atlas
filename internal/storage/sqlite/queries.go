package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"agentatlas/internal/correlate"
	"agentatlas/internal/event"
)

// StoredEvent is a committed event with its sequence number.
type StoredEvent struct {
	Seq       int64       `json:"seq"`
	SessionID string      `json:"session_id,omitempty"`
	SpanID    string      `json:"span_id,omitempty"`
	Event     event.Event `json:"event"`
}

// SessionFilter filters session listings. Zero values mean "any".
type SessionFilter struct {
	Status    string // active | ended
	Harness   string
	Workspace string // substring
	SessionID string // substring of id or source session id
	From, To  int64  // last_event_at range (µs)
	Limit     int
	Offset    int
	// OldestFirst orders by last activity ascending (default newest first).
	OldestFirst bool
}

func clampLimit(n, def, max int) int {
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

// ListSessions returns sessions newest-first and the total match count.
func (s *Store) ListSessions(ctx context.Context, f SessionFilter) ([]correlate.Session, int, error) {
	var where []string
	var args []any
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if f.Harness != "" {
		where = append(where, "harness = ?")
		args = append(args, f.Harness)
	}
	if f.Workspace != "" {
		where = append(where, `workspace_path LIKE ? ESCAPE '\'`)
		args = append(args, likeEscape(f.Workspace))
	}
	if f.SessionID != "" {
		where = append(where, `(id LIKE ? ESCAPE '\' OR source_session_id LIKE ? ESCAPE '\')`)
		args = append(args, likeEscape(f.SessionID), likeEscape(f.SessionID))
	}
	if f.From > 0 {
		where = append(where, "last_event_at >= ?")
		args = append(args, f.From)
	}
	if f.To > 0 {
		where = append(where, "started_at <= ?")
		args = append(args, f.To)
	}
	w := ""
	if len(where) > 0 {
		w = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := clampLimit(f.Limit, 100, 500)
	order := ` ORDER BY last_event_at DESC, id DESC`
	if f.OldestFirst {
		order = ` ORDER BY last_event_at ASC, id ASC`
	}
	rows, err := s.r.QueryContext(ctx, `SELECT `+sessionCols+` FROM sessions`+w+order+` LIMIT ? OFFSET ?`, append(args, limit, max(f.Offset, 0))...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []correlate.Session
	for rows.Next() {
		ss, err := scanSession(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *ss)
	}
	return out, total, rows.Err()
}

// GetSession returns one session or nil.
func (s *Store) GetSession(ctx context.Context, id string) (*correlate.Session, error) {
	ss, err := scanSession(s.r.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return ss, err
}

// GetSessionByTrace returns the session owning a trace id.
func (s *Store) GetSessionByTrace(ctx context.Context, traceID string) (*correlate.Session, error) {
	ss, err := scanSession(s.r.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE trace_id = ?`, traceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return ss, err
}

func (s *Store) queryEvents(ctx context.Context, query string, args ...any) ([]StoredEvent, error) {
	rows, err := s.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredEvent
	for rows.Next() {
		var se StoredEvent
		var sid sql.NullString
		var meta string
		if err := rows.Scan(&se.Seq, &sid, &se.SpanID, &meta); err != nil {
			return nil, err
		}
		se.SessionID = sid.String
		if err := json.Unmarshal([]byte(meta), &se.Event); err != nil {
			return nil, err
		}
		out = append(out, se)
	}
	return out, rows.Err()
}

// SessionEvents returns a session's events after a sequence cursor.
func (s *Store) SessionEvents(ctx context.Context, sessionID string, afterSeq int64, limit int) ([]StoredEvent, error) {
	return s.queryEvents(ctx, `SELECT seq, session_id, span_id, metadata_json FROM events WHERE session_id = ? AND seq > ? ORDER BY seq LIMIT ?`, sessionID, afterSeq, clampLimit(limit, 500, 100000))
}

// EventsAfter returns events across all sessions after a cursor (stream resume).
func (s *Store) EventsAfter(ctx context.Context, afterSeq int64, limit int) ([]StoredEvent, error) {
	return s.queryEvents(ctx, `SELECT seq, session_id, span_id, metadata_json FROM events WHERE seq > ? ORDER BY seq LIMIT ?`, afterSeq, clampLimit(limit, 500, 5000))
}

// SeqBounds returns the smallest retained and the largest assigned sequence.
// MaxSeq comes from sqlite_sequence so it never goes backwards after pruning.
func (s *Store) SeqBounds(ctx context.Context) (minSeq, maxSeq int64, err error) {
	var mn sql.NullInt64
	if err = s.r.QueryRowContext(ctx, `SELECT MIN(seq) FROM events`).Scan(&mn); err != nil {
		return
	}
	var mx sql.NullInt64
	_ = s.r.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = 'events'`).Scan(&mx)
	return mn.Int64, mx.Int64, nil
}

// TraceSpans returns all spans and links of a trace.
func (s *Store) TraceSpans(ctx context.Context, traceID string) ([]correlate.Span, []correlate.Link, error) {
	spans, err := s.querySpans(s.r, `SELECT `+spanCols+` FROM spans WHERE trace_id = ? ORDER BY started_at, id LIMIT 20000`, traceID)
	if err != nil {
		return nil, nil, err
	}
	links, err := s.queryLinks(s.r, `SELECT sr.span_id, sr.resource_id, r.kind, r.label, sr.relation, sr.evidence, sr.event_seq FROM span_resources sr JOIN resources r ON r.id = sr.resource_id JOIN spans sp ON sp.id = sr.span_id WHERE sp.trace_id = ? LIMIT 50000`, traceID)
	return spans, links, err
}

// SpanFilter filters span searches.
type SpanFilter struct {
	Tool      string // exact tool name or substring
	Category  string
	Status    string
	Harness   string
	Workspace string
	SessionID string
	Resource  string // substring of a linked resource label
	Evidence  string
	Kind      string // defaults to tool
	From, To  int64
	Limit     int
	Offset    int
}

// SpanResult is a span with session context for search results.
type SpanResult struct {
	correlate.Span
	Harness   string `json:"harness"`
	Workspace string `json:"workspace"`
}

// SearchSpans runs an indexed span search.
func (s *Store) SearchSpans(ctx context.Context, f SpanFilter) ([]SpanResult, int, error) {
	where := []string{"sp.kind = ?"}
	kind := f.Kind
	if kind == "" {
		kind = correlate.KindTool
	}
	args := []any{kind}
	if f.Tool != "" {
		where = append(where, `(sp.name = ? OR sp.name LIKE ? ESCAPE '\')`)
		args = append(args, f.Tool, likeEscape(f.Tool))
	}
	if f.Category != "" {
		where = append(where, "sp.tool_category = ?")
		args = append(args, f.Category)
	}
	if f.Status != "" {
		where = append(where, "sp.status = ?")
		args = append(args, f.Status)
	}
	if f.Harness != "" {
		where = append(where, "se.harness = ?")
		args = append(args, f.Harness)
	}
	if f.Workspace != "" {
		where = append(where, `se.workspace_path LIKE ? ESCAPE '\'`)
		args = append(args, likeEscape(f.Workspace))
	}
	if f.SessionID != "" {
		where = append(where, `(se.id = ? OR se.source_session_id = ?)`)
		args = append(args, f.SessionID, f.SessionID)
	}
	if f.Resource != "" {
		where = append(where, `EXISTS (SELECT 1 FROM span_resources sr JOIN resources r ON r.id = sr.resource_id WHERE sr.span_id = sp.id AND r.label LIKE ? ESCAPE '\')`)
		args = append(args, likeEscape(f.Resource))
	}
	if f.Evidence != "" {
		where = append(where, `(sp.status_evidence = ? OR EXISTS (SELECT 1 FROM span_resources sr WHERE sr.span_id = sp.id AND sr.evidence = ?))`)
		args = append(args, f.Evidence, f.Evidence)
	}
	if f.From > 0 {
		where = append(where, "sp.started_at >= ?")
		args = append(args, f.From)
	}
	if f.To > 0 {
		where = append(where, "sp.started_at <= ?")
		args = append(args, f.To)
	}
	w := " WHERE " + strings.Join(where, " AND ")
	from := ` FROM spans sp JOIN sessions se ON se.id = sp.session_id`
	var total int
	if err := s.r.QueryRowContext(ctx, `SELECT COUNT(*)`+from+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := clampLimit(f.Limit, 100, 500)
	cols := strings.ReplaceAll("sp."+strings.ReplaceAll(spanCols, ", ", ", sp."), "sp.sp.", "sp.")
	rows, err := s.r.QueryContext(ctx, `SELECT `+cols+`, se.harness, se.workspace_path`+from+w+` ORDER BY sp.started_at DESC LIMIT ? OFFSET ?`, append(args, limit, max(f.Offset, 0))...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []SpanResult
	for rows.Next() {
		var r SpanResult
		var ended, dur sql.NullInt64
		var attrs string
		sp := &r.Span
		if err := rows.Scan(&sp.ID, &sp.TraceID, &sp.SessionID, &sp.ParentSpanID, &sp.Kind, &sp.Name, &sp.ToolCategory, &sp.Operation, &sp.TargetKind, &sp.TargetLabel, &sp.ToolCallID, &sp.TurnID, &sp.Status, &sp.StatusEvidence, &sp.OutcomeBasis, &sp.StartedAt, &ended, &dur, &sp.StartSeq, &sp.EndSeq, &sp.LastSeq, &attrs, &r.Harness, &r.Workspace); err != nil {
			return nil, 0, err
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
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// Snapshot is the live-view state at a cursor.
type Snapshot struct {
	Cursor     int64               `json:"cursor"`
	Generation int64               `json:"generation"`
	Sessions   []correlate.Session `json:"sessions"`
	Spans      []correlate.Span    `json:"spans"`
	Links      []correlate.Link    `json:"links"`
}

// LiveSnapshot returns recently active sessions, all open spans, and spans that
// ended at/after sinceUS, plus their links. sessionID optionally scopes it.
func (s *Store) LiveSnapshot(ctx context.Context, sinceUS, sessionsSinceUS int64, sessionID string, maxSpans int) (*Snapshot, error) {
	// Read the cursor first: the snapshot reflects at least everything <= cursor;
	// clients dedupe later deltas by span id/last_seq.
	_, cursor, err := s.SeqBounds(ctx)
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{Cursor: cursor, Generation: s.Generation(ctx)}
	sf := SessionFilter{From: sessionsSinceUS, Limit: 500}
	if sessionID != "" {
		sf = SessionFilter{SessionID: sessionID, Limit: 1}
	}
	sessions, _, err := s.ListSessions(ctx, sf)
	if err != nil {
		return nil, err
	}
	snap.Sessions = sessions
	maxSpans = clampLimit(maxSpans, 2000, 10000)
	q := `SELECT ` + spanCols + ` FROM spans WHERE (status IN ('running','stale') OR ended_at >= ? OR started_at >= ?)`
	args := []any{sinceUS, sinceUS}
	if sessionID != "" {
		q += ` AND session_id = ?`
		args = append(args, sessionID)
	}
	q += ` ORDER BY started_at DESC LIMIT ?`
	args = append(args, maxSpans)
	spans, err := s.querySpans(s.r, q, args...)
	if err != nil {
		return nil, err
	}
	snap.Spans = spans
	if len(spans) > 0 {
		ids := make([]any, len(spans))
		ph := make([]string, len(spans))
		for i, sp := range spans {
			ids[i] = sp.ID
			ph[i] = "?"
		}
		links, err := s.queryLinks(s.r, `SELECT sr.span_id, sr.resource_id, r.kind, r.label, sr.relation, sr.evidence, sr.event_seq FROM span_resources sr JOIN resources r ON r.id = sr.resource_id WHERE sr.span_id IN (`+strings.Join(ph, ",")+`)`, ids...)
		if err != nil {
			return nil, err
		}
		snap.Links = links
	}
	if snap.Sessions == nil {
		snap.Sessions = []correlate.Session{}
	}
	if snap.Spans == nil {
		snap.Spans = []correlate.Span{}
	}
	if snap.Links == nil {
		snap.Links = []correlate.Link{}
	}
	return snap, nil
}

// Stats summarizes storage.
type Stats struct {
	Sessions      int64 `json:"sessions"`
	Events        int64 `json:"events"`
	Spans         int64 `json:"spans"`
	Resources     int64 `json:"resources"`
	DBBytes       int64 `json:"db_bytes"`
	WALBytes      int64 `json:"wal_bytes"`
	OldestEventUS int64 `json:"oldest_event_us"`
	MinSeq        int64 `json:"min_seq"`
	MaxSeq        int64 `json:"max_seq"`
}

// Stats returns storage statistics.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	for q, dst := range map[string]*int64{
		`SELECT COUNT(*) FROM sessions`:  &st.Sessions,
		`SELECT COUNT(*) FROM events`:    &st.Events,
		`SELECT COUNT(*) FROM spans`:     &st.Spans,
		`SELECT COUNT(*) FROM resources`: &st.Resources,
	} {
		if err := s.r.QueryRowContext(ctx, q).Scan(dst); err != nil {
			return st, err
		}
	}
	var oldest sql.NullInt64
	_ = s.r.QueryRowContext(ctx, `SELECT MIN(occurred_at) FROM events`).Scan(&oldest)
	st.OldestEventUS = oldest.Int64
	st.MinSeq, st.MaxSeq, _ = s.SeqBounds(ctx)
	st.DBBytes = fileSize(s.path)
	st.WALBytes = fileSize(s.path + "-wal")
	return st, nil
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// TotalBytes is the on-disk footprint (db + wal).
func (s *Store) TotalBytes() int64 { return fileSize(s.path) + fileSize(s.path+"-wal") }

// PruneResult reports what retention removed.
type PruneResult struct {
	Sessions int64 `json:"sessions"`
	Events   int64 `json:"events"`
}

// PruneBefore deletes ended-or-idle sessions whose last activity is older than
// beforeUS (cascading to events/spans/links), orphan events older than
// beforeUS, and unreferenced resources.
func (s *Store) PruneBefore(ctx context.Context, beforeUS int64) (PruneResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var res PruneResult
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	var evCount int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE session_id IN (SELECT id FROM sessions WHERE last_event_at < ?)`, beforeUS).Scan(&evCount); err != nil {
		return res, err
	}
	r1, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE last_event_at < ?`, beforeUS)
	if err != nil {
		return res, err
	}
	res.Sessions, _ = r1.RowsAffected()
	r2, err := tx.ExecContext(ctx, `DELETE FROM events WHERE occurred_at < ? AND (session_id IS NULL OR session_id NOT IN (SELECT id FROM sessions))`, beforeUS)
	if err != nil {
		return res, err
	}
	n2, _ := r2.RowsAffected()
	res.Events = evCount + n2
	if _, err := tx.ExecContext(ctx, `DELETE FROM resources WHERE id NOT IN (SELECT resource_id FROM span_resources)`); err != nil {
		return res, err
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	return res, nil
}

// PruneOldestSessions deletes up to the n least-recently-active sessions,
// never touching sessions active at or after protectFromUS.
func (s *Store) PruneOldestSessions(ctx context.Context, n int, protectFromUS int64) (PruneResult, error) {
	var cutoff sql.NullInt64
	err := s.r.QueryRowContext(ctx, `SELECT MAX(last_event_at) FROM (SELECT last_event_at FROM sessions WHERE last_event_at < ? ORDER BY last_event_at ASC LIMIT ?)`, protectFromUS, max(n, 1)).Scan(&cutoff)
	if errors.Is(err, sql.ErrNoRows) || !cutoff.Valid {
		return PruneResult{}, nil
	}
	if err != nil {
		return PruneResult{}, err
	}
	return s.PruneBefore(ctx, cutoff.Int64+1)
}

// EndedSession pairs a session with the commit sequence of its end event.
type EndedSession struct {
	Seq     int64
	Session correlate.Session
}

// EndedSessionsAfterSeq returns ended sessions in commit order of their
// session.ended events (used by the OTLP exporter; immune to late spooled ends).
func (s *Store) EndedSessionsAfterSeq(ctx context.Context, afterSeq int64, limit int) ([]EndedSession, error) {
	cols := strings.ReplaceAll("s."+strings.ReplaceAll(sessionCols, ", ", ", s."), "s.s.", "s.")
	rows, err := s.r.QueryContext(ctx, `SELECT e.seq, `+cols+` FROM events e JOIN sessions s ON s.id = e.session_id WHERE e.type = 'session.ended' AND e.seq > ? AND s.status = 'ended' ORDER BY e.seq LIMIT ?`, afterSeq, clampLimit(limit, 100, 1000))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EndedSession
	for rows.Next() {
		var es EndedSession
		var startObs int
		var ended sql.NullInt64
		var attrs string
		ss := &es.Session
		if err := rows.Scan(&es.Seq, &ss.ID, &ss.HostID, &ss.Harness, &ss.SourceSessionID, &ss.Workspace, &ss.Status, &startObs, &ss.StartedAt, &ended, &ss.LastEventAt, &ss.TraceID, &ss.RootSpanID, &ss.ToolCount, &ss.ErrorCount, &ss.DeniedCount, &ss.UnknownCount, &ss.Model, &attrs); err != nil {
			return nil, err
		}
		ss.StartObserved = startObs == 1
		if ended.Valid {
			v := ended.Int64
			ss.EndedAt = &v
		}
		ss.Attrs = unmarshalAttrs(attrs)
		out = append(out, es)
	}
	return out, rows.Err()
}

// SessionCount returns the number of sessions.
func (s *Store) SessionCount(ctx context.Context) int {
	var n int
	_ = s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n)
	return n
}

// Checkpoint truncates the WAL.
func (s *Store) Checkpoint(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.w.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// Vacuum reclaims free pages (used after large deletions).
func (s *Store) Vacuum(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.w.ExecContext(ctx, `VACUUM`)
	return err
}

// ClearHistory deletes all stored telemetry. The sequence counter is kept so
// cursors never go backwards; clients get resync_required.
func (s *Store) ClearHistory(ctx context.Context) error {
	s.mu.Lock()
	cutoff := time.Now().UnixMicro()
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	for _, q := range []string{`DELETE FROM span_resources`, `DELETE FROM spans`, `DELETE FROM events`, `DELETE FROM resources`, `DELETE FROM sessions`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			tx.Rollback()
			s.mu.Unlock()
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('cleared_before_us', ?), ('generation', '1')
ON CONFLICT(key) DO UPDATE SET value = CASE excluded.key WHEN 'generation' THEN CAST(meta.value AS INTEGER) + 1 ELSE max(CAST(meta.value AS INTEGER), CAST(excluded.value AS INTEGER)) END`, cutoff); err != nil {
		tx.Rollback()
		s.mu.Unlock()
		return err
	}
	err = tx.Commit()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	_ = s.Vacuum(ctx)
	return s.Checkpoint(ctx)
}

// GetMeta reads a meta value.
func (s *Store) GetMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.r.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta writes a meta value.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.w.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Generation is a counter bumped whenever stored history is deleted (clear,
// retention). Clients holding state from an older generation must resync.
func (s *Store) Generation(ctx context.Context) int64 {
	v, _ := s.GetMeta(ctx, "generation")
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

// BumpGeneration increments the history generation.
func (s *Store) BumpGeneration(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	err := s.w.QueryRowContext(ctx, `INSERT INTO meta (key, value) VALUES ('generation', '1')
ON CONFLICT(key) DO UPDATE SET value = CAST(meta.value AS INTEGER) + 1 RETURNING CAST(value AS INTEGER)`).Scan(&n)
	return n, err
}

// SpansTouchedAt returns spans of a session last updated by event seq (e.g. all
// spans closed by a session.ended), for reconnect catch-up.
func (s *Store) SpansTouchedAt(ctx context.Context, sessionID string, seq int64) ([]correlate.Span, error) {
	return s.querySpans(s.r, `SELECT `+spanCols+` FROM spans WHERE session_id = ? AND (last_seq = ? OR end_seq = ? OR start_seq = ?)`, sessionID, seq, seq, seq)
}

// ReplayEvents returns the oldest 100,000 session events in sequence order and
// indicates whether further events were omitted.
func (s *Store) ReplayEvents(ctx context.Context, sessionID string) ([]StoredEvent, bool, error) {
	const limit = 100000
	events, err := s.queryEvents(ctx, `SELECT seq, session_id, span_id, metadata_json FROM events WHERE session_id = ? ORDER BY seq LIMIT ?`, sessionID, limit+1)
	if len(events) > limit {
		return events[:limit], true, err
	}
	return events, false, err
}

// LastEventByHarness returns, per harness and source hook event, the most
// recent occurrence (used by `atlas doctor` to compare configured vs observed).
func (s *Store) LastEventByHarness(ctx context.Context) (map[string]map[string]int64, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT harness, COALESCE(json_extract(metadata_json, '$.source_event'), type), MAX(occurred_at) FROM events GROUP BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int64{}
	for rows.Next() {
		var h, src string
		var ts int64
		if err := rows.Scan(&h, &src, &ts); err != nil {
			return nil, err
		}
		if out[h] == nil {
			out[h] = map[string]int64{}
		}
		out[h][src] = ts
	}
	return out, rows.Err()
}
