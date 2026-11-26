package correlate

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"agentatlas/internal/event"
)

var t0 = time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

func ev(i int, typ, call string, mut ...func(*event.Event)) event.Event {
	e := event.Event{
		SchemaVersion: 1, EventID: "e" + Itoa(int64(i)), Timestamp: t0.Add(time.Duration(i) * time.Second),
		HostID: "h", Harness: event.HarnessClaude, SessionID: "S1", TurnID: "T1", ToolCallID: call,
		Type: typ, Evidence: event.EvidenceObserved, Privacy: event.PrivacyMetadataOnly,
	}
	if call != "" {
		e.Tool = &event.Tool{Name: "Bash", Category: "shell"}
	}
	for _, m := range mut {
		m(&e)
	}
	return e
}

func spanOf(p *Projector, call string) *Span {
	return p.span(ToolSpanID(SessionID("h", event.HarnessClaude, "S1"), call))
}

func TestConcurrentCallsPairByID(t *testing.T) {
	p := New(nil)
	seq := int64(0)
	apply := func(e event.Event) Delta { seq++; return p.Apply(seq, e) }
	apply(ev(0, event.TypeSessionStarted, ""))
	apply(ev(1, event.TypeTurnStarted, ""))
	apply(ev(2, event.TypeToolStarted, "A"))
	apply(ev(3, event.TypeToolStarted, "B"))
	apply(ev(4, event.TypeToolCompleted, "B", func(e *event.Event) { e.Status = event.StatusCompleted }))
	apply(ev(6, event.TypeToolFailed, "A", func(e *event.Event) { e.Status = event.StatusFailed }))
	a, b := spanOf(p, "A"), spanOf(p, "B")
	if a.Status != event.StatusFailed || b.Status != event.StatusCompleted {
		t.Fatalf("A=%s B=%s", a.Status, b.Status)
	}
	if *a.DurationUS != 4_000_000 || *b.DurationUS != 1_000_000 {
		t.Fatalf("durations A=%d B=%d", *a.DurationUS, *b.DurationUS)
	}
	if a.ParentSpanID != TurnSpanID(a.SessionID, "T1") {
		t.Fatalf("parent must be the turn span")
	}
	s := p.session(a.SessionID)
	if s.ToolCount != 2 || s.ErrorCount != 1 {
		t.Fatalf("counts %+v", s)
	}
}

func TestMissingCompletionBecomesUnknownNotFailed(t *testing.T) {
	p := New(nil)
	p.Apply(1, ev(0, event.TypeSessionStarted, ""))
	p.Apply(2, ev(1, event.TypeToolStarted, "A"))
	d := p.Apply(3, ev(9, event.TypeSessionEnded, ""))
	a := spanOf(p, "A")
	if a.Status != event.StatusUnknown || a.StatusEvidence != event.EvidenceDerived {
		t.Fatalf("got %s/%s", a.Status, a.StatusEvidence)
	}
	if d.Session.Status != "ended" || d.Session.ErrorCount != 0 || d.Session.UnknownCount != 1 {
		t.Fatalf("session %+v", d.Session)
	}
}

func TestStaleIsNotTerminalAndLateCompletionWins(t *testing.T) {
	p := New(nil)
	p.Apply(1, ev(0, event.TypeToolStarted, "A"))
	p.Apply(2, ev(700, event.TypeToolStale, "A", func(e *event.Event) { e.Evidence = event.EvidenceDerived }))
	if spanOf(p, "A").Status != event.StatusStale {
		t.Fatal("expected stale")
	}
	p.Apply(3, ev(800, event.TypeToolCompleted, "A"))
	if spanOf(p, "A").Status != event.StatusCompleted {
		t.Fatal("late completion must override stale")
	}
}

func TestEvidencePrecedence(t *testing.T) {
	p := New(nil)
	p.Apply(1, ev(0, event.TypeToolStarted, "A"))
	// batch report arrives first (derived unknown), then the observed completion
	p.Apply(2, ev(1, event.TypeToolBatchReported, "A", func(e *event.Event) { e.Status = event.StatusUnknown; e.Evidence = event.EvidenceDerived }))
	p.Apply(3, ev(2, event.TypeToolCompleted, "A"))
	if sp := spanOf(p, "A"); sp.Status != event.StatusCompleted || sp.StatusEvidence != event.EvidenceObserved {
		t.Fatalf("observed must win: %s/%s", sp.Status, sp.StatusEvidence)
	}
	// a later weaker signal must not override
	p.Apply(4, ev(3, event.TypeToolBatchReported, "A", func(e *event.Event) { e.Status = event.StatusDenied; e.Evidence = event.EvidenceInferred }))
	if sp := spanOf(p, "A"); sp.Status != event.StatusCompleted {
		t.Fatalf("weaker evidence overrode: %s", sp.Status)
	}
	if s := p.session(spanOf(p, "A").SessionID); s.UnknownCount != 0 || s.DeniedCount != 0 {
		t.Fatalf("counter drift %+v", s)
	}
}

func TestTerminalBeforeStartAndDuplicates(t *testing.T) {
	p := New(nil)
	dur := int64(1500)
	p.Apply(1, ev(10, event.TypeToolCompleted, "A", func(e *event.Event) { e.DurationMS = &dur }))
	sp := spanOf(p, "A")
	if sp.Attrs["start_missing"] != "true" || sp.StartedAt != t0.Add(10*time.Second).UnixMicro()-1_500_000 {
		t.Fatalf("derived start wrong: %+v", sp)
	}
	p.Apply(2, ev(9, event.TypeToolStarted, "A"))
	p.Apply(3, ev(9, event.TypeToolStarted, "A"))
	sp = spanOf(p, "A")
	if sp.Status != event.StatusCompleted || sp.Attrs["start_missing"] != "" {
		t.Fatalf("late start must not reopen: %+v", sp)
	}
	if s := p.session(sp.SessionID); s.ToolCount != 1 {
		t.Fatalf("duplicate starts double counted: %d", s.ToolCount)
	}
}

func TestLinksEvidenceUpgradeOnly(t *testing.T) {
	p := New(nil)
	inf := []event.Resource{{Kind: "file", Label: "a.go", Relation: "reads", Evidence: event.EvidenceInferred}}
	obs := []event.Resource{{Kind: "file", Label: "a.go", Relation: "reads", Evidence: event.EvidenceObserved}}
	d1 := p.Apply(1, ev(0, event.TypeToolStarted, "A", func(e *event.Event) { e.Resources = inf }))
	d2 := p.Apply(2, ev(1, event.TypeToolCompleted, "A", func(e *event.Event) { e.Resources = obs }))
	d3 := p.Apply(3, ev(2, event.TypeToolCompleted, "A", func(e *event.Event) { e.Resources = inf }))
	if len(d1.Links) != 1 || len(d2.Links) != 1 || d2.Links[0].Evidence != event.EvidenceObserved || len(d3.Links) != 0 {
		t.Fatalf("links d1=%v d2=%v d3=%v", d1.Links, d2.Links, d3.Links)
	}
}

func TestReplayDeterministic(t *testing.T) {
	events := []event.Event{
		ev(0, event.TypeSessionStarted, ""), ev(1, event.TypeTurnStarted, ""), ev(2, event.TypeToolStarted, "A"),
		ev(3, event.TypeToolStarted, "B"), ev(4, event.TypeToolCompleted, "A"), ev(5, event.TypeTurnEnded, ""),
		ev(6, event.TypeSessionEnded, ""),
	}
	run := func() []byte {
		p := New(nil)
		var ds []Delta
		for i, e := range events {
			ds = append(ds, p.Apply(int64(i+1), e))
		}
		b, _ := json.Marshal(ds)
		return b
	}
	a, b := run(), run()
	if !reflect.DeepEqual(a, b) {
		t.Fatal("replay not deterministic")
	}
}

func TestLateSpooledEventsDoNotReopenOrWronglyClose(t *testing.T) {
	p := New(nil)
	p.Apply(1, ev(5, event.TypeSessionEnded, ""))
	d := p.Apply(2, ev(0, event.TypeSessionStarted, "")) // older start replayed late
	if d.Session.Status != "ended" || d.Session.EndedAt == nil {
		t.Fatalf("late start reopened session: %+v", d.Session)
	}
	q := New(nil)
	q.Apply(1, ev(0, event.TypeSessionStarted, ""))
	q.Apply(2, ev(10, event.TypeToolStarted, "NEW"))
	q.Apply(3, ev(2, event.TypeSessionEnded, "")) // old end applied late
	sp := q.span(ToolSpanID(SessionID("h", event.HarnessClaude, "S1"), "NEW"))
	if sp.Status != event.StatusRunning {
		t.Fatalf("tool started after the end was closed by it: %s", sp.Status)
	}
	// a genuinely newer start resumes
	r := New(nil)
	r.Apply(1, ev(0, event.TypeSessionStarted, ""))
	r.Apply(2, ev(5, event.TypeSessionEnded, ""))
	if d := r.Apply(3, ev(9, event.TypeSessionStarted, "")); d.Session.Status != "active" {
		t.Fatalf("resume not applied: %+v", d.Session)
	}
}

func TestImplicitTurnSpanIsEmittedAndDeltasAreImmutable(t *testing.T) {
	p := New(nil)
	d := p.Apply(1, ev(0, event.TypeToolStarted, "A"))
	var turn bool
	for _, sp := range d.Spans {
		if sp.Kind == KindTurn {
			turn = true
		}
	}
	if !turn {
		t.Fatal("implicitly created turn span missing from delta (would never be persisted)")
	}
	s1 := p.Apply(2, ev(1, event.TypeSessionStarted, "", func(e *event.Event) { e.Attrs = map[string]string{"source": "startup"} }))
	before := s1.Session.Attrs["end_reason"]
	p.Apply(3, ev(2, event.TypeSessionEnded, "", func(e *event.Event) { e.Attrs = map[string]string{"reason": "logout"} }))
	if s1.Session.Attrs["end_reason"] != before {
		t.Fatal("earlier delta was mutated by a later Apply")
	}
}

func TestDurationRepairedWhenStartArrivesLate(t *testing.T) {
	p := New(nil)
	p.Apply(1, ev(5, event.TypeToolCompleted, "A"))
	p.Apply(2, ev(2, event.TypeToolStarted, "A"))
	if sp := spanOf(p, "A"); *sp.DurationUS != 3_000_000 {
		t.Fatalf("duration %d", *sp.DurationUS)
	}
	p.Apply(3, ev(9, event.TypeTurnEnded, ""))
	turn := p.span(TurnSpanID(spanOf(p, "A").SessionID, "T1"))
	p.Apply(4, ev(1, event.TypeTurnStarted, ""))
	if *turn.DurationUS != 8_000_000 {
		t.Fatalf("turn duration %d", *turn.DurationUS)
	}
}

func TestObservedTurnEndBeatsDerivedUnknown(t *testing.T) {
	p := New(nil)
	p.Apply(1, ev(0, event.TypeTurnStarted, ""))
	p.Apply(2, ev(3, event.TypeSessionEnded, ""))
	p.Apply(3, ev(2, event.TypeTurnEnded, ""))
	turn := p.span(TurnSpanID(SessionID("h", event.HarnessClaude, "S1"), "T1"))
	if turn.Status != event.StatusCompleted || turn.StatusEvidence != event.EvidenceObserved {
		t.Fatalf("turn %s/%s", turn.Status, turn.StatusEvidence)
	}
}

func TestLateEndDoesNotRewindOrCloseResumedSession(t *testing.T) {
	p := New(nil)
	p.Apply(1, ev(0, event.TypeSessionStarted, ""))
	p.Apply(2, ev(20, event.TypeSessionEnded, "", func(e *event.Event) { e.Attrs = map[string]string{"reason": "logout"} }))
	d := p.Apply(3, ev(5, event.TypeSessionEnded, "", func(e *event.Event) { e.Attrs = map[string]string{"reason": "clear"} }))
	if *d.Session.EndedAt != t0.Add(20*time.Second).UnixMicro() || d.Session.Attrs["end_reason"] != "logout" {
		t.Fatalf("late end rewound session: %+v", d.Session)
	}
	q := New(nil)
	q.Apply(1, ev(0, event.TypeSessionStarted, ""))
	q.Apply(2, ev(10, event.TypeSessionStarted, "")) // resume
	q.Apply(3, ev(11, event.TypeToolStarted, "t1"))
	if d := q.Apply(4, ev(5, event.TypeSessionEnded, "")); d.Session.Status != "active" {
		t.Fatalf("old end closed a resumed session: %+v", d.Session)
	}
}

func TestLateStartRecomputesRootDuration(t *testing.T) {
	p := New(nil)
	p.Apply(1, ev(10, event.TypeToolStarted, "A"))
	p.Apply(2, ev(20, event.TypeSessionEnded, ""))
	d := p.Apply(3, ev(0, event.TypeSessionStarted, ""))
	for _, sp := range d.Spans {
		if sp.Kind == KindSession && *sp.DurationUS != *sp.EndedAt-sp.StartedAt {
			t.Fatalf("root duration %d != end-start %d", *sp.DurationUS, *sp.EndedAt-sp.StartedAt)
		}
	}
}
