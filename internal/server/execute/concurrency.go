package execute

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"raccoonci/internal/expr"
	"raccoonci/internal/runner"
	"raccoonci/internal/value"
	"raccoonci/internal/workflow"
)

// errSuperseded ends a run that waited for its concurrency group and was
// replaced by a newer run of the same group, which is what GitHub does to a
// pending run.
var errSuperseded = errors.New("a newer run of the same concurrency group replaced this one while it waited")

// cgroup is one workflow-level concurrency group: at most one run in it
// executes and at most one waits. The state is in this process, which is
// right for the single-instance server; after a restart every run is queued
// again and enters its group afresh.
type cgroup struct {
	running int64
	waiter  *cwaiter
}

type cwaiter struct {
	run int64
	ch  chan bool // true: your turn; false: replaced by a newer run
}

// concurrencyOf evaluates the workflow's `concurrency:` for a run: the group
// ("" for none) and whether a run already in it is cancelled. Only the github,
// inputs and vars contexts exist at workflow level.
func concurrencyOf(w *workflow.Workflow, x *execution) (group string, cancelInProgress bool, err error) {
	ectx := &expr.Context{Contexts: map[string]any{
		"github": x.github, "inputs": orEmpty(x.inputs), "vars": orEmpty(x.vars),
	}}
	var rawGroup string
	var rawCancel any
	switch c := w.Concurrency.(type) {
	case nil:
		return "", false, nil
	case string:
		rawGroup = c
	case value.Object:
		g, _ := c.Get("group")
		s, ok := g.(string)
		if !ok {
			return "", false, errors.New("concurrency: group must be a string")
		}
		rawGroup = s
		rawCancel, _ = c.Get("cancel-in-progress")
	default:
		return "", false, errors.New("concurrency: must be a string or a mapping with a group")
	}
	v, err := expr.Interpolate(rawGroup, ectx)
	if err != nil {
		return "", false, fmt.Errorf("concurrency group: %w", err)
	}
	group = strings.TrimSpace(expr.ToString(v))
	if s, ok := rawCancel.(string); ok {
		r, err := expr.Interpolate(s, ectx)
		if err != nil {
			return "", false, fmt.Errorf("concurrency cancel-in-progress: %w", err)
		}
		rawCancel = r
	}
	switch c := rawCancel.(type) {
	case bool:
		cancelInProgress = c
	case string:
		cancelInProgress = c == "true"
	}
	return group, cancelInProgress, nil
}

// enterGroup waits until the run may execute in its group and returns the
// function that gives the group up. With cancelRunning the run in the group is
// cancelled at once. A run that waits is replaced when a newer one arrives (it
// then gets errSuperseded); the one that waits is always the newest.
func (e *Executor) enterGroup(ctx context.Context, repo, group string, runID int64, cancelRunning bool, sink *sink) (func(), error) {
	key := strings.ToLower(repo) + "\x00" + strings.ToLower(group)
	e.gmu.Lock()
	if e.groups == nil {
		e.groups = map[string]*cgroup{}
	}
	g := e.groups[key]
	if g == nil {
		e.groups[key] = &cgroup{running: runID}
		e.gmu.Unlock()
		return func() { e.leaveGroup(key, runID) }, nil
	}
	me := &cwaiter{run: runID, ch: make(chan bool, 1)}
	if g.waiter != nil {
		g.waiter.ch <- false
	}
	g.waiter = me
	holder := g.running
	e.gmu.Unlock()

	if cancelRunning {
		sink.Add(runner.Event{Kind: runner.Notice, Time: time.Now(),
			Text: fmt.Sprintf("concurrency group %q: cancelling run %d, which is in it", group, holder)})
		e.Cancel(holder)
	} else {
		sink.Add(runner.Event{Kind: runner.Notice, Time: time.Now(),
			Text: fmt.Sprintf("concurrency group %q: waiting for run %d, which is in it", group, holder)})
	}
	// A run that only waits must not hold one of the executor's places: with
	// every push in one group, a few waiters would stop all other runs.
	yielded := e.yieldSlot()
	if yielded {
		defer e.takeSlot()
	}
	select {
	case ok := <-me.ch:
		if !ok {
			return nil, errSuperseded
		}
		return func() { e.leaveGroup(key, runID) }, nil
	case <-ctx.Done():
		e.gmu.Lock()
		if g := e.groups[key]; g != nil && g.waiter == me {
			g.waiter = nil
			e.gmu.Unlock()
			return nil, ctx.Err()
		}
		e.gmu.Unlock()
		// The turn came (or the replacement) while we were being cancelled.
		select {
		case ok := <-me.ch:
			if ok {
				e.leaveGroup(key, runID)
			}
		default:
		}
		return nil, ctx.Err()
	}
}

// yieldSlot gives up the calling run's place among the runs that execute at
// once, and wakes the executor so that it takes another run in it. It reports
// whether there was a place to give up (not when the executor is not running
// the loop, as in a test of the group alone).
func (e *Executor) yieldSlot() bool {
	if e.slots == nil {
		return false
	}
	select {
	case <-e.slots:
		e.Wake()
		return true
	default:
		return false
	}
}

// takeSlot gets a place back after yieldSlot; it waits for one to be free.
func (e *Executor) takeSlot() { e.slots <- struct{}{} }

// leaveGroup hands the group to the run waiting for it, if any.
func (e *Executor) leaveGroup(key string, runID int64) {
	e.gmu.Lock()
	defer e.gmu.Unlock()
	g := e.groups[key]
	if g == nil || g.running != runID {
		return
	}
	if g.waiter == nil {
		delete(e.groups, key)
		return
	}
	next := g.waiter
	g.running, g.waiter = next.run, nil
	next.ch <- true
}
