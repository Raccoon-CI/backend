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

// errSuperseded ends a run or job that waited for its concurrency group and was
// replaced by a newer one of the same group, which is what GitHub does to a
// pending run.
var errSuperseded = errors.New("a newer run of the same concurrency group replaced this one while it waited")

// holder is whoever is in a concurrency group: a run (workflow-level
// `concurrency:`) or one job instance (job-level). cancel stops it.
type holder struct {
	id     int64
	desc   string // for the log: "run 12", "job build of run 12"
	cancel func()
}

// cgroup is one concurrency group: at most one holder executes and at most one
// waits. The state is in this process, which is right for the single-instance
// server; after a restart every run is queued again and enters its groups
// afresh.
type cgroup struct {
	running holder
	waiter  *cwaiter
}

type cwaiter struct {
	holder
	ch chan bool // true: your turn; false: replaced by a newer one
}

// groupKey names a group. Workflow-level and job-level groups are apart (the
// same name in both is two groups, as on GitHub), per repository, and names do
// not distinguish case.
func groupKey(level, repo, group string) string {
	return level + "\x00" + strings.ToLower(repo) + "\x00" + strings.ToLower(group)
}

// concurrencyOf evaluates the workflow's `concurrency:` for a run: the group
// ("" for none) and whether a run already in it is cancelled. Only the github,
// inputs and vars contexts exist at workflow level.
func concurrencyOf(w *workflow.Workflow, x *execution) (group string, cancelInProgress bool, err error) {
	return evalConcurrency(w.Concurrency, &expr.Context{Contexts: map[string]any{
		"github": x.github, "inputs": orEmpty(x.inputs), "vars": orEmpty(x.vars),
	}})
}

// jobConcurrencyOf is the same for a job's `concurrency:`, which also sees
// needs, strategy and matrix.
func jobConcurrencyOf(inst jobInstance, x *execution) (string, bool, error) {
	return evalConcurrency(inst.concurrency, &expr.Context{Contexts: map[string]any{
		"github": x.github, "inputs": orEmpty(x.inputs), "vars": orEmpty(x.vars),
		"needs": orEmpty(inst.needs), "strategy": orEmpty(inst.strategy), "matrix": orEmpty(inst.matrix),
	}})
}

// jobInstance is what the job-level group is worked out from.
type jobInstance struct {
	concurrency             any
	needs, strategy, matrix value.Object
}

// evalConcurrency reads a `concurrency:` value (a string, or a mapping with a
// group and cancel-in-progress, either of which may be an expression).
func evalConcurrency(raw any, ectx *expr.Context) (group string, cancelInProgress bool, err error) {
	var rawGroup string
	var rawCancel any
	switch c := raw.(type) {
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

// enterGroup waits until h may execute in the group and returns the function
// that gives the group up. With cancelRunning the holder in the group is
// cancelled at once. One that waits is replaced when a newer one arrives (it
// then gets errSuperseded); the one that waits is always the newest. A run
// (yield) gives up its place in the executor while it waits.
func (e *Executor) enterGroup(ctx context.Context, key, name string, h holder, cancelRunning, yield bool, sink *sink) (func(), error) {
	e.gmu.Lock()
	if e.groups == nil {
		e.groups = map[string]*cgroup{}
	}
	g := e.groups[key]
	if g == nil {
		e.groups[key] = &cgroup{running: h}
		e.gmu.Unlock()
		return func() { e.leaveGroup(key, h.id) }, nil
	}
	me := &cwaiter{holder: h, ch: make(chan bool, 1)}
	if g.waiter != nil {
		g.waiter.ch <- false
	}
	g.waiter = me
	holding := g.running
	e.gmu.Unlock()

	if cancelRunning {
		sink.Add(runner.Event{Kind: runner.Notice, Time: time.Now(),
			Text: fmt.Sprintf("concurrency group %q: cancelling %s, which is in it", name, holding.desc)})
		if holding.cancel != nil {
			holding.cancel()
		}
	} else {
		sink.Add(runner.Event{Kind: runner.Notice, Time: time.Now(),
			Text: fmt.Sprintf("concurrency group %q: waiting for %s, which is in it", name, holding.desc)})
	}
	// A run that only waits must not hold one of the executor's places: with
	// every push in one group, a few waiters would stop all other runs.
	if yield && e.yieldSlot() {
		defer e.takeSlot()
	}
	select {
	case ok := <-me.ch:
		if !ok {
			return nil, errSuperseded
		}
		return func() { e.leaveGroup(key, h.id) }, nil
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
				e.leaveGroup(key, h.id)
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

// leaveGroup hands the group to the one waiting for it, if any.
func (e *Executor) leaveGroup(key string, id int64) {
	e.gmu.Lock()
	defer e.gmu.Unlock()
	g := e.groups[key]
	if g == nil || g.running.id != id {
		return
	}
	if g.waiter == nil {
		delete(e.groups, key)
		return
	}
	next := g.waiter
	g.running, g.waiter = next.holder, nil
	next.ch <- true
}
