// Package schedule fires the runs of `on: schedule:` workflows. GitHub does it
// for GitHub Actions; here the server does, because it runs the workflows.
//
// Like GitHub, only the workflow files on the repository's default branch count,
// and a run is for the newest commit of that branch at the time of the fire.
// The server looks for repositories among those it has had an event for (a
// delivery, a dispatch) and those named in Extra, and keeps what it found
// about each one's schedules for a few minutes. A fire is the cron expression's
// minute; it is recorded as an event with an id made from the repository, the
// file, the expression and the minute, so a restart, or two servers, cannot
// start one fire twice. A fire that passes while the server is down is not
// made up for (GitHub does not either; its runs may be late or dropped).
package schedule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"sync"
	"time"

	"github.com/Raccoon-CI/backend/internal/server/github"
	"github.com/Raccoon-CI/backend/internal/server/store"
	"github.com/Raccoon-CI/lib/pkg/cron"
	"github.com/Raccoon-CI/lib/pkg/trigger"
	"github.com/Raccoon-CI/lib/pkg/workflow"
)

// refresh is how long what is known about a repository's schedules is kept.
const refresh = 5 * time.Minute

// retry is how long after a failed look the next one comes.
const retry = time.Minute

// Source is what the scheduler needs from GitHub; internal/server/github.Client
// is the real one.
type Source interface {
	DefaultBranch(ctx context.Context, installationID int64, repo string) (string, error)
	ResolveRef(ctx context.Context, installationID int64, repo, ref string) (string, error)
	WorkflowFiles(ctx context.Context, installationID int64, repo, ref string) ([]github.File, error)
	FileAt(ctx context.Context, installationID int64, repo, filePath, ref string) ([]byte, error)
}

// Scheduler fires scheduled runs.
type Scheduler struct {
	Store  store.Store
	Source Source
	// Allowed says whether the server works for a repository.
	Allowed func(repo string) bool
	// Extra are repositories to watch before any event has named them
	// ("owner/name").
	Extra []string
	Log   *slog.Logger
	// Notify, if set, is called after runs have been queued.
	Notify func()

	mu    sync.Mutex
	last  time.Time
	repos map[string]*repoState
}

type repoState struct {
	loaded  time.Time
	branch  string
	entries []entry
}

type entry struct {
	path  string
	crons []fire
}

type fire struct {
	expr string
	c    *cron.Cron
}

func (s *Scheduler) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Run looks every 15 seconds until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	s.Tick(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.Tick(ctx, now)
		}
	}
}

// Tick fires what became due since the previous tick (the first tick only
// notes the time). Exported for tests.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last.IsZero() {
		s.last = now
		return
	}
	last := s.last
	s.last = now
	repos, err := s.knownRepos(ctx)
	if err != nil {
		s.log().Error("listing repositories for schedules failed", "err", err)
		return
	}
	started := 0
	for _, r := range repos {
		st := s.state(ctx, r, now)
		for _, e := range st.entries {
			for _, f := range e.crons {
				// Only the newest of several missed minutes: after a stall one
				// run is wanted, not a burst.
				var due time.Time
				for n := f.c.Next(last); !n.IsZero() && !n.After(now); n = f.c.Next(n) {
					due = n
				}
				if due.IsZero() {
					continue
				}
				ok, err := s.fire(ctx, r, st.branch, e.path, f.expr, due)
				if err != nil {
					s.log().Error("firing a scheduled run failed", "repo", r.Name, "workflow", e.path, "cron", f.expr, "err", err)
					continue
				}
				if ok {
					started++
				}
			}
		}
	}
	if started > 0 && s.Notify != nil {
		s.Notify()
	}
}

func (s *Scheduler) knownRepos(ctx context.Context) ([]store.RepoRef, error) {
	known, err := s.Store.Repos(ctx)
	if err != nil {
		return nil, err
	}
	for _, name := range s.Extra {
		if !slices.ContainsFunc(known, func(r store.RepoRef) bool { return r.Name == name }) {
			known = append(known, store.RepoRef{Name: name})
		}
	}
	return slices.DeleteFunc(known, func(r store.RepoRef) bool { return s.Allowed != nil && !s.Allowed(r.Name) }), nil
}

// state returns what is known about a repository's schedules, looking again
// when it is old. A failed look keeps the old knowledge.
func (s *Scheduler) state(ctx context.Context, r store.RepoRef, now time.Time) *repoState {
	if s.repos == nil {
		s.repos = map[string]*repoState{}
	}
	st := s.repos[r.Name]
	if st != nil && now.Sub(st.loaded) < refresh {
		return st
	}
	fresh, err := s.load(ctx, r)
	switch {
	case err != nil && st != nil:
		s.log().Warn("looking at a repository's schedules failed; keeping what was known", "repo", r.Name, "err", err)
		st.loaded = now.Add(retry - refresh)
		return st
	case err != nil:
		s.log().Warn("looking at a repository's schedules failed", "repo", r.Name, "err", err)
		fresh = &repoState{}
		fresh.loaded = now.Add(retry - refresh)
	default:
		fresh.loaded = now
	}
	s.repos[r.Name] = fresh
	return fresh
}

func (s *Scheduler) load(ctx context.Context, r store.RepoRef) (*repoState, error) {
	branch, err := s.Source.DefaultBranch(ctx, r.InstallationID, r.Name)
	if err != nil {
		return nil, fmt.Errorf("default branch: %w", err)
	}
	sha, err := s.Source.ResolveRef(ctx, r.InstallationID, r.Name, "refs/heads/"+branch)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", branch, err)
	}
	files, err := s.Source.WorkflowFiles(ctx, r.InstallationID, r.Name, sha)
	if err != nil {
		return nil, fmt.Errorf("workflows at %s: %w", sha, err)
	}
	st := &repoState{branch: branch}
	for _, f := range files {
		w, err := workflow.Parse(f.Content)
		if err != nil {
			continue
		}
		exprs, err := trigger.Schedules(w.On)
		if err != nil || len(exprs) == 0 {
			continue
		}
		e := entry{path: f.Path}
		for _, x := range exprs {
			c, err := cron.Parse(x)
			if err != nil {
				s.log().Warn("a schedule cannot be read", "repo", r.Name, "workflow", f.Path, "err", err)
				continue
			}
			e.crons = append(e.crons, fire{expr: x, c: c})
		}
		if len(e.crons) > 0 {
			st.entries = append(st.entries, e)
		}
	}
	return st, nil
}

// fire starts the run for one cron entry of one workflow at minute due. The
// workflow is read again at the branch's newest commit, because that is what
// the run is for and what decides whether the entry still exists.
func (s *Scheduler) fire(ctx context.Context, r store.RepoRef, branch, file, expr string, due time.Time) (bool, error) {
	ref := "refs/heads/" + branch
	sha, err := s.Source.ResolveRef(ctx, r.InstallationID, r.Name, ref)
	if err != nil {
		return false, fmt.Errorf("resolving %s: %w", ref, err)
	}
	text, err := s.Source.FileAt(ctx, r.InstallationID, r.Name, file, sha)
	if err != nil {
		return false, fmt.Errorf("reading %s at %s: %w", file, sha, err)
	}
	w, err := workflow.Parse(text)
	if err != nil {
		return false, nil // changed since it was looked at; the next look drops it
	}
	res, err := trigger.Match(w.On, trigger.Event{Name: "schedule", Cron: expr})
	if err != nil || !res.Triggered {
		return false, nil
	}
	sum := sha256.Sum256([]byte(expr))
	payload, _ := json.Marshal(map[string]any{"schedule": expr, "repository": map[string]any{"full_name": r.Name}})
	ev, _, err := s.Store.RecordEvent(ctx, store.Event{
		DeliveryID: fmt.Sprintf("schedule-%s-%s-%s-%d", r.Name, file, hex.EncodeToString(sum[:4]), due.Unix()),
		Kind:       "schedule", Repo: r.Name, InstallationID: r.InstallationID, Payload: payload,
		Status: store.EventHandled, Note: fmt.Sprintf("%s fired at %s", expr, due.UTC().Format(time.RFC3339)),
	})
	if err != nil {
		return false, err
	}
	run, created, err := s.Store.CreateRun(ctx, store.Run{
		EventID: ev.ID, Repo: r.Name, WorkflowPath: file, WorkflowName: nameOf(w, file),
		Event: "schedule", Ref: ref, SHA: sha, Actor: "schedule", WorkflowText: string(text),
	})
	if err != nil {
		return false, err
	}
	if created {
		s.log().Info("scheduled run created", "run", run.ID, "repo", r.Name, "workflow", file, "cron", expr)
	}
	return created, nil
}

func nameOf(w *workflow.Workflow, filePath string) string {
	if w.Name != "" {
		return w.Name
	}
	return path.Base(filePath)
}
