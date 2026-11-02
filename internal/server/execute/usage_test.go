package execute

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/server/store"
	"github.com/Raccoon-CI/raccoonci/pkg/dispatch"
	"github.com/Raccoon-CI/raccoonci/pkg/wire"
)

var nov15 = time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)

func TestTheTimeAMachineWasHeldIsCountedForTheRepositoryWhenTheRunEnds(t *testing.T) {
	e := newEnv(t)
	e.ex.Now = func() time.Time { return nov15 }
	e.m.handle = func(c call, out *wire.Encoder) (dispatch.Result, error) {
		time.Sleep(1100 * time.Millisecond) // two jobs, so about 2 s each of machine time
		return succeed(c, out)
	}
	run := e.queue(t, "u1", twoJobs, nil)
	e.start(t)
	e.wait(t, run.ID)
	// The row is written just after the run is marked finished.
	var secs int64
	for i := 0; i < 200 && secs == 0; i++ {
		secs, _ = e.st.UsedSince(context.Background(), nov15, "acme/web")
		time.Sleep(10 * time.Millisecond)
	}
	// Two instances one after the other, 1.1 s each: 2.2 s, summed over the run
	// and rounded up once, so 3 (a little more only if the machine was slow).
	if secs < 3 || secs > 5 {
		t.Fatalf("counted %d machine seconds for two job instances of 1.1 s each", secs)
	}
	rows, _ := e.st.ListUsage(context.Background(), nov15, nov15)
	if len(rows) != 1 || rows[0].Repo != "acme/web" || rows[0].Runs != 1 || rows[0].Day != "2026-11-15" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestARepositoryThatUsedUpItsMonthlyAllowanceFailsItsNewRunsWithTheReason(t *testing.T) {
	e := newEnv(t)
	e.ex.Now = func() time.Time { return nov15 }
	e.ex.Quota = Quota{RepoMinutes: 60}
	e.st.AddUsage(context.Background(), "acme/web", nov15.AddDate(0, 0, -3), 60*60, 5) // exactly the allowance

	over := e.queue(t, "q1", twoJobs, nil)
	other := e.queue(t, "q2", twoJobs, func(r *store.Run) { r.Repo = "acme/other" }) // another repository is fine
	e.start(t)
	if r := e.wait(t, over.ID); r.Conclusion != "failure" {
		t.Fatalf("over the allowance: %q", r.Conclusion)
	}
	if !logHas(e, t, over.ID, "acme/web has used its machine-time allowance for this month (60 of 60 minutes); runs start again on 2026-12-01 UTC") {
		t.Fatalf("log = %+v", e.log(t, over.ID))
	}
	if r := e.wait(t, other.ID); r.Conclusion != "success" {
		t.Fatalf("another repository: %q", r.Conclusion)
	}
	e.m.mu.Lock()
	for _, c := range e.m.calls {
		if strings.HasPrefix(c.name, "rc-r"+strconv.FormatInt(over.ID, 10)+"-") {
			t.Errorf("a machine was used for the run over its allowance: %s", c.name)
		}
	}
	e.m.mu.Unlock()
}

func TestAnOwnersAllowanceCountsAllItsRepositories(t *testing.T) {
	e := newEnv(t)
	e.ex.Now = func() time.Time { return nov15 }
	e.ex.Quota = Quota{OwnerMinutes: 100}
	e.st.AddUsage(context.Background(), "acme/api", nov15, 60*60, 1)
	e.st.AddUsage(context.Background(), "acme/tool", nov15, 41*60, 1) // together 101 minutes
	e.st.AddUsage(context.Background(), "other/web", nov15, 1000*60, 1)
	run := e.queue(t, "o1", twoJobs, nil) // acme/web
	e.start(t)
	if r := e.wait(t, run.ID); r.Conclusion != "failure" || !logHas(e, t, run.ID, "acme has used its machine-time allowance for this month (101 of 100 minutes over all its repositories)") {
		t.Fatalf("%q, log %+v", r.Conclusion, e.log(t, run.ID))
	}
}

func TestTheAllowanceStartsOverWithTheMonthAndNoAllowanceMeansNone(t *testing.T) {
	e := newEnv(t)
	now := nov15
	e.ex.Now = func() time.Time { return now }
	e.ex.Quota = Quota{RepoMinutes: 10}
	e.st.AddUsage(context.Background(), "acme/web", nov15, 3600, 1) // far over, in November
	e.start(t)

	a := e.queue(t, "m1", twoJobs, nil)
	if r := e.wait(t, a.ID); r.Conclusion != "failure" {
		t.Fatalf("in November: %q", r.Conclusion)
	}
	now = time.Date(2026, 12, 2, 8, 0, 0, 0, time.UTC)
	b := e.queue(t, "m2", twoJobs, nil)
	if r := e.wait(t, b.ID); r.Conclusion != "success" {
		t.Fatalf("in December the November usage must not count: %q", r.Conclusion)
	}

	free := newEnv(t) // no Quota set
	free.ex.Now = func() time.Time { return nov15 }
	free.st.AddUsage(context.Background(), "acme/web", nov15, 1_000_000, 1)
	free.start(t)
	c := free.queue(t, "m3", twoJobs, nil)
	if r := free.wait(t, c.ID); r.Conclusion != "success" {
		t.Fatalf("without an allowance: %q", r.Conclusion)
	}
}
