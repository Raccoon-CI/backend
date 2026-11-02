package store

import (
	"context"
	"testing"
	"time"
)

func TestUsageAddsUpPerDayAndRepositoryAndIsSummedByRepoOrOwner(t *testing.T) {
	s, ctx := open(t), context.Background()
	d := func(day int) time.Time { return time.Date(2026, 11, day, 23, 59, 0, 0, time.UTC) }
	s.AddUsage(ctx, "Acme/Web", d(1), 100, 1)
	s.AddUsage(ctx, "acme/web", d(1), 50, 1) // the same repository, the same day
	s.AddUsage(ctx, "acme/web", d(2), 10, 1)
	s.AddUsage(ctx, "acme/api", d(2), 1000, 2)
	s.AddUsage(ctx, "acme_x/web", d(2), 7, 1) // an underscore is not a wildcard
	s.AddUsage(ctx, "other/web", d(2), 5000, 1)
	s.AddUsage(ctx, "acme/web", time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), 99999, 1) // last month

	nov := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for scope, want := range map[string]int64{"acme/web": 160, "ACME/Web": 160, "acme/api": 1000, "acme/*": 1160, "acme_x/*": 7, "other/*": 5000, "nobody/x": 0} {
		if got, err := s.UsedSince(ctx, nov, scope); err != nil || got != want {
			t.Errorf("%s since November: %d, %v; want %d", scope, got, err, want)
		}
	}
	if got, _ := s.UsedSince(ctx, nov.AddDate(0, -1, 0), "acme/web"); got != 160+99999 {
		t.Errorf("since October: %d", got)
	}
	rows, err := s.ListUsage(ctx, nov, time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC))
	if err != nil || len(rows) != 5 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if rows[0] != (Usage{Day: "2026-11-01", Repo: "acme/web", MachineSeconds: 150, Runs: 2}) {
		t.Errorf("first row = %+v", rows[0])
	}
}
