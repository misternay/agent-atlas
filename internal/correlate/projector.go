// Package correlate projects sanitized events into sessions, spans and
// resource links (design §6). The Projector is deterministic: it depends only
// on the ordered event stream (and committed rows loaded on cache miss), so the
// live daemon and the replay endpoint share it.
package correlate

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"sort"
	"strconv"

	"agentatlas/internal/event"
)

// Session is the materialized session row.
type Session struct {
	ID              string            `json:"id"`
	HostID          string            `json:"host_id"`
	Harness         string            `json:"harness"`
	SourceSessionID string            `json:"source_session_id"`
	Workspace       string            `json:"workspace"`
	Status          string            `json:"status"` // active | ended
	StartObserved   bool              `json:"start_observed"`
	StartedAt       int64             `json:"started_at_us"`
	EndedAt         *int64            `json:"ended_at_us,omitempty"`
	LastEventAt     int64             `json:"last_event_at_us"`
	TraceID         string            `json:"trace_id"`
	RootSpanID      string            `json:"root_span_id"`
	ToolCount       int64             `json:"tool_count"`
	ErrorCount      int64             `json:"error_count"`
	DeniedCount     int64             `json:"denied_count"`
	UnknownCount    int64             `json:"unknown_count"`
	Model           string            `json:"model,omitempty"`
	Attrs           map[string]string `json:"attrs,omitempty"`
}

// Span kinds.
const (
	KindSession = "session"
	KindTurn    = "turn"
	KindTool    = "tool"
)

// Span is the materialized span row.
type Span struct {
	ID             string            `json:"id"`
	TraceID        string            `json:"trace_id"`
	SessionID      string            `json:"session_id"`
	ParentSpanID   string            `json:"parent_span_id,omitempty"`
	Kind           string            `json:"kind"`
	Name           string            `json:"name"`
	ToolCategory   string            `json:"tool_category,omitempty"`
	Operation      string            `json:"operation,omitempty"`
	TargetKind     string            `json:"target_kind,omitempty"`
	TargetLabel    string            `json:"target_label,omitempty"`
	ToolCallID     string            `json:"tool_call_id,omitempty"`
	TurnID         string            `json:"turn_id,omitempty"`
	Status         string            `json:"status"`
	StatusEvidence string            `json:"status_evidence"`
	OutcomeBasis   string            `json:"outcome_basis,omitempty"`
	StartedAt      int64             `json:"started_at_us"`
	EndedAt        *int64            `json:"ended_at_us,omitempty"`
	DurationUS     *int64            `json:"duration_us,omitempty"`
	StartSeq       int64             `json:"start_seq,omitempty"`
	EndSeq         int64             `json:"end_seq,omitempty"`
	LastSeq        int64             `json:"last_seq"`
	Attrs          map[string]string `json:"attrs,omitempty"`
}

// Link connects a span to a resource.
type Link struct {
	SpanID     string `json:"span_id"`
	ResourceID string `json:"resource_id"`
	Kind       string `json:"kind"`
	Label      string `json:"label"`
	Relation   string `json:"relation"`
	Evidence   string `json:"evidence"`
	EventSeq   int64  `json:"event_seq"`
}

// Delta is the result of applying one event: rows that changed.
type Delta struct {
	Seq     int64       `json:"seq"`
	Event   event.Event `json:"event"`
	Session *Session    `json:"session,omitempty"`
	Spans   []Span      `json:"spans,omitempty"`
	Links   []Link      `json:"links,omitempty"`
	SpanID  string      `json:"span_id,omitempty"` // primary span the event applied to
}

// Loader fetches committed rows on cache miss. A nil Loader means "empty
// history" (used by replay).
type Loader interface {
	LoadSession(id string) (*Session, error)
	LoadSpan(id string) (*Span, error)
	LoadOpenSpans(sessionID string) ([]Span, error)
	LoadLinks(spanID string) ([]Link, error)
}

// Projector holds the working set of sessions and spans.
type Projector struct {
	loader   Loader
	sessions map[string]*Session
	spans    map[string]*Span
	open     map[string]map[string]bool // session id -> open (running/stale) span ids
	loaded   map[string]bool            // sessions whose open spans were loaded
	links    map[string]map[string]*Link
}

// New returns a projector. loader may be nil.
func New(loader Loader) *Projector {
	p := &Projector{loader: loader}
	p.Reset()
	return p
}

// Reset drops all cached state (e.g. after a failed transaction).
func (p *Projector) Reset() {
	p.sessions = map[string]*Session{}
	p.spans = map[string]*Span{}
	p.open = map[string]map[string]bool{}
	p.loaded = map[string]bool{}
	p.links = map[string]map[string]*Link{}
}

// Evict drops cached rows for ended sessions and terminal spans so memory stays
// bounded; rows are reloaded from storage on demand.
func (p *Projector) Evict(olderThanUS int64) {
	for id, sp := range p.spans {
		if event.IsTerminal(sp.Status) && sp.LastSeq > 0 && sp.EndedAt != nil && *sp.EndedAt < olderThanUS {
			delete(p.spans, id)
			delete(p.links, id)
		}
	}
	for id, s := range p.sessions {
		if s.LastEventAt < olderThanUS {
			delete(p.sessions, id)
			delete(p.open, id)
			delete(p.loaded, id)
		}
	}
}

func hash(parts ...string) string {
	h := sha256.New()
	for _, s := range parts {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SessionID returns the deterministic session row id.
func SessionID(hostID, harness, sourceSessionID string) string {
	return "s_" + hash(hostID, harness, sourceSessionID)[:20]
}

// TraceID returns the W3C 128-bit trace id for a session.
func TraceID(sessionID string) string { return hash("trace", sessionID)[:32] }

func rootSpanID(sessionID string) string { return hash("root", sessionID)[:16] }

// TurnSpanID returns the span id for a turn.
func TurnSpanID(sessionID, turnID string) string { return hash("turn", sessionID, turnID)[:16] }

// ToolSpanID returns the span id for a tool call.
func ToolSpanID(sessionID, toolCallID string) string { return hash("tool", sessionID, toolCallID)[:16] }

// ResourceID returns the deterministic resource id.
func ResourceID(kind, label string) string { return "r_" + hash(kind, label)[:20] }

func evidenceRank(e string) int {
	switch e {
	case event.EvidenceObserved:
		return 3
	case event.EvidenceDerived:
		return 2
	case event.EvidenceInferred:
		return 1
	}
	return 0
}

func us(e event.Event) int64 { return e.Timestamp.UnixMicro() }

func (p *Projector) session(id string) *Session {
	if s, ok := p.sessions[id]; ok {
		return s
	}
	if p.loader != nil {
		if s, err := p.loader.LoadSession(id); err == nil && s != nil {
			p.sessions[id] = s
			return s
		}
	}
	return nil
}

func (p *Projector) span(id string) *Span {
	if s, ok := p.spans[id]; ok {
		return s
	}
	if p.loader != nil {
		if s, err := p.loader.LoadSpan(id); err == nil && s != nil {
			p.spans[id] = s
			return s
		}
	}
	return nil
}

func (p *Projector) ensureOpenLoaded(sessionID string) {
	if p.loaded[sessionID] {
		return
	}
	p.loaded[sessionID] = true
	if p.open[sessionID] == nil {
		p.open[sessionID] = map[string]bool{}
	}
	if p.loader == nil {
		return
	}
	spans, err := p.loader.LoadOpenSpans(sessionID)
	if err != nil {
		return
	}
	for i := range spans {
		sp := spans[i]
		if cur, ok := p.spans[sp.ID]; ok {
			if !event.IsTerminal(cur.Status) {
				p.open[sessionID][sp.ID] = true
			}
			continue
		}
		p.spans[sp.ID] = &sp
		p.open[sessionID][sp.ID] = true
	}
}

func (p *Projector) markOpen(sp *Span) {
	if p.open[sp.SessionID] == nil {
		p.open[sp.SessionID] = map[string]bool{}
	}
	if event.IsTerminal(sp.Status) {
		delete(p.open[sp.SessionID], sp.ID)
	} else {
		p.open[sp.SessionID][sp.ID] = true
	}
}

type tracker struct {
	spans map[string]*Span
	order []string
	links []Link
}

func (t *tracker) touch(sp *Span) {
	if t.spans == nil {
		t.spans = map[string]*Span{}
	}
	if _, ok := t.spans[sp.ID]; !ok {
		t.order = append(t.order, sp.ID)
	}
	t.spans[sp.ID] = sp
}

// Apply projects one committed event (with its storage sequence number).
func (p *Projector) Apply(seq int64, e event.Event) Delta {
	d := Delta{Seq: seq, Event: e}
	if e.Type == event.TypeTelemetryGap || e.SessionID == "" {
		return d
	}
	t := &tracker{}
	ts := us(e)
	sid := SessionID(e.HostID, e.Harness, e.SessionID)
	s := p.session(sid)
	if s == nil {
		s = &Session{
			ID: sid, HostID: e.HostID, Harness: e.Harness, SourceSessionID: e.SessionID,
			Workspace: e.Workspace, Status: "active", StartedAt: ts, LastEventAt: ts,
			TraceID: TraceID(sid), RootSpanID: rootSpanID(sid),
		}
		p.sessions[sid] = s
		root := &Span{
			ID: s.RootSpanID, TraceID: s.TraceID, SessionID: sid, Kind: KindSession,
			Name: e.Harness + " session", Status: event.StatusRunning, StatusEvidence: event.EvidenceObserved,
			StartedAt: ts, StartSeq: seq, LastSeq: seq,
		}
		if e.Type != event.TypeSessionStarted {
			root.StatusEvidence = event.EvidenceDerived
			root.Attrs = map[string]string{"start_missing": "true"}
		}
		p.spans[root.ID] = root
		p.loaded[sid] = true // brand new session: nothing to load
		p.markOpen(root)
		t.touch(root)
	}
	if ts > s.LastEventAt {
		s.LastEventAt = ts
	}
	if ts < s.StartedAt {
		s.StartedAt = ts
	}
	if s.Workspace == "" && e.Workspace != "" {
		s.Workspace = e.Workspace
	}
	if m := e.Attrs["model"]; m != "" {
		s.Model = m
	}

	switch e.Type {
	case event.TypeSessionStarted:
		s.StartObserved = true
		// Resume only if this start is newer than the recorded end; an older
		// start replayed late from the spool must not reopen an ended session.
		resumed := s.Status == "ended" && s.EndedAt != nil && ts > *s.EndedAt
		if resumed {
			s.Status = "active"
			s.EndedAt = nil
		}
		if s.Attrs == nil {
			s.Attrs = map[string]string{}
		}
		if prev, _ := strconv.ParseInt(s.Attrs["last_start_us"], 10, 64); ts > prev {
			s.Attrs["last_start_us"] = strconv.FormatInt(ts, 10)
		}
		if src := e.Attrs["source"]; src != "" {
			if s.Attrs == nil {
				s.Attrs = map[string]string{}
			}
			s.Attrs["source"] = src
		}
		if root := p.span(s.RootSpanID); root != nil {
			if root.Attrs["start_missing"] == "true" {
				delete(root.Attrs, "start_missing")
				root.StatusEvidence = event.EvidenceObserved
			}
			if ts < root.StartedAt {
				root.StartedAt = ts
				if root.EndedAt != nil && event.IsTerminal(root.Status) {
					dur := *root.EndedAt - ts
					root.DurationUS = &dur
				}
			}
			if event.IsTerminal(root.Status) && resumed {
				root.Status = event.StatusRunning
				root.EndedAt = nil
				root.DurationUS = nil
			}
			root.LastSeq = seq
			p.markOpen(root)
			t.touch(root)
		}
		d.SpanID = s.RootSpanID

	case event.TypeSessionEnded:
		// A late (spooled) end older than the recorded end, or older than a
		// newer observed (re)start, must not rewind or re-close the session.
		lastStart, _ := strconv.ParseInt(s.Attrs["last_start_us"], 10, 64)
		if (s.EndedAt != nil && *s.EndedAt >= ts) || lastStart > ts {
			d.SpanID = s.RootSpanID
			break
		}
		s.Status = "ended"
		end := ts
		s.EndedAt = &end
		if r := e.Attrs["reason"]; r != "" {
			if s.Attrs == nil {
				s.Attrs = map[string]string{}
			}
			s.Attrs["end_reason"] = r
		}
		p.ensureOpenLoaded(sid)
		ids := make([]string, 0, len(p.open[sid]))
		for id := range p.open[sid] {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			sp := p.span(id)
			if sp == nil || event.IsTerminal(sp.Status) {
				continue
			}
			if sp.Kind != KindSession && sp.StartedAt > ts {
				continue // started after this (late-arriving) end: not ended by it
			}
			if sp.Kind == KindSession {
				p.finish(sp, event.StatusCompleted, event.EvidenceObserved, "session_end_hook", ts, nil, seq, s)
			} else if sp.Kind == KindTurn {
				p.finish(sp, event.StatusUnknown, event.EvidenceDerived, "session_ended_before_turn_end", ts, nil, seq, s)
			} else {
				p.finish(sp, event.StatusUnknown, event.EvidenceDerived, "session_ended_without_completion", ts, nil, seq, s)
			}
			t.touch(sp)
		}
		if root := p.span(s.RootSpanID); root != nil && root.Status != event.StatusCompleted {
			p.finish(root, event.StatusCompleted, event.EvidenceObserved, "session_end_hook", ts, nil, seq, s)
			t.touch(root)
		}
		d.SpanID = s.RootSpanID

	case event.TypeTurnStarted:
		if e.TurnID != "" {
			sp := p.turnSpan(s, e.TurnID, ts, seq, true)
			t.touch(sp)
			d.SpanID = sp.ID
		}

	case event.TypeTurnEnded:
		if e.TurnID != "" {
			sp := p.turnSpan(s, e.TurnID, ts, seq, false)
			if !event.IsTerminal(sp.Status) || evidenceRank(e.Evidence) > evidenceRank(sp.StatusEvidence) {
				status, basis := event.StatusCompleted, "stop_hook"
				if e.Attrs["interrupted"] == "true" {
					status, basis = event.StatusUnknown, "interrupted"
				} else if e.Attrs["outcome_basis"] == "stop_failure" {
					status, basis = event.StatusFailed, "stop_failure"
				}
				p.finish(sp, status, e.Evidence, basis, ts, nil, seq, nil)
			}
			sp.LastSeq = seq
			t.touch(sp)
			d.SpanID = sp.ID
		}

	case event.TypeToolStarted:
		sp, created := p.toolSpan(s, e, ts, seq, t)
		if !created && sp.Attrs["start_missing"] == "true" {
			delete(sp.Attrs, "start_missing")
			sp.StartedAt = ts
			// Recompute unless the harness reported the duration itself.
			if sp.EndedAt != nil && sp.Attrs["duration_reported"] != "true" {
				dur := *sp.EndedAt - ts
				if dur < 0 {
					dur = 0
				}
				sp.DurationUS = &dur
			}
		}
		if sp.StartSeq == 0 {
			sp.StartSeq = seq
		}
		p.fillTool(sp, e)
		sp.LastSeq = seq
		t.touch(sp)
		t.links = append(t.links, p.addLinks(sp, e, seq)...)
		d.SpanID = sp.ID

	case event.TypeToolCompleted, event.TypeToolFailed, event.TypeToolDenied, event.TypeToolBatchReported:
		sp, created := p.toolSpan(s, e, ts, seq, t)
		if created {
			// Terminal before start: derive start from reported duration if any.
			if sp.Attrs == nil {
				sp.Attrs = map[string]string{}
			}
			sp.Attrs["start_missing"] = "true"
			if e.DurationMS != nil {
				sp.StartedAt = ts - *e.DurationMS*1000
			}
		}
		if e.Type != event.TypeToolBatchReported || sp.Name == "" {
			p.fillTool(sp, e)
		}
		status := e.Status
		if status == "" {
			status = map[string]string{event.TypeToolCompleted: event.StatusCompleted, event.TypeToolFailed: event.StatusFailed, event.TypeToolDenied: event.StatusDenied}[e.Type]
		}
		basis := e.Attrs["outcome_basis"]
		if basis == "" {
			basis = map[string]string{event.TypeToolCompleted: "post_tool_hook", event.TypeToolFailed: "failure_signal", event.TypeToolDenied: "denied_signal"}[e.Type]
		}
		var durUS *int64
		if e.DurationMS != nil {
			v := *e.DurationMS * 1000
			durUS = &v
		}
		if status != "" && (!event.IsTerminal(sp.Status) || evidenceRank(e.Evidence) > evidenceRank(sp.StatusEvidence)) {
			p.finish(sp, status, e.Evidence, basis, ts, durUS, seq, s)
			if durUS != nil { // only when the reported duration was actually applied
				if sp.Attrs == nil {
					sp.Attrs = map[string]string{}
				}
				sp.Attrs["duration_reported"] = "true"
			}
		}
		sp.LastSeq = seq
		t.touch(sp)
		t.links = append(t.links, p.addLinks(sp, e, seq)...)
		d.SpanID = sp.ID

	case event.TypeToolStale:
		if e.ToolCallID != "" {
			if sp := p.span(ToolSpanID(sid, e.ToolCallID)); sp != nil && sp.Status == event.StatusRunning {
				sp.Status = event.StatusStale
				sp.StatusEvidence = event.EvidenceDerived
				sp.OutcomeBasis = "no_completion_within_threshold"
				sp.LastSeq = seq
				t.touch(sp)
				d.SpanID = sp.ID
			}
		}

	case event.TypePermissionRequested:
		if e.ToolCallID != "" {
			if sp := p.span(ToolSpanID(sid, e.ToolCallID)); sp != nil {
				if sp.Attrs == nil {
					sp.Attrs = map[string]string{}
				}
				sp.Attrs["permission_requested"] = "true"
				sp.LastSeq = seq
				t.touch(sp)
				d.SpanID = sp.ID
			}
		}
	}

	// Published deltas must be immutable snapshots: the projector keeps
	// mutating its rows (and their maps) while subscribers serialize later.
	cp := *s
	cp.Attrs = maps.Clone(s.Attrs)
	cp.EndedAt = clonePtr(s.EndedAt)
	d.Session = &cp
	for _, id := range t.order {
		c := *t.spans[id]
		c.Attrs = maps.Clone(c.Attrs)
		c.EndedAt = clonePtr(c.EndedAt)
		c.DurationUS = clonePtr(c.DurationUS)
		d.Spans = append(d.Spans, c)
	}
	d.Links = t.links
	return d
}

func (p *Projector) turnSpan(s *Session, turnID string, ts, seq int64, starting bool) *Span {
	id := TurnSpanID(s.ID, turnID)
	sp := p.span(id)
	if sp == nil {
		sp = &Span{
			ID: id, TraceID: s.TraceID, SessionID: s.ID, ParentSpanID: s.RootSpanID, Kind: KindTurn,
			Name: "turn", TurnID: turnID, Status: event.StatusRunning, StatusEvidence: event.EvidenceObserved,
			StartedAt: ts, StartSeq: seq, LastSeq: seq,
		}
		if !starting {
			sp.Attrs = map[string]string{"start_missing": "true"}
			sp.StatusEvidence = event.EvidenceDerived
		}
		p.spans[id] = sp
		p.markOpen(sp)
	} else if starting && sp.Attrs["start_missing"] == "true" {
		delete(sp.Attrs, "start_missing")
		sp.StartedAt = ts
		sp.StartSeq = seq
		if sp.StatusEvidence == event.EvidenceDerived && !event.IsTerminal(sp.Status) {
			sp.StatusEvidence = event.EvidenceObserved
		}
		if sp.EndedAt != nil {
			dur := *sp.EndedAt - ts
			if dur < 0 {
				dur = 0
			}
			sp.DurationUS = &dur
		}
	}
	return sp
}

func (p *Projector) toolSpan(s *Session, e event.Event, ts, seq int64, t *tracker) (*Span, bool) {
	key := e.ToolCallID
	if key == "" {
		key = "event:" + e.EventID // cannot correlate without a call id
	}
	id := ToolSpanID(s.ID, key)
	if sp := p.span(id); sp != nil {
		return sp, false
	}
	parent := s.RootSpanID
	if e.TurnID != "" {
		ps := p.turnSpan(s, e.TurnID, ts, seq, false)
		parent = ps.ID
		if ps.Attrs["start_missing"] == "true" && ps.StartedAt > ts {
			ps.StartedAt = ts
		}
		// Persist/broadcast the (possibly implicitly created) parent turn span.
		t.touch(ps)
	}
	sp := &Span{
		ID: id, TraceID: s.TraceID, SessionID: s.ID, ParentSpanID: parent, Kind: KindTool,
		ToolCallID: e.ToolCallID, TurnID: e.TurnID, Status: event.StatusRunning,
		StatusEvidence: event.EvidenceObserved, StartedAt: ts, LastSeq: seq,
	}
	if e.Type == event.TypeToolStarted {
		sp.StartSeq = seq
	}
	if e.ToolCallID == "" {
		sp.Attrs = map[string]string{"uncorrelated": "true"}
	}
	p.spans[id] = sp
	p.markOpen(sp)
	s.ToolCount++
	return sp, true
}

func (p *Projector) fillTool(sp *Span, e event.Event) {
	if e.Tool != nil {
		if e.Tool.Name != "" {
			sp.Name = e.Tool.Name
		}
		if e.Tool.Category != "" {
			sp.ToolCategory = e.Tool.Category
		}
		if e.Tool.Operation != "" {
			sp.Operation = e.Tool.Operation
		}
		if e.Tool.MCPServer != "" {
			if sp.Attrs == nil {
				sp.Attrs = map[string]string{}
			}
			sp.Attrs["mcp_server"] = e.Tool.MCPServer
			sp.Attrs["mcp_tool"] = e.Tool.MCPTool
		}
	}
	if e.Target != nil && sp.TargetLabel == "" {
		sp.TargetKind = e.Target.Kind
		sp.TargetLabel = e.Target.Label
	}
	if sp.Name == "" {
		sp.Name = "tool"
	}
	if e.AgentID != "" {
		if sp.Attrs == nil {
			sp.Attrs = map[string]string{}
		}
		sp.Attrs["agent_id"] = e.AgentID
	}
}

func (p *Projector) finish(sp *Span, status, evidence, basis string, ts int64, durUS *int64, seq int64, s *Session) {
	prev := sp.Status
	sp.Status = status
	sp.StatusEvidence = evidence
	sp.OutcomeBasis = basis
	end := ts
	if end < sp.StartedAt {
		end = sp.StartedAt
	}
	sp.EndedAt = &end
	if durUS != nil {
		v := *durUS
		sp.DurationUS = &v
	} else {
		v := end - sp.StartedAt
		sp.DurationUS = &v
	}
	sp.EndSeq = seq
	if seq > sp.LastSeq {
		sp.LastSeq = seq
	}
	p.markOpen(sp)
	if s != nil && sp.Kind == KindTool && prev != status {
		adj := func(st string, delta int64) {
			switch st {
			case event.StatusFailed:
				s.ErrorCount += delta
			case event.StatusDenied:
				s.DeniedCount += delta
			case event.StatusUnknown:
				s.UnknownCount += delta
			}
		}
		adj(prev, -1)
		adj(status, 1)
	}
}

func (p *Projector) addLinks(sp *Span, e event.Event, seq int64) []Link {
	if len(e.Resources) == 0 {
		return nil
	}
	if p.links[sp.ID] == nil {
		p.links[sp.ID] = map[string]*Link{}
		if p.loader != nil {
			if ls, err := p.loader.LoadLinks(sp.ID); err == nil {
				for i := range ls {
					l := ls[i]
					p.links[sp.ID][l.ResourceID+"|"+l.Relation] = &l
				}
			}
		}
	}
	var out []Link
	for _, r := range e.Resources {
		rid := ResourceID(r.Kind, r.Label)
		k := rid + "|" + r.Relation
		if cur, ok := p.links[sp.ID][k]; ok {
			if evidenceRank(r.Evidence) <= evidenceRank(cur.Evidence) {
				continue
			}
			cur.Evidence = r.Evidence
			cur.EventSeq = seq
			out = append(out, *cur)
			continue
		}
		l := &Link{SpanID: sp.ID, ResourceID: rid, Kind: r.Kind, Label: r.Label, Relation: r.Relation, Evidence: r.Evidence, EventSeq: seq}
		p.links[sp.ID][k] = l
		out = append(out, *l)
	}
	return out
}

// StaleCandidates returns cached running tool spans that started before cutoff.
func (p *Projector) StaleCandidates(cutoffUS int64) []Span {
	var out []Span
	for _, sp := range p.spans {
		if sp.Kind == KindTool && sp.Status == event.StatusRunning && sp.StartedAt < cutoffUS {
			out = append(out, *sp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func clonePtr(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// Itoa is a small helper used by callers formatting attrs.
func Itoa(n int64) string { return strconv.FormatInt(n, 10) }
