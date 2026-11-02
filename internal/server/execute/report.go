package execute

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/runevent"
	"github.com/Raccoon-CI/raccoonci/internal/server/github"
	"github.com/Raccoon-CI/raccoonci/internal/server/store"
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

// CheckRunner writes check runs (github.Client). Only a GitHub App
// installation may, so the executor uses it only for events that carry one.
type CheckRunner interface {
	CreateCheckRun(ctx context.Context, installationID int64, repo, sha string, r github.CheckRun) (int64, error)
	UpdateCheckRun(ctx context.Context, installationID int64, repo string, id int64, r github.CheckRun) error
}

// checkName is the name of the check run of one job instance.
func checkName(run store.Run, job string) string {
	return github.StatusContext(run.WorkflowName) + " / " + job
}

// checkConclusion maps a job's conclusion to GitHub's.
func checkConclusion(c runevent.Conclusion) string {
	switch c {
	case runevent.Success:
		return github.CheckSuccess
	case runevent.Cancelled:
		return github.CheckCancelled
	case runevent.Skipped:
		return github.CheckSkipped
	}
	return github.CheckFailure
}

// jobCheck is the check run of one job instance: started in the background
// when the job begins (a slow GitHub must not delay the job) and completed
// when it ends. Failures are logged, never fatal.
type jobCheck struct {
	x       *execution
	name    string
	started time.Time
	id      chan int64 // receives the id once, 0 when making it failed
}

func (x *execution) startCheck(job string) *jobCheck {
	if x.e.Checks == nil || x.installation == 0 {
		return nil
	}
	c := &jobCheck{x: x, name: checkName(x.run, job), started: time.Now(), id: make(chan int64, 1)}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		id, err := x.e.Checks.CreateCheckRun(ctx, x.installation, x.run.Repo, x.run.SHA, github.CheckRun{
			Name: c.name, Status: github.CheckInProgress, StartedAt: c.started, Title: "Running", Summary: "The job is running."})
		if err != nil {
			x.log.Warn("making a check run failed", "check", c.name, "err", err)
			id = 0
		}
		c.id <- id
	}()
	return c
}

// finish completes the check run with the job's conclusion, its summary
// ($GITHUB_STEP_SUMMARY, markdown; the plain title line when there is none)
// and the annotations its steps reported.
func (c *jobCheck) finish(conclusion runevent.Conclusion, jobSummary string, notes *annotations) {
	if c == nil {
		return
	}
	id := <-c.id
	if id == 0 {
		return
	}
	ended := time.Now()
	took := ended.Sub(c.started).Round(time.Second)
	title := fmt.Sprintf("%s in %v", strings.ToUpper(string(conclusion)[:1])+string(conclusion)[1:], took)
	summary := fmt.Sprintf("%s: %s.", c.name, title)
	if strings.TrimSpace(jobSummary) != "" {
		summary = jobSummary
	}
	var list []github.Annotation
	if notes != nil {
		var dropped int
		list, dropped = notes.get()
		if dropped > 0 {
			summary += fmt.Sprintf("\n\n%d more annotations were left out (GitHub takes %d per check run); the run's log has all of them.", dropped, github.MaxAnnotations)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := c.x.e.Checks.UpdateCheckRun(ctx, c.x.installation, c.x.run.Repo, id, github.CheckRun{
		Name: c.name, Status: github.CheckCompleted, Conclusion: checkConclusion(conclusion),
		Title: title, Summary: summary, Annotations: list, CompletedAt: ended})
	if err != nil {
		c.x.log.Warn("completing a check run failed", "check", c.name, "err", err)
	}
}

// Report implements Reporter.
func (s Statuses) Report(ctx context.Context, installationID int64, run store.Run, state, description string) error {
	return s.GH.SetStatus(ctx, installationID, run.Repo, run.SHA, state, github.StatusContext(run.WorkflowName), description, "")
}
