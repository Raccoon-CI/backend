package store

import (
	"context"
	"testing"
)

func TestRunNumbersCountPerWorkflowAndRerunsKeepTheNumberAndRaiseTheAttempt(t *testing.T) {
	s, ctx := open(t), context.Background()
	mk := func(delivery, workflow string, rerunOf int64) Run {
		ev, _, err := s.RecordEvent(ctx, Event{DeliveryID: delivery, Kind: "push", Repo: "o/r", Payload: []byte("{}")})
		if err != nil {
			t.Fatal(err)
		}
		r, _, err := s.CreateRun(ctx, Run{EventID: ev.ID, Repo: "o/r", WorkflowPath: workflow, Event: "push", SHA: "s", WorkflowText: "x", RerunOf: rerunOf})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	ci1 := mk("d1", "ci.yml", 0)
	lint1 := mk("d2", "lint.yml", 0)
	ci2 := mk("d3", "ci.yml", 0)
	rerun := mk("d4", "ci.yml", ci1.ID)
	rerun2 := mk("d5", "ci.yml", ci1.ID)
	ci3 := mk("d6", "ci.yml", 0)
	for _, c := range []struct {
		name            string
		run             Run
		number, attempt int
	}{
		{"first ci", ci1, 1, 1}, {"first lint", lint1, 1, 1}, {"second ci", ci2, 2, 1},
		{"rerun of the first", rerun, 1, 2}, {"second rerun of the first", rerun2, 1, 3}, {"third ci", ci3, 3, 1},
	} {
		n, a, err := s.RunNumbers(ctx, c.run)
		if err != nil || n != c.number || a != c.attempt {
			t.Errorf("%s: number %d attempt %d (%v), want %d and %d", c.name, n, a, err, c.number, c.attempt)
		}
	}
}
