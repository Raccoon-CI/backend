package store

import (
	"context"
	"testing"
)

func TestClaimRunIsFairBetweenRepositoriesAndFifoWithinOne(t *testing.T) {
	s, ctx := open(t), context.Background()
	mk := func(delivery, repo, wf string) Run {
		ev, _, err := s.RecordEvent(ctx, Event{DeliveryID: delivery, Kind: "push", Repo: repo, Payload: []byte("{}")})
		if err != nil {
			t.Fatal(err)
		}
		r, _, err := s.CreateRun(ctx, Run{EventID: ev.ID, Repo: repo, WorkflowPath: wf, Event: "push", SHA: "s", WorkflowText: "x"})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	a1 := mk("a1", "acme/big", "w1")
	a2 := mk("a2", "acme/big", "w2")
	a3 := mk("a3", "Acme/Big", "w3") // the same repository in another case
	b1 := mk("b1", "other/small", "w1")
	c1 := mk("c1", "third/one", "w1")
	claim := func() int64 {
		r, ok, err := s.ClaimRun(ctx)
		if err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		return r.ID
	}
	// The oldest first; then the repositories with nothing running go before a
	// second run of the one that is already running.
	got := []int64{claim(), claim(), claim(), claim(), claim()}
	want := []int64{a1.ID, b1.ID, c1.ID, a2.ID, a3.ID}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	if _, ok, _ := s.ClaimRun(ctx); ok {
		t.Fatal("a sixth run from nowhere")
	}
}

func TestOneRepositoryAloneGetsAllItsRunsInOrder(t *testing.T) {
	s, ctx := open(t), context.Background()
	var ids []int64
	for i := 0; i < 4; i++ {
		ev, _, _ := s.RecordEvent(ctx, Event{DeliveryID: string(rune('a' + i)), Kind: "push", Repo: "solo/repo", Payload: []byte("{}")})
		r, _, _ := s.CreateRun(ctx, Run{EventID: ev.ID, Repo: "solo/repo", WorkflowPath: "w", Event: "push", SHA: "s", WorkflowText: "x"})
		ids = append(ids, r.ID)
	}
	for i, want := range ids {
		r, ok, err := s.ClaimRun(ctx)
		if err != nil || !ok || r.ID != want {
			t.Fatalf("claim %d: %d %v %v, want %d", i, r.ID, ok, err, want)
		}
	}
}
