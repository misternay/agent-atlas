package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentatlas/internal/correlate"
	"agentatlas/internal/event"
)

func mk(i int, typ, call string) event.Event {
	return event.Event{
		SchemaVersion: 1, EventID: "e" + correlate.Itoa(int64(i)), Timestamp: time.Date(2026, 11, 23, 10, 0, i, 0, time.UTC),
		HostID: "h", Harness: event.HarnessClaude, SessionID: "S1", TurnID: "T1", ToolCallID: call, Type: typ,
		Evidence: event.EvidenceObserved, Privacy: event.PrivacyMetadataOnly, Workspace: "~/w",
		Tool: func() *event.Tool {
			if call == "" {
				return nil
			}
			return &event.Tool{Name: "Read", Category: "file"}
		}(),
		Resources: func() []event.Resource {
			if typ != event.TypeToolStarted {
				return nil
			}
			return []event.Resource{{Kind: "file", Label: "a.go", Relation: "reads", Evidence: event.EvidenceObserved}}
		}(),
	}
}

func TestCommitDedupRestartAndQueries(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "atlas.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	proj := correlate.New(st)
	batch := []event.Event{mk(0, event.TypeSessionStarted, ""), mk(1, event.TypeTurnStarted, ""), mk(2, event.TypeToolStarted, "A"), mk(3, event.TypeToolStarted, "B")}
	ds, err := st.Commit(ctx, batch, proj)
	if err != nil || len(ds) != 4 {
		t.Fatalf("commit: %v %d", err, len(ds))
	}
	// duplicates are ignored
	ds, err = st.Commit(ctx, batch[2:], proj)
	if err != nil || len(ds) != 0 {
		t.Fatalf("dup commit: %v %d", err, len(ds))
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("db perms %v", fi.Mode().Perm())
	}
	st.Close()

	// restart: fresh projector backed by the reopened store
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	proj = correlate.New(st)
	ds, err = st.Commit(ctx, []event.Event{mk(4, event.TypeToolCompleted, "A"), mk(5, event.TypeSessionEnded, "")}, proj)
	if err != nil || len(ds) != 2 {
		t.Fatalf("post-restart commit: %v", err)
	}
	sess, total, err := st.ListSessions(ctx, SessionFilter{})
	if err != nil || total != 1 {
		t.Fatalf("sessions %v %d", err, total)
	}
	s := sess[0]
	if s.Status != "ended" || s.ToolCount != 2 || s.UnknownCount != 1 {
		t.Fatalf("session after restart %+v", s)
	}
	spans, links, err := st.TraceSpans(ctx, s.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, sp := range spans {
		statuses[sp.ToolCallID+"/"+sp.Kind] = sp.Status
	}
	if statuses["A/tool"] != event.StatusCompleted || statuses["B/tool"] != event.StatusUnknown || statuses["/session"] != event.StatusCompleted {
		t.Fatalf("statuses %v", statuses)
	}
	if len(links) != 2 {
		t.Fatalf("links %d", len(links))
	}
	res, total, err := st.SearchSpans(ctx, SpanFilter{Status: event.StatusUnknown})
	if err != nil || total != 1 || res[0].ToolCallID != "B" {
		t.Fatalf("search %v %d", err, total)
	}
	res, total, _ = st.SearchSpans(ctx, SpanFilter{Resource: "a.go", Evidence: event.EvidenceObserved, Harness: "claude"})
	if total != 2 {
		t.Fatalf("resource search %d", total)
	}
	evs, err := st.SessionEvents(ctx, s.ID, 2, 100)
	if err != nil || len(evs) != 4 || evs[0].Seq != 3 {
		t.Fatalf("events after cursor: %v %d", err, len(evs))
	}
	snap, err := st.LiveSnapshot(ctx, 0, 0, "", 100)
	if err != nil || snap.Cursor != 6 || len(snap.Sessions) != 1 {
		t.Fatalf("snapshot %v %+v", err, snap)
	}
	pr, err := st.PruneBefore(ctx, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC).UnixMicro())
	if err != nil || pr.Sessions != 1 || pr.Events != 6 {
		t.Fatalf("prune %v %+v", err, pr)
	}
	mn, mx, _ := st.SeqBounds(ctx)
	if mn != 0 || mx != 6 {
		t.Fatalf("seq bounds after prune %d %d", mn, mx)
	}
}

func TestPruneOldestSessionsWithFewSessions(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Commit(ctx, []event.Event{mk(0, event.TypeSessionStarted, ""), mk(1, event.TypeToolStarted, "A")}, correlate.New(st)); err != nil {
		t.Fatal(err)
	}
	if pr, _ := st.PruneOldestSessions(ctx, 25, time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC).UnixMicro()); pr.Sessions != 0 {
		t.Fatal("recently active session must be protected")
	}
	pr, err := st.PruneOldestSessions(ctx, 25, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).UnixMicro())
	if err != nil || pr.Sessions != 1 {
		t.Fatalf("size-cap prune with 1 session: %v %+v", err, pr)
	}
}

func TestClearHistoryRejectsOlderEventsAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "atlas.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	old := mk(1, event.TypeToolStarted, "old")
	old.Timestamp = time.Now().Add(-time.Second)
	if err := st.ClearHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fresh := mk(2, event.TypeToolStarted, "fresh")
	fresh.Timestamp = time.Now()
	deltas, err := st.Commit(ctx, []event.Event{old, fresh}, correlate.New(st))
	if err != nil || len(deltas) != 1 || deltas[0].Event.EventID != fresh.EventID || st.Generation(ctx) != 1 {
		t.Fatalf("clear cutoff did not survive restart: %+v %v", deltas, err)
	}
}
