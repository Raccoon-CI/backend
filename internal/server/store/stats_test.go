package store

import (
	"context"
	"testing"
)

func TestRunStatsCountsByStatusAndConclusionAndFindsTheOldestQueued(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if st, err := s.RunStats(ctx); err != nil || len(st.Counts) != 0 || !st.OldestQueued.IsZero() {
		t.Fatalf("empty database: %+v, %v", st, err)
	}
	a, b, c := newRun(t, s, "a"), newRun(t, s, "b"), newRun(t, s, "c")
	_ = b
	_ = c
	first, ok, err := s.ClaimRun(ctx)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.FinishRun(ctx, first.ID, "failure"); err != nil {
		t.Fatal(err)
	}
	st, err := s.RunStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Counts[RunQueued][""] != 2 || st.Counts[RunCompleted]["failure"] != 1 {
		t.Errorf("counts: %+v", st.Counts)
	}
	if st.OldestQueued.IsZero() || st.OldestQueued.Before(a.CreatedAt) {
		t.Errorf("oldest queued: %v", st.OldestQueued)
	}
}
