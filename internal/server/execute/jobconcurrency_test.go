package execute

import (
	"context"
	"strings"
	"testing"
	"time"
)

func ctx() context.Context { return context.Background() }

const jobGrouped = `on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    concurrency:
      group: deploy-${{ github.ref_name }}
    steps:
      - run: echo hi
`

const jobGroupedCancel = `on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    concurrency:
      group: deploy-${{ github.ref_name }}
      cancel-in-progress: true
    steps:
      - run: echo hi
`

const matrixSameGroup = `on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        n: [1, 2]
    concurrency:
      group: one
    steps:
      - run: echo ${{ matrix.n }}
`

const matrixGroupPerValue = `on: push
jobs:
  deploy:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        n: [1, 2]
    concurrency:
      group: g-${{ matrix.n }}
    steps:
      - run: echo ${{ matrix.n }}
`

func TestAJobWaitsForTheJobBeforeItInItsGroupAcrossRunsAndHasNoMachineYet(t *testing.T) {
	e, m := newGateEnv(t)
	a := e.queue(t, "a", jobGrouped, onRef("main"))
	waitFor(t, "the first job on a machine", func() bool { return m.calls() == 1 })
	b := e.queue(t, "b", jobGrouped, onRef("main"))
	waitFor(t, "b's job waiting", func() bool { return logHas(e, t, b.ID, "waiting for job deploy of run") })
	time.Sleep(200 * time.Millisecond)
	if m.calls() != 1 {
		t.Fatalf("the second job started while the group was taken (%d machine calls)", m.calls())
	}
	if jobs, _ := e.st.ListJobs(ctx(), b.ID); len(jobs) != 0 {
		t.Errorf("a waiting job has a row already: %+v", jobs)
	}
	close(m.gate)
	for _, r := range []int64{a.ID, b.ID} {
		if done := e.wait(t, r); done.Conclusion != "success" {
			t.Fatalf("run %d: %q", r, done.Conclusion)
		}
	}
}

func TestOnlyTheNewestWaitingJobKeepsItsPlace(t *testing.T) {
	e, m := newGateEnv(t)
	a := e.queue(t, "a", jobGrouped, onRef("main"))
	waitFor(t, "the first job on a machine", func() bool { return m.calls() == 1 })
	b := e.queue(t, "b", jobGrouped, onRef("main"))
	waitFor(t, "b's job waiting", func() bool { return logHas(e, t, b.ID, "waiting for job deploy of run") })
	c := e.queue(t, "c", jobGrouped, onRef("main"))
	if r := e.wait(t, b.ID); r.Conclusion == "success" || !logHas(e, t, b.ID, "newer run of the same concurrency group") {
		t.Fatalf("b: %q, log %+v", r.Conclusion, e.log(t, b.ID))
	}
	close(m.gate)
	e.wait(t, a.ID)
	if r := e.wait(t, c.ID); r.Conclusion != "success" {
		t.Fatalf("c: %q", r.Conclusion)
	}
	if m.calls() != 2 {
		t.Fatalf("%d machine calls, want a's and c's only", m.calls())
	}
}

func TestCancelInProgressCancelsTheJobInTheGroupAndNotItsWholeRun(t *testing.T) {
	e, m := newGateEnv(t)
	a := e.queue(t, "a", jobGroupedCancel, onRef("main"))
	waitFor(t, "the first job on a machine", func() bool { return m.calls() == 1 })
	b := e.queue(t, "b", jobGroupedCancel, onRef("main"))
	ra := e.wait(t, a.ID)
	if ra.Conclusion != "cancelled" || !logHas(e, t, a.ID, "cancelled by a newer job of its concurrency group") {
		t.Fatalf("a: %q, log %+v", ra.Conclusion, e.log(t, a.ID))
	}
	if rb := e.wait(t, b.ID); rb.Conclusion != "success" {
		t.Fatalf("b: %q", rb.Conclusion)
	}
	jobs, _ := e.st.ListJobs(ctx(), a.ID)
	if len(jobs) != 1 || jobs[0].Conclusion != "cancelled" {
		t.Errorf("a's job = %+v", jobs)
	}
}

func TestJobsOfOneMatrixInOneGroupTakeTurns(t *testing.T) {
	e, m := newGateEnv(t)
	run := e.queue(t, "m", matrixSameGroup, onRef("main"))
	waitFor(t, "the first job on a machine", func() bool { return m.calls() == 1 })
	time.Sleep(300 * time.Millisecond)
	if m.calls() != 1 {
		t.Fatalf("both matrix jobs run at once (%d machine calls)", m.calls())
	}
	close(m.gate)
	if r := e.wait(t, run.ID); r.Conclusion != "success" || m.calls() != 2 {
		t.Fatalf("%q after %d machine calls", r.Conclusion, m.calls())
	}
}

func TestJobsInDifferentGroupsRunTogether(t *testing.T) {
	e, m := newGateEnv(t)
	run := e.queue(t, "m", matrixGroupPerValue, onRef("main"))
	waitFor(t, "both jobs on machines", func() bool { return m.calls() == 2 })
	close(m.gate)
	if r := e.wait(t, run.ID); r.Conclusion != "success" {
		t.Fatalf("%q", r.Conclusion)
	}
}

func TestTheSameNameAtWorkflowAndJobLevelIsTwoGroups(t *testing.T) {
	const both = `on: push
concurrency:
  group: same
jobs:
  j:
    runs-on: ubuntu-latest
    concurrency:
      group: same
    steps:
      - run: echo hi
`
	e, m := newGateEnv(t)
	run := e.queue(t, "both", both, onRef("main"))
	waitFor(t, "the job on a machine (it must not wait for its own run)", func() bool { return m.calls() == 1 })
	close(m.gate)
	if r := e.wait(t, run.ID); r.Conclusion != "success" {
		t.Fatalf("%q", r.Conclusion)
	}
}

func TestABadJobGroupFailsTheJobWithAMessage(t *testing.T) {
	e, _ := newGateEnv(t)
	bad := strings.Replace(jobGrouped, "deploy-${{ github.ref_name }}", "${{ nope.x }}", 1)
	run := e.queue(t, "bad", bad, onRef("main"))
	if done := e.wait(t, run.ID); done.Conclusion != "failure" || !logHas(e, t, run.ID, "concurrency group") {
		t.Fatalf("%q, log %+v", done.Conclusion, e.log(t, run.ID))
	}
}
