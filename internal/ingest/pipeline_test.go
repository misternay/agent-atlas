package ingest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"agentatlas/internal/event"
	"agentatlas/internal/storage/sqlite"
)

// A commit failure must keep the pending gap count and add the lost events.
func TestCommitFailurePreservesGapCount(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	p := NewPipeline(st, "h", nil, 16, nil)
	p.RecordGap(100, "shim_drop")
	st.Close() // force the next commit to fail
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	evs := []event.Event{}
	for i := 0; i < 5; i++ {
		evs = append(evs, event.Event{SchemaVersion: 1, EventID: "e" + string(rune('a'+i)), Timestamp: time.Now(), HostID: "h",
			Harness: event.HarnessClaude, SessionID: "S", Type: event.TypeToolStarted, Evidence: event.EvidenceObserved, Privacy: event.PrivacyMetadataOnly})
	}
	p.SubmitWait(ctx, evs)
	deadline := time.Now().Add(3 * time.Second)
	for p.Stats.CommitErrors.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if p.Stats.CommitErrors.Load() == 0 {
		t.Fatal("expected a commit failure")
	}
	// 100 earlier drops + 5 lost events must still be pending (not just 6).
	if got := p.pendingGap.Load(); got < 105 {
		t.Fatalf("pending gap %d, want >= 105", got)
	}
}

func TestCommitFailureBacksOff(t *testing.T) {
	st, _ := sqlite.Open(filepath.Join(t.TempDir(), "a.db"))
	p := NewPipeline(st, "h", nil, 16, nil)
	p.RecordGap(1, "x")
	st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	time.Sleep(600 * time.Millisecond) // ~24 ticks
	cancel()
	<-done
	if n := p.Stats.CommitErrors.Load(); n > 5 {
		t.Fatalf("no backoff: %d commit attempts in 600ms", n)
	}
}
