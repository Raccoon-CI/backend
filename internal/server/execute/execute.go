// Package execute runs the queued runs of the server: for each it fetches the
// repository at the run's commit, resolves the workflow's jobs with the
// engine, and sends every job instance to a machine of the compute provider
// (internal/dispatch protocol, `raccoonci run-instance` on the machine).
// Events come back over the internal/wire protocol and are stored as the run's
// log.
//
// The engine runs in this process, one goroutine graph per run, as `raccoonci
// run -on instabox` does. A run in progress when the server stops is ended as
// failed at the next start (Executor.Run); resuming it is a later step.
package execute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"raccoonci/internal/catalog"
	"raccoonci/internal/dispatch"
	"raccoonci/internal/engine"
	"raccoonci/internal/runner"
	"raccoonci/internal/runnerimages"
	"raccoonci/internal/server/github"
	"raccoonci/internal/server/secrets"
	"raccoonci/internal/server/store"
	"raccoonci/internal/value"
	"raccoonci/internal/wire"
	"raccoonci/internal/workflow"
)

// Machine runs one dispatch.Job on a machine of the compute provider and
// waits for it to end. instabox.Provider is the real one (see Instabox).
type Machine interface {
	Run(ctx context.Context, name string, job dispatch.Job) (dispatch.Result, error)
}

// A Machine that keeps files on this server (Local) can also have Clean, which
// the executor calls at start to remove what a run that died left behind.
type cleaner interface{ Clean() error }

// removeContents deletes everything in dir but not dir itself; a missing dir
// has nothing in it.
func removeContents(dir string) error {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var firstErr error
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// clean removes the leftovers of runs that were in progress when the server
// died. It runs at start, when none of this server's runs is executing (one
// server uses a data directory).
func (e *Executor) clean() {
	if err := removeContents(e.WorkDir); err != nil {
		e.log().Warn("removing the work directories of runs that died failed", "dir", e.WorkDir, "err", err)
	}
	if c, ok := e.Machine.(cleaner); ok {
		if err := c.Clean(); err != nil {
			e.log().Warn("removing what jobs left on this machine failed", "err", err)
		}
	}
}

// Source writes a repository at a commit as a .tar.gz with the files at its
// root (github.Client.SourceTar).
type Source interface {
	SourceTar(ctx context.Context, installationID int64, repo, sha string, w io.Writer) error
}

// Services gives the environment a job's steps need to reach the artifact
// service as one job of one run (artifacts.Service). It may be nil.
type Services interface {
	JobEnv(run int64, job string) (map[string]string, error)
}

// SecretsSource says which secrets and variables a run gets (secrets.Service).
type SecretsSource interface {
	ForRun(ctx context.Context, repo, workflowText string, fork bool) (secrets.Run, error)
}

// Executor takes queued runs and runs them.
type Executor struct {
	Store    store.Store
	Source   Source
	Machine  Machine
	Services Services
	// WorkDir holds the source archive of each run while it runs.
	WorkDir string
	// MaxRuns is how many runs go at once (default 4); MaxParallel how many
	// job instances of one run (default 8).
	MaxRuns     int
	MaxParallel int
	// MaxAttempts is how many times a run may be taken: a run in progress when
	// the server stopped (or being run when it is stopped) is queued again
	// until it has been taken this often (default 3), then it ends as failed.
	MaxAttempts int
	// Install is what machines may install for a job (`raccoonci run -install`):
	// "all" (default) also installs apt packages, which is fine on machines
	// that exist for one job.
	Install string
	// Poll is how often the queue is looked at when nobody calls Wake.
	Poll time.Duration
	// MinFreeBytes, with Free, keeps new runs waiting in the queue while the
	// work directory's disk has less than this available (0 = no check).
	MinFreeBytes int64
	Free         func() (int64, error)
	// MergeTries and MergeWait say how often and how far apart the merge ref of
	// a pull request is asked for while GitHub is still making it (default 4, 2 s).
	MergeTries int
	MergeWait  time.Duration
	// Images, if set, gives the runner-images snapshot a run is planned with
	// (default: the one compiled in).
	Images func(ctx context.Context) (*runnerimages.Snapshot, error)
	// MaxSourceBytes caps the source package fetched for one run (default 2 GiB).
	MaxSourceBytes int64
	// Reporter, if set, is told when a run starts and how it ended.
	Reporter Reporter
	// Checks, if set, gets a check run per job instance for events that carry a
	// GitHub App installation (a plain token cannot write check runs).
	Checks CheckRunner
	// IDTokens, if set, gives the jobs that have `permissions: id-token: write`
	// what they need to ask for OpenID Connect tokens (oidc.Service).
	IDTokens IDTokens
	// Tokens, if set, makes each job instance a GITHUB_TOKEN of its own (a
	// GitHub App installation token for the run's repository); see secretsFor.
	Tokens TokenMinter
	// Secrets, if set, gives each run its secrets and variables.
	Secrets SecretsSource
	Log     *slog.Logger

	wake    chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
	mu      sync.Mutex
	cancels map[int64]context.CancelFunc

	gmu       sync.Mutex
	groups    map[string]*cgroup // concurrency groups, workflow-level and job-level, see concurrency.go
	holderSeq atomic.Int64       // ids of job-level holders

	slots chan struct{} // the runs that may execute at once (Run); a run waiting for a concurrency group gives its place up
}

func (e *Executor) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (e *Executor) maxAttempts() int {
	if e.MaxAttempts < 1 {
		return 3
	}
	return e.MaxAttempts
}

func (e *Executor) init() {
	e.once.Do(func() { e.wake = make(chan struct{}, 1) })
}

// Wake tells the executor a run may have been queued.
func (e *Executor) Wake() {
	e.init()
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Run takes queued runs until ctx ends, then waits for the runs it started to
// finish (they are cancelled by ctx and queued again, to be taken by the next
// start). It first deals with the runs a stopped server left in progress: they
// are queued again, unless they have been taken MaxAttempts times, when they
// end as failed.
func (e *Executor) Run(ctx context.Context) error {
	e.init()
	if requeued, ended, err := e.Store.RequeueInterrupted(ctx, e.maxAttempts(), string(runner.Failure)); err != nil {
		return fmt.Errorf("dealing with the runs left from before: %w", err)
	} else if requeued+ended > 0 {
		e.log().Warn("runs were in progress when the server stopped", "queued_again", requeued, "ended_as_failed", ended)
	}
	e.clean()
	maxRuns := e.MaxRuns
	if maxRuns < 1 {
		maxRuns = 4
	}
	poll := e.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	slots := make(chan struct{}, maxRuns)
	e.slots = slots
	warned := false
	for {
		for ctx.Err() == nil {
			if low, free := e.lowOnDisk(); low {
				if !warned {
					e.log().Warn("the disk is nearly full: queued runs wait until space is freed", "free_mib", free>>20, "needed_mib", e.MinFreeBytes>>20)
					warned = true
				}
				break
			}
			warned = false
			select {
			case slots <- struct{}{}:
			default:
				goto wait // all slots busy
			}
			run, ok, err := e.Store.ClaimRun(ctx)
			if err != nil || !ok {
				<-slots
				if err != nil && ctx.Err() == nil {
					e.log().Error("looking for a queued run failed", "err", err)
				}
				break
			}
			e.wg.Add(1)
			go func() {
				defer e.wg.Done()
				defer func() { <-slots }()
				e.execute(ctx, run)
			}()
		}
	wait:
		select {
		case <-ctx.Done():
			e.wg.Wait()
			return nil
		case <-e.wake:
		case <-time.After(poll):
		}
	}
}

// execute runs one claimed run and records how it ended.
func (e *Executor) execute(parent context.Context, run store.Run) {
	log := e.log().With("run", run.ID, "repo", run.Repo, "workflow", run.WorkflowPath)
	sink := newSink(e.Store, run.ID, log)
	ctx, cancel := context.WithCancel(parent) // Cancel ends this run alone; stopping the server ends all
	defer cancel()
	e.register(run.ID, cancel)
	defer e.unregister(run.ID)
	began := time.Now()
	conclusion := runner.Failure
	var installation int64
	if ev, err := e.Store.GetEvent(ctx, run.EventID); err == nil {
		installation = ev.InstallationID
	}
	pending := e.report(run, installation, github.StatePending, "Running", nil)
	defer func() {
		if r := recover(); r != nil {
			log.Error("a run panicked", "panic", fmt.Sprint(r))
			sink.Add(runner.Event{Kind: runner.Error, Time: time.Now(), Text: fmt.Sprintf("the server failed while running this: %v", r)})
			conclusion = runner.Failure
		}
		sink.Close()
		if conclusion == runner.Cancelled && parent.Err() != nil {
			// The server is stopping, nobody cancelled this run: it goes back in
			// the queue for the next start, and GitHub's status stays pending.
			if ok, err := e.Store.Requeue(context.Background(), run.ID, e.maxAttempts()); err != nil {
				log.Error("queuing a run again failed", "err", err)
			} else if ok {
				log.Info("run stopped with the server; queued again", "attempt", run.Attempts)
				return
			}
		}
		if err := e.Store.FinishRun(context.Background(), run.ID, string(conclusion)); err != nil {
			log.Error("recording the end of a run failed", "err", err)
		}
		state, desc := github.StateFailure, "Failed"
		took := time.Since(began).Round(time.Second)
		switch conclusion {
		case runner.Success:
			state, desc = github.StateSuccess, fmt.Sprintf("Succeeded in %v", took)
		case runner.Cancelled:
			state, desc = github.StateError, "Cancelled"
		default:
			desc = fmt.Sprintf("Failed after %v", took)
		}
		<-e.report(run, installation, state, desc, pending)
		log.Info("run ended", "conclusion", conclusion)
	}()
	log.Info("run started", "attempt", run.Attempts)
	if run.Attempts > 1 {
		sink.Add(runner.Event{Kind: runner.Warning, Time: time.Now(),
			Text: fmt.Sprintf("the server stopped while this ran; it is started again from the beginning (attempt %d)", run.Attempts)})
	}

	x, err := e.prepare(ctx, run, sink)
	if err != nil {
		if errors.Is(err, errSuperseded) {
			sink.Add(runner.Event{Kind: runner.Notice, Time: time.Now(), Text: err.Error()})
			conclusion = runner.Cancelled
			return
		}
		sink.Add(runner.Event{Kind: runner.Error, Time: time.Now(), Text: err.Error()})
		if ctx.Err() != nil {
			conclusion = runner.Cancelled
		}
		return
	}
	defer os.RemoveAll(x.dir)
	if x.release != nil {
		defer x.release()
	}

	res, err := engine.Run(ctx, x.w, engine.Options{
		Remote:      x.runInstance,
		Github:      x.github,
		Vars:        x.vars,
		Inputs:      x.inputs,
		Images:      x.images,
		Catalog:     catalog.Default(),
		WorkDir:     x.dir,
		MaxParallel: x.maxParallel,
		Runner:      runner.Config{OnEvent: sink.Add},
	})
	switch {
	case err != nil:
		sink.Add(runner.Event{Kind: runner.Error, Time: time.Now(), Text: err.Error()})
	case ctx.Err() != nil:
		conclusion = runner.Cancelled
	default:
		conclusion = res.Conclusion
	}
}

// runnerImages is what the hosted images preinstall: Images' answer, or the
// snapshot compiled in. When Images fails but still returns a snapshot (the
// database could not be read) that one is used and the failure is logged.
func (e *Executor) runnerImages(ctx context.Context) (*runnerimages.Snapshot, error) {
	if e.Images == nil {
		return runnerimages.Embedded()
	}
	snap, err := e.Images(ctx)
	if err != nil {
		if snap == nil {
			return nil, err
		}
		e.log().Warn("the polled runner-images data could not be read; using the built-in snapshot", "err", err)
	}
	return snap, nil
}

// Reporter tells GitHub how a run is going (a commit status, or a check run).
type Reporter interface {
	Report(ctx context.Context, installationID int64, run store.Run, state, description string) error
}

// report sends a state to GitHub in the background, after the report it
// follows (after, may be nil) has been sent, so a slow "pending" cannot
// overwrite the end. A failure is logged, never fatal: GitHub being
// unreachable must not fail a run. The channel closes when it is sent.
func (e *Executor) report(run store.Run, installation int64, state, desc string, after <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	if e.Reporter == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		if after != nil {
			<-after
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := e.Reporter.Report(ctx, installation, run, state, desc); err != nil {
			e.log().Warn("reporting to GitHub failed", "run", run.ID, "state", state, "err", err)
		}
	}()
	return done
}

func (e *Executor) register(id int64, cancel context.CancelFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancels == nil {
		e.cancels = map[int64]context.CancelFunc{}
	}
	e.cancels[id] = cancel
}

func (e *Executor) unregister(id int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.cancels, id)
}

// Cancel stops a run this executor is running; false when it is not running one with that id.
func (e *Executor) Cancel(id int64) bool {
	e.mu.Lock()
	cancel, ok := e.cancels[id]
	e.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// execution is one run being run.
type execution struct {
	e           *Executor
	run         store.Run
	log         *slog.Logger
	sink        *sink
	w           *workflow.Workflow
	github      value.Object
	inputs      value.Object
	images      *runnerimages.Snapshot
	dir         string
	sourceTar   string
	maxParallel int
	n           atomic.Int32
	// installation is the GitHub App installation of the run's event, 0 for none.
	installation int64
	// release gives up the run's concurrency group; nil when it has none.
	release func()
	// secrets are the opened values this run's workflow names, sent to each
	// machine in its assignment; vars the variables of its repository.
	secrets map[string]string
	vars    value.Object
}

func (e *Executor) prepare(ctx context.Context, run store.Run, sink *sink) (_ *execution, err error) {
	var release func() // the concurrency group, given up again if preparing fails
	defer func() {
		if err != nil && release != nil {
			release()
		}
	}()
	w, err := workflow.Parse([]byte(run.WorkflowText))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", run.WorkflowPath, err)
	}
	ev, err := e.Store.GetEvent(ctx, run.EventID)
	if err != nil {
		return nil, fmt.Errorf("reading the event of the run: %w", err)
	}
	inputs, err := parseObject(run.Inputs)
	if err != nil {
		return nil, fmt.Errorf("the run's inputs: %w", err)
	}
	payload, err := parseObject(string(ev.Payload))
	if err != nil {
		return nil, fmt.Errorf("the event's payload: %w", err)
	}
	images, err := e.runnerImages(ctx)
	if err != nil {
		return nil, err
	}
	// The secrets and variables are resolved once, before anything is made, so
	// a failure here leaves nothing behind. Opened values stay in memory and
	// go to the machines in the assignment; they are never written to disk or
	// to the log (the runner masks them in what it prints).
	var secretValues map[string]string
	vars := value.Object{}
	if e.Secrets != nil {
		r, err := e.Secrets.ForRun(ctx, run.Repo, run.WorkflowText, run.Fork)
		if err != nil {
			return nil, fmt.Errorf("reading the secrets: %w", err)
		}
		secretValues = r.Secrets
		for _, k := range slices.Sorted(maps.Keys(r.Vars)) {
			vars = append(vars, value.Field{Key: k, Value: r.Vars[k]})
		}
		for _, w := range r.Warnings {
			sink.Add(runner.Event{Kind: runner.Warning, Time: time.Now(), Text: w})
		}
		if run.Fork {
			sink.Add(runner.Event{Kind: runner.Notice, Time: time.Now(), Text: "this run is for a pull request from another repository: it gets no secrets"})
		}
	}
	number, attempt, err := e.Store.RunNumbers(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("numbering the run: %w", err)
	}
	dir := filepath.Join(e.WorkDir, fmt.Sprintf("run-%d", run.ID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	x := &execution{e: e, run: run, log: e.log().With("run", run.ID), sink: sink, w: w, inputs: inputs, images: images, dir: dir,
		github: githubContext(run, payload, number, attempt), maxParallel: e.MaxParallel, secrets: secretValues, vars: vars,
		installation: ev.InstallationID}
	if x.maxParallel < 1 {
		x.maxParallel = 8
	}
	// A workflow with `concurrency:` waits here for the run before it in the
	// same group (or cancels it); the source is fetched after the wait, so a
	// pull request's merge ref is the freshest one.
	group, cancelRunning, cerr := concurrencyOf(w, x)
	if cerr != nil {
		os.RemoveAll(dir)
		return nil, cerr
	}
	if group != "" {
		self := holder{id: run.ID, desc: fmt.Sprintf("run %d", run.ID), cancel: func() { e.Cancel(run.ID) }}
		if release, err = e.enterGroup(ctx, groupKey("run", run.Repo, group), group, self, cancelRunning, true, sink); err != nil {
			os.RemoveAll(dir)
			return nil, err
		}
		x.release = release
	}
	x.sourceTar = filepath.Join(dir, "_source.tar.gz")
	f, err := os.Create(x.sourceTar)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	maxSource := e.MaxSourceBytes
	if maxSource <= 0 {
		maxSource = defaultMaxSource
	}
	what := run.SHA
	err = nil
	if run.Event == "pull_request" && run.PullRequest > 0 {
		// GitHub runs a pull request on the merge of its head into the base;
		// that ref appears a moment after the event, and does not exist when the
		// two conflict. Try it a few times, then run the head and say so.
		ref := fmt.Sprintf("refs/pull/%d/merge", run.PullRequest)
		err = e.fetchSource(ctx, f, ev.InstallationID, run.Repo, ref, maxSource, e.mergeTries(), e.mergeWait())
		if err == nil {
			what = ref
		} else if ctx.Err() == nil {
			sink.Add(runner.Event{Kind: runner.Warning, Time: time.Now(),
				Text: fmt.Sprintf("the merge of this pull request could not be fetched (%v): running its head commit %s instead", err, run.SHA[:min(7, len(run.SHA))])})
			err = nil
		}
	}
	if what == run.SHA && err == nil {
		err = e.fetchSource(ctx, f, ev.InstallationID, run.Repo, run.SHA, maxSource, 1, 0)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("fetching %s at %s: %w", run.Repo, what, err)
	}
	return x, nil
}

func (e *Executor) mergeTries() int {
	if e.MergeTries < 1 {
		return 4
	}
	return e.MergeTries
}

func (e *Executor) mergeWait() time.Duration {
	if e.MergeWait <= 0 {
		return 2 * time.Second
	}
	return e.MergeWait
}

// fetchSource writes the source of ref to f, trying up to tries times while
// GitHub answers "not found" (a ref that is still being made), wait apart.
// Anything else, or the size limit, ends it at once. The file is emptied
// before each try.
func (e *Executor) fetchSource(ctx context.Context, f *os.File, installation int64, repo, ref string, max int64, tries int, wait time.Duration) error {
	var err error
	for i := 0; i < tries; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		if err = f.Truncate(0); err != nil {
			return err
		}
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		err = e.Source.SourceTar(ctx, installation, repo, ref, &cappedWriter{w: f, max: max})
		if err == nil || !errors.Is(err, github.ErrNotFound) {
			return err
		}
	}
	return err
}

func parseObject(s string) (value.Object, error) {
	if strings.TrimSpace(s) == "" {
		return value.Object{}, nil
	}
	v, err := value.FromJSON([]byte(s))
	if err != nil {
		return nil, err
	}
	o, _ := v.(value.Object)
	if o == nil {
		o = value.Object{}
	}
	return o, nil
}

// runInstance is engine.Options.Remote: it sends one job instance to a
// machine, stores what the machine says, and returns how it ended.
func (x *execution) runInstance(ctx context.Context, inst engine.RemoteInstance) (runner.Result, error) {
	// A job's own `concurrency:` is entered before the job starts, so a job
	// that waits has no row, check run or machine yet. jobCtx is cancelled
	// when a newer job of the group takes this one's place.
	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()
	group, cancelRunning, err := jobConcurrencyOf(jobInstance{inst.Job.Concurrency, inst.Needs, inst.Strategy, inst.Matrix}, x)
	if err != nil {
		return runner.Result{}, err
	}
	if group != "" {
		self := holder{id: x.e.holderSeq.Add(1), desc: fmt.Sprintf("job %s of run %d", inst.Name, x.run.ID), cancel: cancelJob}
		release, err := x.e.enterGroup(ctx, groupKey("job", x.run.Repo, group), group, self, cancelRunning, false, x.sink)
		switch {
		case errors.Is(err, errSuperseded):
			x.sink.Add(runner.Event{Kind: runner.Notice, Time: time.Now(), Job: inst.Name, Text: err.Error()})
			return runner.Result{Conclusion: runner.Cancelled}, nil
		case err != nil:
			return runner.Result{}, err
		}
		defer release()
	}
	job, err := x.e.Store.StartJob(ctx, x.run.ID, inst.Name)
	if err != nil {
		return runner.Result{}, err
	}
	conclusion := runner.Failure
	check := x.startCheck(inst.Name)
	notes := &annotations{} // what the steps reported, for the check run
	var jobSummary string   // $GITHUB_STEP_SUMMARY, for the check run
	defer func() {
		if ctx.Err() != nil {
			check.finish(runner.Cancelled, jobSummary, notes)
		} else {
			check.finish(conclusion, jobSummary, notes)
		}
		if err := x.e.Store.FinishJob(context.Background(), job.ID, string(conclusion)); err != nil {
			x.log.Error("recording the end of a job failed", "job", inst.Name, "err", err)
		}
	}()

	contexts, err := json.Marshal(value.Object{
		{Key: "github", Value: x.github},
		{Key: "matrix", Value: orEmpty(inst.Matrix)},
		{Key: "strategy", Value: orEmpty(inst.Strategy)},
		{Key: "needs", Value: orEmpty(inst.Needs)},
		{Key: "inputs", Value: orEmpty(x.inputs)},
		{Key: "vars", Value: x.vars},
	})
	if err != nil {
		return runner.Result{}, err
	}
	n := x.n.Add(1)
	// The service token's job id is unique per instance (a matrix has many
	// with one job id) and holds only what a token may.
	var svcEnv map[string]string
	if x.e.Services != nil {
		if svcEnv, err = x.e.Services.JobEnv(x.run.ID, fmt.Sprintf("%s-%d", inst.Job.ID, n)); err != nil {
			return runner.Result{}, err
		}
	}
	// Only a job that asks for `id-token: write` can ask for ID tokens, and
	// never a pull request from a fork (as on GitHub).
	if x.e.IDTokens != nil && !x.run.Fork && wantsIDToken(x.w, inst.Job) {
		if svcEnv == nil {
			svcEnv = map[string]string{}
		}
		maps.Copy(svcEnv, x.e.IDTokens.Env(x.run.ID, fmt.Sprintf("%s-%d", inst.Job.ID, n)))
	}
	install := x.e.Install
	if install == "" {
		install = "all"
	}
	input, err := json.Marshal(wire.InstanceSpec{Workflow: x.run.WorkflowPath, Job: inst.Job.ID, Contexts: contexts, ServiceEnv: svcEnv, Install: install})
	if err != nil {
		return runner.Result{}, err
	}
	reader := &wire.Reader{Name: inst.Name, Event: func(e runner.Event) { x.sink.Add(e); notes.add(e) }}
	defer reader.Flush()
	secrets, revoke := x.secretsFor(ctx, inst)
	defer revoke() // the job's GITHUB_TOKEN stops working when the job is over
	res, err := x.e.Machine.Run(jobCtx, fmt.Sprintf("rc-r%d-%d", x.run.ID, n), dispatch.Job{
		Command: "run-instance",
		Input:   input,
		Secrets: secrets,
		Source: func(w io.Writer) error {
			f, err := os.Open(x.sourceTar)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(w, f)
			return err
		},
		Log: reader,
	})
	if err != nil {
		if jobCtx.Err() != nil && ctx.Err() == nil {
			// A newer job of the job's concurrency group cancelled it.
			conclusion = runner.Cancelled
			x.sink.Add(runner.Event{Kind: runner.Notice, Time: time.Now(), Job: inst.Name,
				Text: "this job was cancelled by a newer job of its concurrency group (cancel-in-progress)"})
			return runner.Result{Conclusion: runner.Cancelled}, nil
		}
		return runner.Result{}, err
	}
	if res.Error != "" {
		return runner.Result{}, errors.New("the machine could not run the job: " + res.Error)
	}
	reader.Flush()
	out, ok, err := reader.Result()
	switch {
	case err != nil:
		return runner.Result{}, fmt.Errorf("the machine's result: %w", err)
	case !ok:
		return runner.Result{}, fmt.Errorf("the machine exited with status %d without a result (its output is in the log)", res.ExitCode)
	}
	conclusion, jobSummary = out.Conclusion, out.Summary
	return out, nil
}

// secretsFor is the run's secrets plus, when the server can make one, the
// GITHUB_TOKEN of this job instance: a token for the run's repository alone,
// with the permissions the job (or workflow) asks for, read-only by default.
// A pull request from a fork gets none, like it gets no secrets. When the App
// lacks a permission the job asks for, the job gets the default token and a
// warning says so; when no token can be made the job runs without and a
// warning says so (the token itself is never in the log).
func (x *execution) secretsFor(ctx context.Context, inst engine.RemoteInstance) (secrets map[string]string, revoke func()) {
	revoke = func() {}
	if x.e.Tokens == nil || x.installation == 0 || x.run.Fork {
		return x.secrets, revoke
	}
	warn := func(format string, a ...any) {
		x.sink.Add(runner.Event{Kind: runner.Warning, Time: time.Now(), Job: inst.Name, Text: fmt.Sprintf(format, a...)})
	}
	want := tokenPermissions(x.w, inst.Job)
	tok, err := x.e.Tokens.RunToken(ctx, x.installation, x.run.Repo, want)
	if err != nil && !maps.Equal(want, defaultPermissions) {
		warn("the GitHub App cannot give the permissions this job asks for (%v): it gets a token with the default ones", err)
		tok, err = x.e.Tokens.RunToken(ctx, x.installation, x.run.Repo, defaultPermissions)
	}
	if err != nil {
		warn("no GITHUB_TOKEN could be made for this job: %v", err)
		return x.secrets, revoke
	}
	out := make(map[string]string, len(x.secrets)+1)
	maps.Copy(out, x.secrets)
	out["GITHUB_TOKEN"] = tok
	return out, func() {
		// Not with the job's context, which may be cancelled: that is when the
		// token matters most. A failure only means it expires by itself.
		rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := x.e.Tokens.RevokeToken(rctx, tok); err != nil {
			x.log.Warn("revoking a job's token failed (it expires by itself)", "job", inst.Name, "err", err)
		}
	}
}

func orEmpty(o value.Object) value.Object {
	if o == nil {
		return value.Object{}
	}
	return o
}

// githubContext builds the github context of a run from what is stored. The
// commit is the one the event is about: for a pull request its head (GitHub
// itself gives the merge commit; the server does not make one yet).
func githubContext(run store.Run, payload value.Object, number, attempt int) value.Object {
	owner, _, _ := strings.Cut(run.Repo, "/")
	refType, refName := "branch", run.Ref
	switch {
	case strings.HasPrefix(run.Ref, "refs/heads/"):
		refName = strings.TrimPrefix(run.Ref, "refs/heads/")
	case strings.HasPrefix(run.Ref, "refs/tags/"):
		refType, refName = "tag", strings.TrimPrefix(run.Ref, "refs/tags/")
	case strings.HasPrefix(run.Ref, "refs/pull/"):
		refName = strings.TrimPrefix(run.Ref, "refs/pull/")
	}
	var headRef, baseRef string
	if run.PullRequest > 0 {
		headRef, baseRef = dig(payload, "pull_request", "head", "ref"), dig(payload, "pull_request", "base", "ref")
	}
	id := strconv.FormatInt(run.ID, 10)
	name := run.WorkflowName
	if name == "" {
		name = run.WorkflowPath
	}
	return value.Object{
		{Key: "event_name", Value: run.Event},
		{Key: "event", Value: payload},
		{Key: "sha", Value: run.SHA},
		{Key: "ref", Value: run.Ref},
		{Key: "ref_name", Value: refName},
		{Key: "ref_type", Value: refType},
		{Key: "ref_protected", Value: "false"},
		{Key: "head_ref", Value: headRef},
		{Key: "base_ref", Value: baseRef},
		{Key: "repository", Value: run.Repo},
		{Key: "repository_owner", Value: owner},
		{Key: "actor", Value: run.Actor},
		{Key: "triggering_actor", Value: run.Actor},
		{Key: "workflow", Value: name},
		{Key: "workflow_ref", Value: run.Repo + "/" + run.WorkflowPath + "@" + run.Ref},
		{Key: "workflow_sha", Value: run.SHA},
		{Key: "run_id", Value: id},
		{Key: "run_number", Value: strconv.Itoa(number)},
		{Key: "run_attempt", Value: strconv.Itoa(attempt)},
		{Key: "retention_days", Value: "90"},
		{Key: "server_url", Value: "https://github.com"},
		{Key: "api_url", Value: "https://api.github.com"},
		{Key: "graphql_url", Value: "https://api.github.com/graphql"},
	}
}

// dig reads a string at a path of object keys, "" when it is not there.
func dig(o value.Object, path ...string) string {
	var cur any = o
	for _, p := range path {
		obj, ok := cur.(value.Object)
		if !ok {
			return ""
		}
		cur, ok = obj.Get(p)
		if !ok {
			return ""
		}
	}
	s, _ := cur.(string)
	return s
}
