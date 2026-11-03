package execute

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Raccoon-CI/backend/internal/server/store"
	"github.com/Raccoon-CI/lib/pkg/dispatch"
	"github.com/Raccoon-CI/lib/pkg/wire"
)

const grouped = `on: push
concurrency:
  group: deploy-${{ github.ref_name }}
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`

const groupedCancel = `on: push
concurrency:
  group: deploy-${{ github.ref_name }}
  cancel-in-progress: ${{ github.ref_name == 'main' }}
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`

// gateMachine holds the first call until gate is closed (or its run is
// cancelled) and lets every later call through; it records the calls.
type gateMachine struct {
	mu    sync.Mutex
	names []string
	gate  chan struct{}
}

func (m *gateMachine) Run(ctx context.Context, name string, job dispatch.Job) (dispatch.Result, error) {
	m.mu.Lock()
	first := len(m.names) == 0
	m.names = append(m.names, name)
	m.mu.Unlock()
	if first {
		select {
		case <-m.gate:
		case <-ctx.Done():
			return dispatch.Result{}, ctx.Err()
		}
	}
	var spec wire.InstanceSpec
	if err := json.Unmarshal(job.Input, &spec); err != nil {
		return dispatch.Result{}, err
	}
	return succeed(call{name: name, spec: spec, job: job}, wire.NewEncoder(job.Log))
}

func (m *gateMachine) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.names)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	// Generous: the whole suite runs in parallel under -race, on a shared machine.
	deadline := time.Now().Add(45 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func logHas(e *env, t *testing.T, id int64, text string) bool {
	for _, ev := range e.log(t, id) {
		if strings.Contains(ev.Text, text) {
			return true
		}
	}
	return false
}

func onRef(ref string) func(*store.Run) {
	return func(r *store.Run) { r.Ref = "refs/heads/" + ref }
}

func newGateEnv(t *testing.T) (*env, *gateMachine) {
	e := newEnv(t)
	m := &gateMachine{gate: make(chan struct{})}
	e.ex.Machine = m
	e.start(t)
	return e, m
}

func TestARunWaitsForTheRunBeforeItInTheSameGroup(t *testing.T) {
	e, m := newGateEnv(t)
	a := e.queue(t, "a", grouped, onRef("main"))
	waitFor(t, "the first job on a machine", func() bool { return m.calls() == 1 })
	b := e.queue(t, "b", grouped, onRef("main"))
	waitFor(t, "the wait notice", func() bool { return logHas(e, t, b.ID, "waiting for run") })
	time.Sleep(200 * time.Millisecond)
	if m.calls() != 1 {
		t.Fatalf("the second run started while the group was taken (%d machine calls)", m.calls())
	}
	close(m.gate)
	if r := e.wait(t, a.ID); r.Conclusion != "success" {
		t.Fatalf("a: %q", r.Conclusion)
	}
	if r := e.wait(t, b.ID); r.Conclusion != "success" {
		t.Fatalf("b: %q", r.Conclusion)
	}
}

func TestOnlyTheNewestWaitingRunKeepsItsPlace(t *testing.T) {
	e, m := newGateEnv(t)
	a := e.queue(t, "a", grouped, onRef("main"))
	waitFor(t, "the first job on a machine", func() bool { return m.calls() == 1 })
	b := e.queue(t, "b", grouped, onRef("main"))
	waitFor(t, "b waiting", func() bool { return logHas(e, t, b.ID, "waiting for run") })
	c := e.queue(t, "c", grouped, onRef("main"))
	if r := e.wait(t, b.ID); r.Conclusion != "cancelled" || !logHas(e, t, b.ID, "newer run of the same concurrency group") {
		t.Fatalf("b: %q, log %+v", r.Conclusion, e.log(t, b.ID))
	}
	close(m.gate)
	e.wait(t, a.ID)
	if r := e.wait(t, c.ID); r.Conclusion != "success" {
		t.Fatalf("c: %q", r.Conclusion)
	}
	if m.calls() != 2 {
		t.Fatalf("%d machine calls, want a and c only", m.calls())
	}
}

func TestCancelInProgressStopsTheRunInTheGroup(t *testing.T) {
	e, m := newGateEnv(t)
	a := e.queue(t, "a", groupedCancel, onRef("main"))
	waitFor(t, "the first job on a machine", func() bool { return m.calls() == 1 })
	b := e.queue(t, "b", groupedCancel, onRef("main"))
	if r := e.wait(t, a.ID); r.Conclusion != "cancelled" {
		t.Fatalf("a: %q", r.Conclusion)
	}
	if r := e.wait(t, b.ID); r.Conclusion != "success" {
		t.Fatalf("b: %q", r.Conclusion)
	}
}

func TestCancelInProgressIsAnExpressionAndFalseJustWaits(t *testing.T) {
	e, m := newGateEnv(t)
	a := e.queue(t, "a", groupedCancel, onRef("dev")) // cancel-in-progress is false for dev
	waitFor(t, "the first job on a machine", func() bool { return m.calls() == 1 })
	b := e.queue(t, "b", groupedCancel, onRef("dev"))
	waitFor(t, "b waiting", func() bool { return logHas(e, t, b.ID, "waiting for run") })
	close(m.gate)
	if r := e.wait(t, a.ID); r.Conclusion != "success" {
		t.Fatalf("a was cancelled although cancel-in-progress is false: %q", r.Conclusion)
	}
	e.wait(t, b.ID)
}

func TestDifferentGroupsRunAtTheSameTime(t *testing.T) {
	e, m := newGateEnv(t)
	a := e.queue(t, "a", grouped, onRef("main"))
	waitFor(t, "the first job on a machine", func() bool { return m.calls() == 1 })
	b := e.queue(t, "b", grouped, onRef("dev"))
	if r := e.wait(t, b.ID); r.Conclusion != "success" {
		t.Fatalf("b: %q", r.Conclusion)
	}
	close(m.gate)
	e.wait(t, a.ID)
}

func TestAWaitingRunThatIsCancelledLeavesTheGroupAlone(t *testing.T) {
	e, m := newGateEnv(t)
	a := e.queue(t, "a", grouped, onRef("main"))
	waitFor(t, "the first job on a machine", func() bool { return m.calls() == 1 })
	b := e.queue(t, "b", grouped, onRef("main"))
	waitFor(t, "b waiting", func() bool { return logHas(e, t, b.ID, "waiting for run") })
	if !e.ex.Cancel(b.ID) {
		t.Fatal("the waiting run is not cancellable")
	}
	if r := e.wait(t, b.ID); r.Conclusion != "cancelled" {
		t.Fatalf("b: %q", r.Conclusion)
	}
	close(m.gate)
	if r := e.wait(t, a.ID); r.Conclusion != "success" {
		t.Fatalf("a: %q", r.Conclusion)
	}
	// The group is free again: a new run goes straight in.
	c := e.queue(t, "c", grouped, onRef("main"))
	if r := e.wait(t, c.ID); r.Conclusion != "success" {
		t.Fatalf("c: %q", r.Conclusion)
	}
}

func TestARunWaitingForItsGroupDoesNotHoldAPlaceOfTheExecutor(t *testing.T) {
	e := newEnv(t)
	m := &gateMachine{gate: make(chan struct{})}
	e.ex.Machine = m
	e.ex.MaxRuns = 2 // the running run and the waiting one would fill it
	e.start(t)
	a := e.queue(t, "a", grouped, onRef("main"))
	waitFor(t, "the first job on a machine", func() bool { return m.calls() == 1 })
	b := e.queue(t, "b", grouped, onRef("main"))
	waitFor(t, "b waiting", func() bool { return logHas(e, t, b.ID, "waiting for run") })
	// A third run, of another group, still gets the second place.
	c := e.queue(t, "c", grouped, onRef("dev"))
	if r := e.wait(t, c.ID); r.Conclusion != "success" {
		t.Fatalf("c: %q (the waiting run held a place)", r.Conclusion)
	}
	// And a fourth waiter does not stop things either.
	d := e.queue(t, "d", grouped, onRef("main"))
	waitFor(t, "d replaces b", func() bool { return logHas(e, t, b.ID, "newer run of the same concurrency group") })
	close(m.gate)
	e.wait(t, a.ID)
	if r := e.wait(t, d.ID); r.Conclusion != "success" {
		t.Fatalf("d: %q", r.Conclusion)
	}
	e.wait(t, b.ID)
}

func TestABadConcurrencyGroupFailsTheRunWithAMessage(t *testing.T) {
	e, _ := newGateEnv(t)
	bad := strings.Replace(grouped, "deploy-${{ github.ref_name }}", "${{ nope.x }}", 1)
	r := e.queue(t, "bad", bad, onRef("main"))
	if done := e.wait(t, r.ID); done.Conclusion != "failure" || !logHas(e, t, r.ID, "concurrency group") {
		t.Fatalf("%q, log %+v", done.Conclusion, e.log(t, r.ID))
	}
}
