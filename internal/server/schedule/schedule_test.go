package schedule

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Raccoon-CI/backend/internal/server/github"
	"github.com/Raccoon-CI/backend/internal/server/store"
)

type fakeSource struct {
	branch string
	sha    string
	files  map[string]string // path -> content, at every sha
	fail   error
	looks  int // WorkflowFiles calls
}

func (f *fakeSource) DefaultBranch(context.Context, int64, string) (string, error) {
	return f.branch, f.fail
}

func (f *fakeSource) ResolveRef(_ context.Context, _ int64, _, ref string) (string, error) {
	if f.fail != nil {
		return "", f.fail
	}
	if ref != "refs/heads/"+f.branch {
		return "", github.ErrNotFound
	}
	return f.sha, nil
}

func (f *fakeSource) WorkflowFiles(context.Context, int64, string, string) ([]github.File, error) {
	f.looks++
	var out []github.File
	for p, c := range f.files {
		out = append(out, github.File{Path: p, Content: []byte(c)})
	}
	return out, f.fail
}

func (f *fakeSource) FileAt(_ context.Context, _ int64, _, p, _ string) ([]byte, error) {
	c, ok := f.files[p]
	if !ok {
		return nil, github.ErrNotFound
	}
	return []byte(c), nil
}

const nightly = `name: Nightly
on:
  schedule:
    - cron: '30 2 * * *'
    - cron: '0 12 * * 1'
  push:
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`

const pushOnly = `on: push
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`

type env struct {
	st  *store.SQLite
	src *fakeSource
	s   *Scheduler
	n   int // notifications
}

func newEnv(t *testing.T, files map[string]string) *env {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{st: st, src: &fakeSource{branch: "main", sha: strings.Repeat("a", 40), files: files}}
	e.s = &Scheduler{Store: st, Source: e.src, Allowed: func(r string) bool { return strings.HasPrefix(r, "acme/") }, Notify: func() { e.n++ }}
	// the repository is known because an event named it
	if _, _, err := st.RecordEvent(context.Background(), store.Event{DeliveryID: "d1", Kind: "push", Repo: "acme/web", InstallationID: 7, Payload: []byte("{}"), Status: store.EventIgnored}); err != nil {
		t.Fatal(err)
	}
	return e
}

func at(s string) time.Time {
	v, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		panic(err)
	}
	return v
}

func (e *env) runs(t *testing.T) []store.Run {
	t.Helper()
	runs, err := e.st.ListRuns(context.Background(), store.RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func TestFiresAtTheMinuteOnTheDefaultBranch(t *testing.T) {
	e := newEnv(t, map[string]string{".github/workflows/nightly.yml": nightly, ".github/workflows/ci.yml": pushOnly})
	ctx := context.Background()
	e.s.Tick(ctx, at("2026-11-02 02:29:00")) // first tick only notes the time
	e.s.Tick(ctx, at("2026-11-02 02:29:45"))
	if len(e.runs(t)) != 0 {
		t.Fatal("nothing is due before 02:30")
	}
	e.s.Tick(ctx, at("2026-11-02 02:30:10"))
	runs := e.runs(t)
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	r := runs[0]
	if r.Event != "schedule" || r.Ref != "refs/heads/main" || r.SHA != e.src.sha || r.Repo != "acme/web" ||
		r.WorkflowPath != ".github/workflows/nightly.yml" || r.WorkflowName != "Nightly" || r.Status != store.RunQueued || r.Fork {
		t.Fatalf("run = %+v", r)
	}
	full, err := e.st.GetRun(ctx, r.ID)
	if err != nil || !strings.Contains(full.WorkflowText, "schedule") {
		t.Fatalf("the run must keep the workflow text: %v", err)
	}
	ev, err := e.st.GetEvent(ctx, r.EventID)
	if err != nil || ev.Kind != "schedule" || !strings.Contains(string(ev.Payload), `"schedule":"30 2 * * *"`) || ev.InstallationID != 7 {
		t.Fatalf("event = %+v, %v", ev, err)
	}
	if e.n != 1 {
		t.Fatalf("notified %d times", e.n)
	}
	// the same minute again, and the minutes after, start nothing
	e.s.Tick(ctx, at("2026-11-02 02:30:40"))
	e.s.Tick(ctx, at("2026-11-02 02:35:00"))
	if len(e.runs(t)) != 1 {
		t.Fatal("a fire must start one run")
	}
}

func TestTheOtherCronOfTheSameFileIsItsOwnRun(t *testing.T) {
	e := newEnv(t, map[string]string{".github/workflows/nightly.yml": nightly})
	ctx := context.Background()
	e.s.Tick(ctx, at("2026-11-02 11:59:00")) // a Monday
	e.s.Tick(ctx, at("2026-11-02 12:00:05"))
	runs := e.runs(t)
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	ev, _ := e.st.GetEvent(ctx, runs[0].EventID)
	if !strings.Contains(string(ev.Payload), `"schedule":"0 12 * * 1"`) {
		t.Fatalf("payload = %s", ev.Payload)
	}
}

func TestAStallMakesOneRunNotABurst(t *testing.T) {
	e := newEnv(t, map[string]string{".github/workflows/every.yml": "on:\n  schedule:\n    - cron: '*/5 * * * *'\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: x\n"})
	ctx := context.Background()
	e.s.Tick(ctx, at("2026-11-02 10:00:00"))
	e.s.Tick(ctx, at("2026-11-02 10:42:00")) // eight fires passed
	if got := len(e.runs(t)); got != 1 {
		t.Fatalf("%d runs", got)
	}
}

func TestAFireAlreadyRecordedIsNotRepeated(t *testing.T) {
	// A restart: a second scheduler that saw the same minute finds the event
	// of the first and starts nothing more.
	e := newEnv(t, map[string]string{".github/workflows/nightly.yml": nightly})
	ctx := context.Background()
	e.s.Tick(ctx, at("2026-11-02 02:29:00"))
	e.s.Tick(ctx, at("2026-11-02 02:30:10"))
	again := &Scheduler{Store: e.st, Source: e.src}
	again.Tick(ctx, at("2026-11-02 02:29:30"))
	again.Tick(ctx, at("2026-11-02 02:30:20"))
	if got := len(e.runs(t)); got != 1 {
		t.Fatalf("%d runs", got)
	}
}

func TestTheEntryMustStillBeThereAtTheNewestCommit(t *testing.T) {
	e := newEnv(t, map[string]string{".github/workflows/nightly.yml": nightly})
	ctx := context.Background()
	e.s.Tick(ctx, at("2026-11-02 02:29:00"))
	e.s.Tick(ctx, at("2026-11-02 02:29:30"))                // learns the schedules
	e.src.files[".github/workflows/nightly.yml"] = pushOnly // removed by a commit since
	e.s.Tick(ctx, at("2026-11-02 02:30:10"))
	if len(e.runs(t)) != 0 {
		t.Fatal("a schedule that was removed must not fire")
	}
}

func TestRepositoriesNotAllowedAreLeftAlone(t *testing.T) {
	e := newEnv(t, map[string]string{".github/workflows/nightly.yml": nightly})
	e.s.Allowed = func(string) bool { return false }
	ctx := context.Background()
	e.s.Tick(ctx, at("2026-11-02 02:29:00"))
	e.s.Tick(ctx, at("2026-11-02 02:30:10"))
	if len(e.runs(t)) != 0 || e.src.looks != 0 {
		t.Fatal("a repository that is not enabled must not be looked at")
	}
}

func TestExtraRepositoriesAreWatchedBeforeAnyEvent(t *testing.T) {
	e := newEnv(t, map[string]string{".github/workflows/nightly.yml": nightly})
	e.s.Extra = []string{"acme/api"}
	ctx := context.Background()
	e.s.Tick(ctx, at("2026-11-02 02:29:00"))
	e.s.Tick(ctx, at("2026-11-02 02:30:10"))
	repos := map[string]bool{}
	for _, r := range e.runs(t) {
		repos[r.Repo] = true
	}
	if len(repos) != 2 || !repos["acme/web"] || !repos["acme/api"] {
		t.Fatalf("repos = %v", repos)
	}
}

func TestAFailedLookKeepsWhatWasKnownAndIsRetried(t *testing.T) {
	e := newEnv(t, map[string]string{".github/workflows/nightly.yml": nightly})
	ctx := context.Background()
	e.s.Tick(ctx, at("2026-11-02 02:20:00"))
	e.s.Tick(ctx, at("2026-11-02 02:20:30")) // knows the schedules
	looks := e.src.looks
	// the next look (after five minutes) fails; the knowledge stays and the
	// fire at 02:30 still comes, because only the fire reads GitHub again
	e.src.fail = errors.New("github is down")
	e.s.Tick(ctx, at("2026-11-02 02:26:00"))
	e.src.fail = nil
	e.s.Tick(ctx, at("2026-11-02 02:30:10"))
	if len(e.runs(t)) != 1 {
		t.Fatalf("runs = %d", len(e.runs(t)))
	}
	if e.src.looks <= looks {
		t.Fatal("a failed look must be tried again")
	}
}

func TestOnlyTheWorkflowsOfTheDefaultBranchAreRead(t *testing.T) {
	e := newEnv(t, map[string]string{".github/workflows/nightly.yml": nightly})
	e.src.branch = "trunk" // the default branch is whatever GitHub says
	ctx := context.Background()
	e.s.Tick(ctx, at("2026-11-02 02:29:00"))
	e.s.Tick(ctx, at("2026-11-02 02:30:10"))
	runs := e.runs(t)
	if len(runs) != 1 || runs[0].Ref != "refs/heads/trunk" {
		t.Fatalf("runs = %+v", runs)
	}
}
