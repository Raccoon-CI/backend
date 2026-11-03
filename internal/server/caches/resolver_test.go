package caches

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Raccoon-CI/backend/internal/server/store"
)

type fakeRuns struct {
	runs  map[int64]store.Run
	calls int
}

func (f *fakeRuns) GetRun(_ context.Context, id int64) (store.Run, error) {
	f.calls++
	if r, ok := f.runs[id]; ok {
		return r, nil
	}
	return store.Run{}, store.ErrNotFound
}

func (f *fakeRuns) GetEvent(context.Context, int64) (store.Event, error) {
	return store.Event{InstallationID: 42}, nil
}

type fakeBranches struct {
	name  string
	err   error
	calls int
	inst  int64
}

func (f *fakeBranches) DefaultBranch(_ context.Context, inst int64, _ string) (string, error) {
	f.calls++
	f.inst = inst
	return f.name, f.err
}

func TestResolverMapsARunToItsSpaces(t *testing.T) {
	runs := &fakeRuns{runs: map[int64]store.Run{
		1: {Repo: "acme/web", Ref: "refs/heads/main"},
		2: {Repo: "acme/web", Ref: "refs/heads/feature"},
	}}
	br := &fakeBranches{name: "main"}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	r := &Resolver{Runs: runs, Branches: br, Now: func() time.Time { return now }}
	ctx := context.Background()

	got1, err := r.Spaces(ctx, 1)
	if err != nil || got1.Own != SpaceFor("acme/web", "refs/heads/main", "main").Own {
		t.Fatalf("run 1 = %+v, %v", got1, err)
	}
	got2, _ := r.Spaces(ctx, 2)
	if got2.Own == got1.Own || got2.Read[1] != got1.Own {
		t.Fatalf("run 2 = %+v", got2)
	}
	if br.calls != 1 || br.inst != 42 {
		t.Fatalf("GitHub was asked %d times (installation %d): one answer serves the repository", br.calls, br.inst)
	}
	r.Spaces(ctx, 1)
	r.Spaces(ctx, 2)
	if runs.calls != 2 {
		t.Fatalf("the store was read %d times, want 2: a run's answer is kept", runs.calls)
	}
	if _, err := r.Spaces(ctx, 99); err == nil {
		t.Fatal("an unknown run got spaces")
	}
	now = now.Add(11 * time.Minute) // the branch answer is old
	br.name = "trunk"
	r.bySpaces = nil
	if got, _ := r.Spaces(ctx, 1); got.Own == SpaceFor("acme/web", "refs/heads/main", "main").Own || br.calls != 2 {
		t.Fatalf("after the default branch changed: %+v after %d asks", got, br.calls)
	}
}

func TestWithoutTheDefaultBranchNoRunIsTakenForTheDefaultAndItIsAskedAgainSoon(t *testing.T) {
	runs := &fakeRuns{runs: map[int64]store.Run{1: {Repo: "acme/web", Ref: "refs/heads/main"}}}
	br := &fakeBranches{err: errors.New("github is down")}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	r := &Resolver{Runs: runs, Branches: br, Now: func() time.Time { return now }}
	got, err := r.Spaces(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Own == SpaceFor("acme/web", "refs/heads/main", "main").Own {
		t.Fatal("a run was given the default branch's space without knowing the default branch")
	}
	br.err, br.name = nil, "main"
	now = now.Add(time.Minute) // the short answer has expired
	if got, _ := r.Spaces(context.Background(), 1); got.Own != SpaceFor("acme/web", "refs/heads/main", "main").Own {
		t.Fatalf("after GitHub came back: %+v", got)
	}
}
