package execute

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Quota is an allowance of machine time per UTC month. Zero means none. Machine
// time is what a job instance held a machine for, start-up included (see the
// usage table). The count is of runs that have finished, so runs that are in
// progress when the allowance runs out still finish: it can be overshot by
// what runs at the same time.
type Quota struct {
	// RepoMinutes is the allowance of each repository, OwnerMinutes of each
	// owner (user or organization) over all its repositories.
	RepoMinutes, OwnerMinutes int64
}

func (e *Executor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// monthStart is the first instant of the UTC month t is in.
func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// overQuota says why a repository may not start a run now, or "" when it may.
// A failure to read the usage lets the run go on: a database hiccup must not
// stop everybody's builds.
func (e *Executor) overQuota(ctx context.Context, repo string) string {
	q := e.Quota
	if q.RepoMinutes <= 0 && q.OwnerMinutes <= 0 {
		return ""
	}
	now := e.now()
	from := monthStart(now)
	resets := from.AddDate(0, 1, 0).Format("2006-01-02")
	repo = strings.ToLower(repo)
	if q.RepoMinutes > 0 {
		if used, err := e.Store.UsedSince(ctx, from, repo); err == nil && used >= q.RepoMinutes*60 {
			return fmt.Sprintf("%s has used its machine-time allowance for this month (%d of %d minutes); runs start again on %s UTC", repo, used/60, q.RepoMinutes, resets)
		}
	}
	if q.OwnerMinutes > 0 {
		owner, _, _ := strings.Cut(repo, "/")
		if used, err := e.Store.UsedSince(ctx, from, owner+"/*"); err == nil && used >= q.OwnerMinutes*60 {
			return fmt.Sprintf("%s has used its machine-time allowance for this month (%d of %d minutes over all its repositories); runs start again on %s UTC", owner, used/60, q.OwnerMinutes, resets)
		}
	}
	return ""
}
