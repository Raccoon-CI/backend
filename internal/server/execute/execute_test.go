package execute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Raccoon-CI/backend/internal/server/github"
	"github.com/Raccoon-CI/backend/internal/server/secrets"
	"github.com/Raccoon-CI/backend/internal/server/store"
	"github.com/Raccoon-CI/backend/internal/server/vault"
	"github.com/Raccoon-CI/lib/pkg/catalog"
	"github.com/Raccoon-CI/lib/pkg/dispatch"
	"github.com/Raccoon-CI/lib/pkg/runevent"
	"github.com/Raccoon-CI/lib/pkg/runnerimages"
	"github.com/Raccoon-CI/lib/pkg/value"
	"github.com/Raccoon-CI/lib/pkg/wire"
)

const twoJobs = `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    outputs:
      v: ${{ steps.s.outputs.v }}
    steps:
      - id: s
        run: echo "v=1" >> "$GITHUB_OUTPUT"
  test:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ needs.build.outputs.v }}
`

// call is what the fake machine saw for one job.
type call struct {
	name   string
	spec   wire.InstanceSpec
	job    dispatch.Job
	source []byte
}

// fakeMachine plays a machine: it reads the instance it is sent, as
// `raccoonci run-instance` would, and answers with wire lines.
type fakeMachine struct {
	mu     sync.Mutex
	calls  []call
	handle func(c call, out *wire.Encoder) (dispatch.Result, error)
	// hang makes a job wait until it is cancelled, closing started once it is there.
	hang    bool
	started chan struct{}
	once    sync.Once
}

func (m *fakeMachine) Run(ctx context.Context, name string, job dispatch.Job) (dispatch.Result, error) {
	var spec wire.InstanceSpec
	if err := json.Unmarshal(job.Input, &spec); err != nil {
		return dispatch.Result{}, err
	}
	var src bytes.Buffer
	if err := job.Source(&src); err != nil {
		return dispatch.Result{}, err
	}
	c := call{name: name, spec: spec, job: job, source: src.Bytes()}
	m.mu.Lock()
	m.calls = append(m.calls, c)
	m.mu.Unlock()
	if m.hang {
		m.once.Do(func() { close(m.started) })
		<-ctx.Done()
		return dispatch.Result{}, ctx.Err()
	}
	out := wire.NewEncoder(job.Log)
	if m.handle != nil {
		return m.handle(c, out)
	}
	return succeed(c, out)
}

func succeed(c call, out *wire.Encoder) (dispatch.Result, error) {
	now := time.Now()
	out.Write(wire.Line{Event: &runevent.Event{Kind: runevent.JobStarted, Time: now, Job: c.spec.Job}})
	out.Write(wire.Line{Event: &runevent.Event{Kind: runevent.Output, Time: now, Job: c.spec.Job, Step: "Run echo", Text: "hello from " + c.spec.Job}})
	out.Write(wire.Line{Event: &runevent.Event{Kind: runevent.JobFinished, Time: now, Job: c.spec.Job, Conclusion: runevent.Success}})
	out.Write(wire.Line{Result: &wire.Result{Conclusion: runevent.Success, Outputs: json.RawMessage(`{"v":"1"}`), DurationMS: 5}})
	return dispatch.Result{}, nil
}

type fakeSource struct {
	mu    sync.Mutex
	got   []string
	err   error
	bytes string
	// missing makes a ref answer "not found" that many times (negative: always).
	missing map[string]int
}

func (s *fakeSource) SourceTar(_ context.Context, inst int64, repo, sha string, w io.Writer) error {
	s.mu.Lock()
	s.got = append(s.got, fmt.Sprintf("%d %s %s", inst, repo, sha))
	s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.mu.Lock()
	if n := s.missing[sha]; n != 0 {
		if n > 0 {
			s.missing[sha] = n - 1
		}
		s.mu.Unlock()
		io.WriteString(w, "PARTIAL") // a failed try may leave something behind
		return github.ErrNotFound
	}
	s.mu.Unlock()
	out := s.bytes
	if strings.HasPrefix(sha, "refs/") {
		out += "@" + sha // a test can tell which ref the machine was given
	}
	_, err := io.WriteString(w, out)
	return err
}

type fakeServices struct{}

func (fakeServices) JobEnv(run int64, job string) (map[string]string, error) {
	return map[string]string{"ACTIONS_RESULTS_URL": "https://ci.example/", "ACTIONS_RUNTIME_TOKEN": fmt.Sprintf("tok-%d-%s", run, job)}, nil
}

type env struct {
	st  *store.SQLite
	src *fakeSource
	m   *fakeMachine
	ex  *Executor
	// installation is the App installation of queued events (default 42; 0 is a plain token).
	installation *int64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{st: st, src: &fakeSource{bytes: "TARBALL"}, m: &fakeMachine{}}
	e.ex = &Executor{Store: st, Source: e.src, Machine: e.m, Services: fakeServices{}, WorkDir: t.TempDir(), Poll: 10 * time.Millisecond}
	return e
}

func (e *env) queue(t *testing.T, delivery, workflowText string, edit func(*store.Run)) store.Run {
	t.Helper()
	inst := int64(42)
	if e.installation != nil {
		inst = *e.installation
	}
	ev, _, err := e.st.RecordEvent(context.Background(), store.Event{DeliveryID: delivery, Kind: "push", Repo: "acme/web", InstallationID: inst,
		Payload: []byte(`{"ref":"refs/heads/main","repository":{"full_name":"acme/web"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	r := store.Run{EventID: ev.ID, Repo: "acme/web", WorkflowPath: ".github/workflows/ci.yml", WorkflowName: "CI", Event: "push",
		Ref: "refs/heads/main", SHA: strings.Repeat("a", 40), Actor: "alice", WorkflowText: workflowText}
	if edit != nil {
		edit(&r)
	}
	run, _, err := e.st.CreateRun(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// start runs the executor until the test ends.
func (e *env) start(t *testing.T) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.ex.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return cancel
}

func (e *env) wait(t *testing.T, id int64) store.Run {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		r, err := e.st.GetRun(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status == store.RunCompleted {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Say where it is stuck, so that a rare hang under load can be told from a
	// run that never started.
	r, _ := e.st.GetRun(context.Background(), id)
	var tail []string
	lines, _ := e.st.ReadLog(context.Background(), id, 0, 10000)
	for _, l := range lines[max(0, len(lines)-8):] {
		tail = append(tail, l.Data)
	}
	jobs, _ := e.st.ListJobs(context.Background(), id)
	e.m.mu.Lock()
	machineCalls := len(e.m.calls)
	e.m.mu.Unlock()
	t.Fatalf("run %d did not finish: status %q, attempts %d, %d job rows, %d machine calls; last log lines: %v", id, r.Status, r.Attempts, len(jobs), machineCalls, tail)
	return store.Run{}
}

func (e *env) log(t *testing.T, id int64) []logEvent {
	t.Helper()
	lines, err := e.st.ReadLog(context.Background(), id, 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	var out []logEvent
	for _, l := range lines {
		var ev logEvent
		if err := json.Unmarshal([]byte(l.Data), &ev); err != nil {
			t.Fatalf("log line %q: %v", l.Data, err)
		}
		out = append(out, ev)
	}
	return out
}

func TestRunsWaitWhileTheDiskIsNearlyFullAndGoOnWhenSpaceReturns(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	free := int64(10 << 20)
	e.ex.MinFreeBytes = 100 << 20
	e.ex.Free = func() (int64, error) { mu.Lock(); defer mu.Unlock(); return free, nil }
	run := e.queue(t, "disk", twoJobs, nil)
	e.start(t)
	time.Sleep(300 * time.Millisecond)
	if r, _ := e.st.GetRun(context.Background(), run.ID); r.Status != store.RunQueued {
		t.Fatalf("a run was taken on a full disk: %q", r.Status)
	}
	mu.Lock()
	free = 1 << 30
	mu.Unlock()
	if done := e.wait(t, run.ID); done.Conclusion != "success" {
		t.Fatalf("conclusion %q", done.Conclusion)
	}
}

func TestTheImagesARunWasPlannedWithTravelWithEveryJob(t *testing.T) {
	e := newEnv(t)
	e.ex.Images = func(context.Context) (*runnerimages.Snapshot, error) {
		snap, err := runnerimages.Embedded()
		snap.Images["ubuntu24"].Version = "29990101.7" // what the server polled
		return snap, err
	}
	run := e.queue(t, "imgs", twoJobs, nil)
	e.start(t)
	e.wait(t, run.ID)
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	if len(e.m.calls) != 2 {
		t.Fatalf("%d jobs", len(e.m.calls))
	}
	for _, c := range e.m.calls {
		if c.spec.Protocol != wire.Version {
			t.Errorf("job %s: the assignment says protocol %d, want %d", c.spec.Job, c.spec.Protocol, wire.Version)
		}
		sent, err := runnerimages.ParseSnapshot(c.spec.Images)
		if err != nil {
			t.Fatalf("job %s: the images sent: %v", c.spec.Job, err)
		}
		if sent.Images["ubuntu24"].Version != "29990101.7" {
			t.Errorf("job %s got image version %s, want the one the run was planned with", c.spec.Job, sent.Images["ubuntu24"].Version)
		}
	}
}

func TestARunIsPlannedWithTheImagesTheServerPolledAndSurvivesAnUnreadableOne(t *testing.T) {
	e := newEnv(t)
	var asked int
	e.ex.Images = func(context.Context) (*runnerimages.Snapshot, error) {
		asked++
		snap, err := runnerimages.Embedded()
		if asked == 1 {
			return snap, err
		}
		return snap, errors.New("the database could not be read") // still a usable snapshot
	}
	e.start(t)
	for i, d := range []string{"img1", "img2"} {
		run := e.queue(t, d, twoJobs, nil)
		if done := e.wait(t, run.ID); done.Conclusion != "success" {
			t.Fatalf("run %d: conclusion %q", i, done.Conclusion)
		}
	}
	if asked != 2 {
		t.Fatalf("Images was asked %d times, want once per run", asked)
	}
}

func TestTheCatalogARunWasPlannedWithTravelsWithEveryJobAndAFailingOneFallsBack(t *testing.T) {
	const feed = "toolchains:\n  zig: {title: Zig, commands: [zig]}\n"
	e := newEnv(t)
	var asked int
	e.ex.Catalog = func(context.Context) (*catalog.Catalog, []byte, error) {
		asked++
		if asked == 2 {
			return catalog.Default(), nil, errors.New("the database could not be read")
		}
		cat, err := catalog.Default().Overlay([]byte(feed))
		return cat, []byte(feed), err
	}
	e.start(t)
	for i, d := range []string{"cat1", "cat2"} {
		run := e.queue(t, d, twoJobs, nil)
		if done := e.wait(t, run.ID); done.Conclusion != "success" {
			t.Fatalf("run %d: conclusion %q", i, done.Conclusion)
		}
	}
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	if len(e.m.calls) != 4 || asked != 2 {
		t.Fatalf("%d jobs, Catalog asked %d times (want 4, once per run)", len(e.m.calls), asked)
	}
	withFeed := 0
	for _, c := range e.m.calls {
		if c.spec.Catalog == feed {
			withFeed++
		} else if len(c.spec.Catalog) != 0 {
			t.Errorf("job %s sent catalog %q", c.spec.Job, c.spec.Catalog)
		}
	}
	if withFeed != 2 {
		t.Errorf("%d jobs carried the feed, want the 2 of the first run", withFeed)
	}
}

func TestWithoutACatalogSourceNothingIsSent(t *testing.T) {
	e := newEnv(t)
	run := e.queue(t, "nocat", twoJobs, nil)
	e.start(t)
	e.wait(t, run.ID)
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	for _, c := range e.m.calls {
		if len(c.spec.Catalog) != 0 {
			t.Errorf("job %s sent catalog %q", c.spec.Job, c.spec.Catalog)
		}
	}
}

func prRun(r *store.Run) { r.Event = "pull_request"; r.PullRequest = 7 }

func TestAPullRequestRunsOnItsMergeRefOnceGitHubHasMadeIt(t *testing.T) {
	e := newEnv(t)
	e.ex.MergeWait = time.Millisecond
	e.src.missing = map[string]int{"refs/pull/7/merge": 2} // 404 right after the event
	run := e.queue(t, "pr", twoJobs, prRun)
	e.start(t)
	if done := e.wait(t, run.ID); done.Conclusion != "success" {
		t.Fatalf("conclusion %q", done.Conclusion)
	}
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	if string(e.m.calls[0].source) != "TARBALL@refs/pull/7/merge" {
		t.Fatalf("the machine got %q (a failed try must not leave bytes behind)", e.m.calls[0].source)
	}
}

func TestAPullRequestWithoutAMergeRefRunsItsHeadAndSaysSo(t *testing.T) {
	e := newEnv(t)
	e.ex.MergeWait = time.Millisecond
	e.ex.MergeTries = 3
	e.src.missing = map[string]int{"refs/pull/7/merge": -1} // conflicts: never made
	run := e.queue(t, "pr", twoJobs, prRun)
	e.start(t)
	if done := e.wait(t, run.ID); done.Conclusion != "success" {
		t.Fatalf("conclusion %q", done.Conclusion)
	}
	e.m.mu.Lock()
	if string(e.m.calls[0].source) != "TARBALL" {
		t.Fatalf("the machine got %q, want the head's source", e.m.calls[0].source)
	}
	e.m.mu.Unlock()
	var warned bool
	for _, ev := range e.log(t, run.ID) {
		warned = warned || strings.Contains(ev.Text, "merge of this pull request could not be fetched")
	}
	if !warned {
		t.Fatalf("no warning in the log: %+v", e.log(t, run.ID))
	}
	e.src.mu.Lock()
	defer e.src.mu.Unlock()
	if n := len(e.src.got); n != 4 { // 3 tries at the merge ref, then the head
		t.Fatalf("source asked %d times: %v", n, e.src.got)
	}
}

func TestAPushIsNeverGivenAMergeRef(t *testing.T) {
	e := newEnv(t)
	run := e.queue(t, "push", twoJobs, nil)
	e.start(t)
	e.wait(t, run.ID)
	e.src.mu.Lock()
	defer e.src.mu.Unlock()
	if len(e.src.got) != 1 || strings.Contains(e.src.got[0], "refs/") {
		t.Fatalf("source asked for %v", e.src.got)
	}
}

func TestAnOversizeSourceFailsTheRunWithAClearMessage(t *testing.T) {
	e := newEnv(t)
	e.src.bytes = strings.Repeat("x", 4096)
	e.ex.MaxSourceBytes = 1024
	run := e.queue(t, "big", twoJobs, nil)
	e.start(t)
	done := e.wait(t, run.ID)
	if done.Conclusion != "failure" {
		t.Fatalf("conclusion %q", done.Conclusion)
	}
	var found bool
	for _, ev := range e.log(t, run.ID) {
		if strings.Contains(ev.Text, "bigger than the limit") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no message about the limit: %+v", e.log(t, run.ID))
	}
	if entries, _ := os.ReadDir(e.ex.WorkDir); len(entries) != 0 {
		t.Fatalf("the failed run left %d entries in the work directory", len(entries))
	}
}

func TestARunIsFetchedSentToMachinesAndLogged(t *testing.T) {
	e := newEnv(t)
	run := e.queue(t, "d1", twoJobs, nil)
	e.start(t)
	done := e.wait(t, run.ID)
	if done.Conclusion != "success" {
		t.Fatalf("conclusion %q; log: %+v", done.Conclusion, e.log(t, run.ID))
	}
	if len(e.src.got) != 1 || e.src.got[0] != "42 acme/web "+strings.Repeat("a", 40) {
		t.Fatalf("source fetched as %v, want the run's commit for installation 42", e.src.got)
	}
	if len(e.m.calls) != 2 {
		t.Fatalf("%d machine calls, want 2", len(e.m.calls))
	}
	byJob := map[string]call{}
	names := map[string]bool{}
	for _, c := range e.m.calls {
		byJob[c.spec.Job] = c
		names[c.name] = true
		if c.job.Command != "run-instance" || string(c.source) != "TARBALL" || c.spec.Workflow != ".github/workflows/ci.yml" || c.spec.Install != "all" {
			t.Errorf("call = %+v", c)
		}
		if !strings.HasPrefix(c.spec.ServiceEnv["ACTIONS_RUNTIME_TOKEN"], fmt.Sprintf("tok-%d-", run.ID)) {
			t.Errorf("service env = %v", c.spec.ServiceEnv)
		}
		if len(c.job.Secrets) != 0 {
			t.Errorf("a run without secrets sent some: %v", c.job.Secrets)
		}
	}
	if len(names) != 2 {
		t.Fatalf("machines must have unique names, got %v", names)
	}
	// The second job saw the first one's output.
	ctxv, err := value.FromJSON(byJob["test"].spec.Contexts)
	if err != nil {
		t.Fatal(err)
	}
	needs, _ := ctxv.(value.Object).Get("needs")
	build, _ := needs.(value.Object).Get("build")
	outputs, _ := build.(value.Object).Get("outputs")
	if v, _ := outputs.(value.Object).Get("v"); v != "1" {
		t.Fatalf("needs.build.outputs.v = %v, want 1 (contexts %s)", v, byJob["test"].spec.Contexts)
	}
	gh, _ := ctxv.(value.Object).Get("github")
	if sha, _ := gh.(value.Object).Get("sha"); sha != strings.Repeat("a", 40) {
		t.Fatalf("github.sha = %v", sha)
	}
	jobs, _ := e.st.ListJobs(context.Background(), run.ID)
	if len(jobs) != 2 || jobs[0].Status != store.RunCompleted || jobs[0].Conclusion != "success" || jobs[1].Conclusion != "success" {
		t.Fatalf("jobs = %+v", jobs)
	}
	var hello int
	for _, ev := range e.log(t, run.ID) {
		if ev.Kind == "output" && strings.HasPrefix(ev.Text, "hello from ") {
			hello++
		}
	}
	if hello != 2 {
		t.Fatalf("%d output lines in the log, want 2: %+v", hello, e.log(t, run.ID))
	}
	if entries, _ := filepath.Glob(filepath.Join(e.ex.WorkDir, "run-*")); len(entries) != 0 {
		t.Fatalf("the run's working files were left: %v", entries)
	}
}

func TestAFailingJobFailsTheRunAndStopsWhatNeedsIt(t *testing.T) {
	e := newEnv(t)
	e.m.handle = func(c call, out *wire.Encoder) (dispatch.Result, error) {
		out.Write(wire.Line{Event: &runevent.Event{Kind: runevent.Error, Time: time.Now(), Job: c.spec.Job, Text: "boom"}})
		out.Write(wire.Line{Result: &wire.Result{Conclusion: runevent.Failure, Outputs: json.RawMessage(`{}`)}})
		return dispatch.Result{ExitCode: 0}, nil
	}
	run := e.queue(t, "d", twoJobs, nil)
	e.start(t)
	done := e.wait(t, run.ID)
	if done.Conclusion != "failure" {
		t.Fatalf("conclusion %q", done.Conclusion)
	}
	if len(e.m.calls) != 1 {
		t.Fatalf("%d machines used, want only build's: test needs it and must not run", len(e.m.calls))
	}
	jobs, _ := e.st.ListJobs(context.Background(), run.ID)
	if len(jobs) != 1 || jobs[0].Conclusion != "failure" {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestAMachineThatFailsOrSaysNothingFailsTheInstanceWithAReason(t *testing.T) {
	cases := map[string]func(c call, out *wire.Encoder) (dispatch.Result, error){
		"error": func(call, *wire.Encoder) (dispatch.Result, error) {
			return dispatch.Result{}, errors.New("no machine could be made")
		},
		"agent error": func(call, *wire.Encoder) (dispatch.Result, error) { return dispatch.Result{Error: "cannot start"}, nil },
		"no result":   func(call, *wire.Encoder) (dispatch.Result, error) { return dispatch.Result{ExitCode: 2}, nil },
		"plain output": func(c call, out *wire.Encoder) (dispatch.Result, error) {
			c.job.Log.Write([]byte("panic: oops\n"))
			return dispatch.Result{ExitCode: 2}, nil
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.m.handle = h
			run := e.queue(t, "d", twoJobs, nil)
			e.start(t)
			if done := e.wait(t, run.ID); done.Conclusion != "failure" {
				t.Fatalf("conclusion %q", done.Conclusion)
			}
			var errText string
			for _, ev := range e.log(t, run.ID) {
				if ev.Kind == "error" {
					errText += ev.Text
				}
			}
			if errText == "" {
				t.Fatalf("no error in the log: %+v", e.log(t, run.ID))
			}
			jobs, _ := e.st.ListJobs(context.Background(), run.ID)
			if len(jobs) == 0 || jobs[0].Conclusion != "failure" || jobs[0].CompletedAt.IsZero() {
				t.Fatalf("jobs = %+v", jobs)
			}
		})
	}
}

func TestARunWhoseSourceCannotBeFetchedFailsWithoutAMachine(t *testing.T) {
	e := newEnv(t)
	e.src.err = errors.New("repository not found")
	run := e.queue(t, "d", twoJobs, nil)
	e.start(t)
	done := e.wait(t, run.ID)
	if done.Conclusion != "failure" || len(e.m.calls) != 0 {
		t.Fatalf("conclusion %q, %d machines", done.Conclusion, len(e.m.calls))
	}
	if log := e.log(t, run.ID); len(log) != 1 || !strings.Contains(log[0].Text, "repository not found") {
		t.Fatalf("log = %+v", log)
	}
}

func TestAnInvalidStoredWorkflowFailsTheRun(t *testing.T) {
	e := newEnv(t)
	run := e.queue(t, "d", "on: push\njobs: {}\nbogus: 1\n", nil)
	e.start(t)
	if done := e.wait(t, run.ID); done.Conclusion != "failure" {
		t.Fatalf("conclusion %q", done.Conclusion)
	}
	if len(e.src.got) != 0 {
		t.Fatal("the source was fetched for a workflow that cannot run")
	}
}

func TestRunsLeftInProgressByAStoppedServerAreRunAgain(t *testing.T) {
	e := newEnv(t)
	left := e.queue(t, "left", twoJobs, nil)
	if _, ok, _ := e.st.ClaimRun(context.Background()); !ok { // a previous process claimed it and died
		t.Fatal("setup")
	}
	e.m.handle = func(c call, out *wire.Encoder) (dispatch.Result, error) { return succeed(c, out) }
	e.start(t)
	got := e.wait(t, left.ID)
	if got.Conclusion != "success" || got.Attempts != 2 || len(e.m.calls) == 0 {
		t.Fatalf("interrupted run: %q after %d attempts, %d machine calls; it must be run again", got.Conclusion, got.Attempts, len(e.m.calls))
	}
	var said bool
	for _, l := range e.log(t, left.ID) {
		said = said || (l.Kind == "warning" && strings.Contains(l.Text, "attempt 2"))
	}
	if !said {
		t.Fatal("the log must say the run was started again")
	}
}

func TestStartRemovesWhatARunThatDiedLeftInTheWorkDirectories(t *testing.T) {
	e := newEnv(t)
	local := t.TempDir()
	e.ex.Machine = Local{Binary: "/bin/true", Dir: local}
	for _, p := range []string{filepath.Join(e.ex.WorkDir, "run-7", "_source.tar.gz"), filepath.Join(local, "rc-r7-1-123", "src", "f")} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e.start(t)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a, _ := os.ReadDir(e.ex.WorkDir)
		b, _ := os.ReadDir(local)
		if len(a) == 0 && len(b) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if a, _ := os.ReadDir(e.ex.WorkDir); len(a) != 0 {
		t.Fatalf("work directory still holds %v", a)
	}
	if b, _ := os.ReadDir(local); len(b) != 0 {
		t.Fatalf("the local runner's directory still holds %v", b)
	}
	// The directories themselves stay: they are the server's to use.
	if _, err := os.Stat(e.ex.WorkDir); err != nil {
		t.Fatal(err)
	}
}

func TestARunTakenTooOftenEndsAsFailedInsteadOfLoopingForEver(t *testing.T) {
	e := newEnv(t)
	e.ex.MaxAttempts = 1
	left := e.queue(t, "left", twoJobs, nil)
	if _, ok, _ := e.st.ClaimRun(context.Background()); !ok {
		t.Fatal("setup")
	}
	e.m.handle = func(c call, out *wire.Encoder) (dispatch.Result, error) { return succeed(c, out) }
	e.start(t)
	got := e.wait(t, left.ID)
	if got.Conclusion != "failure" || len(e.m.calls) != 0 {
		t.Fatalf("run: %q, %d machine calls; it has used its attempts and must be ended", got.Conclusion, len(e.m.calls))
	}
}

func TestStoppingTheServerQueuesRunsInProgressAgainForTheNextStart(t *testing.T) {
	e := newEnv(t)
	e.m.hang = true
	e.m.started = make(chan struct{})
	run := e.queue(t, "d", twoJobs, nil)
	stop := e.start(t)
	select {
	case <-e.m.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the job never reached a machine")
	}
	stop()
	deadline := time.Now().Add(10 * time.Second)
	var got store.Run
	for time.Now().Before(deadline) {
		got, _ = e.st.GetRun(context.Background(), run.ID)
		if got.Status == store.RunQueued {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.Status != store.RunQueued || got.Conclusion != "" {
		t.Fatalf("run = %+v: stopped with the server, it must wait in the queue, not end as cancelled", got)
	}
	if jobs, _ := e.st.ListJobs(context.Background(), run.ID); len(jobs) != 0 {
		t.Fatalf("jobs = %+v: the attempt that stopped leaves none behind", jobs)
	}
	// The next start runs it.
	e.m.hang = false
	e.m.handle = func(c call, out *wire.Encoder) (dispatch.Result, error) { return succeed(c, out) }
	e2 := &Executor{Store: e.st, Source: e.src, Machine: e.m, Services: fakeServices{}, WorkDir: t.TempDir(), Poll: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e2.Run(ctx) }()
	defer func() { cancel(); <-done }()
	if end := e.wait(t, run.ID); end.Conclusion != "success" || end.Attempts != 2 {
		t.Fatalf("after the next start: %q, %d attempts", end.Conclusion, end.Attempts)
	}
}

type fakeReporter struct {
	mu          sync.Mutex
	got         []string
	pendingWait time.Duration
	err         error
}

func (f *fakeReporter) Report(_ context.Context, inst int64, run store.Run, state, desc string) error {
	if state == "pending" {
		time.Sleep(f.pendingWait)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, fmt.Sprintf("%d %s %s %s %s", inst, run.SHA[:4], state, strings.Fields(desc)[0], run.WorkflowName))
	return f.err
}

func (f *fakeReporter) states() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

type fakeChecks struct {
	mu      sync.Mutex
	got     []string
	updates []github.CheckRun // what each completed check run was given
	next    int64
	fail    bool
}

func (f *fakeChecks) CreateCheckRun(_ context.Context, inst int64, repo, sha string, r github.CheckRun) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return 0, errors.New("no checks permission")
	}
	f.got = append(f.got, fmt.Sprintf("create %d %s %s %s", inst, repo, r.Name, r.Status))
	f.next++
	return f.next, nil
}

func (f *fakeChecks) UpdateCheckRun(_ context.Context, inst int64, repo string, id int64, r github.CheckRun) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, fmt.Sprintf("update %d %s %s", id, r.Name, r.Conclusion))
	f.updates = append(f.updates, r)
	return nil
}

func (f *fakeChecks) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

func waitLines(f *fakeChecks, n int) []string {
	deadline := time.Now().Add(5 * time.Second)
	for len(f.lines()) < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return f.lines()
}

func TestEachJobGetsACheckRunCompletedWithItsConclusion(t *testing.T) {
	e := newEnv(t)
	checks := &fakeChecks{}
	e.ex.Checks = checks
	e.m.handle = func(c call, out *wire.Encoder) (dispatch.Result, error) {
		conclusion := runevent.Success
		if c.spec.Job == "test" {
			conclusion = runevent.Failure
		}
		out.Write(wire.Line{Result: &wire.Result{Conclusion: conclusion, Outputs: json.RawMessage(`{}`)}})
		return dispatch.Result{}, nil
	}
	run := e.queue(t, "chk", twoJobs, nil)
	e.start(t)
	e.wait(t, run.ID)
	got := strings.Join(waitLines(checks, 4), "\n")
	for _, want := range []string{
		"create 42 acme/web raccoonci / CI / build in_progress",
		"update 1 raccoonci / CI / build success",
		"create 42 acme/web raccoonci / CI / test in_progress",
		"update 2 raccoonci / CI / test failure",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestNoCheckRunsWithoutAnInstallationAndAFailingOneDoesNotFailTheRun(t *testing.T) {
	e := newEnv(t)
	checks := &fakeChecks{}
	e.ex.Checks = checks
	// An event without an installation (a plain token) cannot write check runs.
	none := int64(0)
	e.installation = &none
	run := e.queue(t, "tok", twoJobs, nil)
	e.start(t)
	e.wait(t, run.ID)
	time.Sleep(100 * time.Millisecond)
	if got := checks.lines(); len(got) != 0 {
		t.Fatalf("check runs without an installation: %v", got)
	}

	e2 := newEnv(t)
	e2.ex.Checks = &fakeChecks{fail: true}
	run2 := e2.queue(t, "fail", twoJobs, nil)
	e2.start(t)
	if end := e2.wait(t, run2.ID); end.Conclusion != "success" {
		t.Fatalf("a failing check run must not fail the run: %q", end.Conclusion)
	}
}

func TestARunIsReportedPendingThenItsEndInThatOrderWhateverTheSpeed(t *testing.T) {
	e := newEnv(t)
	rep := &fakeReporter{pendingWait: 300 * time.Millisecond} // a slow pending must not land after the end
	e.ex.Reporter = rep
	run := e.queue(t, "d", twoJobs, nil)
	e.start(t)
	e.wait(t, run.ID)
	deadline := time.Now().Add(5 * time.Second)
	for len(rep.states()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := rep.states()
	want := []string{"42 aaaa pending Running CI", "42 aaaa success Succeeded CI"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("reports = %v, want %v", got, want)
	}
}

func TestAFailedRunIsReportedFailedAndGitHubErrorsDoNotFailRuns(t *testing.T) {
	e := newEnv(t)
	rep := &fakeReporter{err: errors.New("github is down")}
	e.ex.Reporter = rep
	e.m.handle = func(c call, out *wire.Encoder) (dispatch.Result, error) {
		out.Write(wire.Line{Result: &wire.Result{Conclusion: runevent.Failure, Outputs: json.RawMessage(`{}`)}})
		return dispatch.Result{}, nil
	}
	run := e.queue(t, "d", twoJobs, nil)
	e.start(t)
	done := e.wait(t, run.ID)
	if done.Conclusion != "failure" {
		t.Fatalf("conclusion %q", done.Conclusion)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(rep.states()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := rep.states(); len(got) != 2 || !strings.Contains(got[1], "failure Failed") {
		t.Fatalf("reports = %v", got)
	}
}

func TestCancelEndsOneRunAndOnlyAnActiveOne(t *testing.T) {
	e := newEnv(t)
	e.m.hang = true
	e.m.started = make(chan struct{})
	rep := &fakeReporter{}
	e.ex.Reporter = rep
	run := e.queue(t, "d", twoJobs, nil)
	e.start(t)
	select {
	case <-e.m.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the job never reached a machine")
	}
	if !e.ex.Cancel(run.ID) {
		t.Fatal("a running run could not be cancelled")
	}
	done := e.wait(t, run.ID)
	if done.Conclusion != "cancelled" {
		t.Fatalf("conclusion %q", done.Conclusion)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(rep.states()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := rep.states(); len(got) != 2 || !strings.Contains(got[1], "error Cancelled") {
		t.Fatalf("reports = %v", got)
	}
	if e.ex.Cancel(run.ID) || e.ex.Cancel(999) {
		t.Fatal("Cancel said it stopped a run that is not running")
	}
}

const withSecret = `on: push
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: echo "${{ vars.REGION }}"
        env:
          TOKEN: ${{ secrets.DEPLOY_TOKEN }}
`

func secretsFor(t *testing.T, e *env) {
	t.Helper()
	v, err := vault.New(map[string][]byte{"1": bytes.Repeat([]byte{1}, 32)}, "1")
	if err != nil {
		t.Fatal(err)
	}
	svc := &secrets.Service{Store: e.st, Vault: v}
	ctx := context.Background()
	svc.SetSecret(ctx, secrets.Repo("acme/web"), "DEPLOY_TOKEN", "hunter2")
	svc.SetSecret(ctx, secrets.Repo("acme/web"), "UNUSED", "not sent")
	svc.SetVar(ctx, secrets.Org("acme"), "REGION", "eu")
	e.ex.Secrets = svc
}

func TestARunSendsMachinesTheSecretsItNamesAndItsVariables(t *testing.T) {
	e := newEnv(t)
	secretsFor(t, e)
	run := e.queue(t, "d", withSecret, nil)
	e.start(t)
	if done := e.wait(t, run.ID); done.Conclusion != "success" {
		t.Fatalf("conclusion %q", done.Conclusion)
	}
	if len(e.m.calls) != 1 {
		t.Fatalf("%d calls", len(e.m.calls))
	}
	c := e.m.calls[0]
	if len(c.job.Secrets) != 1 || c.job.Secrets["DEPLOY_TOKEN"] != "hunter2" {
		t.Fatalf("secrets sent = %v: only the one the workflow names", c.job.Secrets)
	}
	ctxv, _ := value.FromJSON(c.spec.Contexts)
	vars, _ := ctxv.(value.Object).Get("vars")
	if v, _ := vars.(value.Object).Get("REGION"); v != "eu" {
		t.Fatalf("vars = %v", vars)
	}
	// The secret is in the assignment's secrets, not in the instance description.
	if strings.Contains(string(c.job.Input), "hunter2") {
		t.Fatal("a secret's value is in the instance's input")
	}
	for _, ev := range e.log(t, run.ID) {
		if strings.Contains(ev.Text, "hunter2") {
			t.Fatalf("a secret's value is in the log: %+v", ev)
		}
	}
}

func TestAForkRunGetsNoSecretsAndSaysSo(t *testing.T) {
	e := newEnv(t)
	secretsFor(t, e)
	run := e.queue(t, "d", withSecret, func(r *store.Run) { r.Fork = true; r.Event = "pull_request"; r.PullRequest = 9 })
	e.start(t)
	e.wait(t, run.ID)
	if len(e.m.calls) != 1 || len(e.m.calls[0].job.Secrets) != 0 {
		t.Fatalf("a fork's run sent secrets: %v", e.m.calls[0].job.Secrets)
	}
	said := false
	for _, ev := range e.log(t, run.ID) {
		if ev.Kind == "notice" && strings.Contains(ev.Text, "no secrets") {
			said = true
		}
	}
	if !said {
		t.Fatalf("the log does not say the run gets no secrets: %+v", e.log(t, run.ID))
	}
}

func TestOnlyMaxRunsRunAtOnce(t *testing.T) {
	e := newEnv(t)
	e.ex.MaxRuns = 1
	var mu sync.Mutex
	active, peak := 0, 0
	e.m.handle = func(c call, out *wire.Encoder) (dispatch.Result, error) {
		mu.Lock()
		active++
		peak = max(peak, active)
		mu.Unlock()
		time.Sleep(60 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return succeed(c, out)
	}
	single := "on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n"
	a := e.queue(t, "a", single, func(r *store.Run) { r.WorkflowPath = "a.yml" })
	b := e.queue(t, "b", single, func(r *store.Run) { r.WorkflowPath = "b.yml" })
	e.start(t)
	e.wait(t, a.ID)
	e.wait(t, b.ID)
	if peak != 1 {
		t.Fatalf("%d runs ran at once with MaxRuns 1", peak)
	}
}

func TestGithubContext(t *testing.T) {
	get := func(o value.Object, k string) any { v, _ := o.Get(k); return v }
	push := githubContext(store.Run{ID: 9, Repo: "acme/web", WorkflowPath: ".github/workflows/ci.yml", WorkflowName: "CI", Event: "push",
		Ref: "refs/heads/feature/x", SHA: "abc", Actor: "alice"}, value.Object{}, 4, 2)
	for k, want := range map[string]string{"event_name": "push", "ref_name": "feature/x", "ref_type": "branch", "repository_owner": "acme",
		"run_id": "9", "run_number": "4", "run_attempt": "2", "workflow": "CI", "workflow_ref": "acme/web/.github/workflows/ci.yml@refs/heads/feature/x", "actor": "alice", "head_ref": ""} {
		if got := get(push, k); got != want {
			t.Errorf("push %s = %v, want %q", k, got, want)
		}
	}
	tag := githubContext(store.Run{Repo: "a/b", Ref: "refs/tags/v1.0", Event: "push", WorkflowPath: "w.yml"}, value.Object{}, 1, 1)
	if get(tag, "ref_type") != "tag" || get(tag, "ref_name") != "v1.0" || get(tag, "workflow") != "w.yml" {
		t.Errorf("tag = %v %v %v", get(tag, "ref_type"), get(tag, "ref_name"), get(tag, "workflow"))
	}
	payload, _ := value.FromJSON([]byte(`{"pull_request":{"head":{"ref":"feature"},"base":{"ref":"main"}}}`))
	pr := githubContext(store.Run{Repo: "a/b", Ref: "refs/pull/7/merge", Event: "pull_request", PullRequest: 7}, payload.(value.Object), 1, 1)
	if get(pr, "head_ref") != "feature" || get(pr, "base_ref") != "main" || get(pr, "ref_name") != "7/merge" {
		t.Errorf("pr = head %v base %v ref_name %v", get(pr, "head_ref"), get(pr, "base_ref"), get(pr, "ref_name"))
	}
	if get(pr, "event") == nil {
		t.Error("the event payload is missing")
	}
}

func TestEncodeEventUsesNamesNotNumbers(t *testing.T) {
	var ev logEvent
	if err := json.Unmarshal([]byte(EncodeEvent(runevent.Event{Kind: runevent.StepFinished, Job: "j", Step: "s", Conclusion: runevent.Success})), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Kind != "step_finished" || ev.Job != "j" || ev.Conclusion != "success" || ev.Time.IsZero() {
		t.Fatalf("event = %+v", ev)
	}
}
