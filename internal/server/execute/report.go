package execute

import (
	"context"

	"raccoonci/internal/server/github"
	"raccoonci/internal/server/store"
)

// Statuser sets a commit status (github.Client).
type Statuser interface {
	SetStatus(ctx context.Context, installationID int64, repo, sha, state, statusContext, description, targetURL string) error
}

// Statuses reports a run as a commit status, "raccoonci / <workflow name>",
// on the commit it ran: it shows on the commit and, for a pull request, in its
// checks list. It works with a plain token as well as with a GitHub App. Check
// runs (a line per job, annotations, a summary) need the App and come later.
type Statuses struct {
	GH Statuser
}

// Report implements Reporter.
func (s Statuses) Report(ctx context.Context, installationID int64, run store.Run, state, description string) error {
	return s.GH.SetStatus(ctx, installationID, run.Repo, run.SHA, state, github.StatusContext(run.WorkflowName), description, "")
}
