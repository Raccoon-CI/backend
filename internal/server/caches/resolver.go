package caches

import (
	"context"
	"sync"
	"time"

	"github.com/Raccoon-CI/backend/internal/server/store"
)

// Runs is what the resolver reads about a run (store.Store).
type Runs interface {
	GetRun(ctx context.Context, id int64) (store.Run, error)
	GetEvent(ctx context.Context, id int64) (store.Event, error)
}

// Branches says which branch is a repository's default (github.Client).
type Branches interface {
	DefaultBranch(ctx context.Context, installationID int64, repo string) (string, error)
}

const (
	runTTL     = 6 * time.Hour
	branchTTL  = 10 * time.Minute
	failureTTL = 30 * time.Second // an answer made without the default branch, kept briefly
)

// Resolver answers Config.Spaces from the stored run: its repository and ref,
// and the repository's default branch, which GitHub is asked once in a while.
type Resolver struct {
	Runs     Runs
	Branches Branches
	// Now is the clock; tests replace it.
	Now func() time.Time

	mu       sync.Mutex
	bySpaces map[int64]cachedSpace
	branches map[string]cachedBranch
}

type cachedSpace struct {
	space   Space
	expires time.Time
}

type cachedBranch struct {
	name    string
	expires time.Time
}

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Spaces is Config.Spaces.
func (r *Resolver) Spaces(ctx context.Context, runID int64) (Space, error) {
	now := r.now()
	r.mu.Lock()
	if c, ok := r.bySpaces[runID]; ok && now.Before(c.expires) {
		r.mu.Unlock()
		return c.space, nil
	}
	r.mu.Unlock()

	run, err := r.Runs.GetRun(ctx, runID)
	if err != nil {
		return Space{}, err
	}
	var installation int64
	if ev, err := r.Runs.GetEvent(ctx, run.EventID); err == nil {
		installation = ev.InstallationID
	}
	def, known := r.defaultBranch(ctx, installation, run.Repo)
	sp := SpaceFor(run.Repo, run.Ref, def)
	ttl := runTTL
	if !known {
		ttl = failureTTL
	}
	r.mu.Lock()
	if r.bySpaces == nil || len(r.bySpaces) > 4096 {
		r.bySpaces = map[int64]cachedSpace{}
	}
	r.bySpaces[runID] = cachedSpace{sp, now.Add(ttl)}
	r.mu.Unlock()
	return sp, nil
}

func (r *Resolver) defaultBranch(ctx context.Context, installation int64, repo string) (string, bool) {
	now := r.now()
	r.mu.Lock()
	if c, ok := r.branches[repo]; ok && now.Before(c.expires) {
		r.mu.Unlock()
		return c.name, true
	}
	r.mu.Unlock()
	name, err := r.Branches.DefaultBranch(ctx, installation, repo)
	if err != nil || name == "" {
		// Without it every run is taken for a non-default one: safe, with fewer hits.
		return "", false
	}
	r.mu.Lock()
	if r.branches == nil {
		r.branches = map[string]cachedBranch{}
	}
	r.branches[repo] = cachedBranch{name, now.Add(branchTTL)}
	r.mu.Unlock()
	return name, true
}
