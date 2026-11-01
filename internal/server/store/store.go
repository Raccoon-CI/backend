// Package store is the server's database: the webhook deliveries it has
// received and the runs they started. The rest of the server talks to the
// Store interface only, so the first implementation (SQLite) can be joined by
// another (PostgreSQL) without touching callers.
package store

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Event statuses. A delivery is stored as received, before anything is done
// with it, so one that is interrupted (the server stops) is found again at
// the next start.
const (
	EventReceived = "received"
	EventHandled  = "handled"
	EventIgnored  = "ignored"
	EventFailed   = "failed"
)

// Run statuses, as GitHub names them.
const (
	RunQueued     = "queued"
	RunInProgress = "in_progress"
	RunCompleted  = "completed"
)

// Event is one webhook delivery.
type Event struct {
	ID int64
	// DeliveryID is GitHub's X-GitHub-Delivery: the same for a redelivery, so
	// it is what makes handling a delivery idempotent.
	DeliveryID     string
	Kind           string // push, pull_request, ...
	Action         string
	Repo           string // owner/name
	InstallationID int64  // 0 when the delivery has none (a repository webhook)
	ReceivedAt     time.Time
	Payload        []byte // the body as received, so a delivery can be replayed
	Status         string
	Note           string // what was decided, per workflow
}

// Run is one workflow run the server decided to start.
type Run struct {
	ID           int64
	EventID      int64
	Repo         string
	WorkflowPath string // .github/workflows/ci.yml
	WorkflowName string
	Event        string // the event name that started it: push, pull_request, ...
	Ref          string // refs/heads/main, refs/tags/v1, refs/pull/7/merge
	SHA          string // the commit to run (a pull request: its head)
	BaseSHA      string // a pull request's base commit
	PullRequest  int    // 0 when not a pull request
	// Fork is set for a pull request from another repository: it gets no
	// secrets and cannot write caches that trusted refs read.
	Fork  bool
	Actor string
	// Inputs are a workflow_dispatch run's inputs, a JSON object ("{}" when none).
	Inputs string
	// RerunOf is the run this one repeats (0 when it started from an event).
	RerunOf    int64
	Status     string
	Conclusion string
	// WorkflowText is the workflow file at SHA, kept so that a rerun uses the
	// same text. Lists leave it out.
	WorkflowText string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Job is one job instance of a run.
type Job struct {
	ID          int64
	RunID       int64
	Name        string
	Status      string
	Conclusion  string
	StartedAt   time.Time // zero until it starts
	CompletedAt time.Time // zero until it ends
}

// LogLine is one entry of a run's log: a runner.Event as JSON.
type LogLine struct {
	ID   int64 // set by the store; grows with the order of arrival
	At   time.Time
	Data string
}

// RunFilter selects runs, newest first.
type RunFilter struct {
	Repo   string // empty: any
	Before int64  // only runs with a smaller id, for paging; 0: from the newest
	Limit  int    // default 50
}

// Store is what the server keeps.
type Store interface {
	// RecordEvent stores a delivery. created is false when its DeliveryID is
	// already stored; the stored event comes back then.
	RecordEvent(ctx context.Context, e Event) (stored Event, created bool, err error)
	// PendingEvents returns deliveries still in status received, oldest first.
	PendingEvents(ctx context.Context) ([]Event, error)
	FinishEvent(ctx context.Context, id int64, status, note string) error
	GetEvent(ctx context.Context, id int64) (Event, error)

	// CreateRun stores a run. created is false when that event already has a
	// run for the workflow file: handling an event again does not start a
	// second run.
	CreateRun(ctx context.Context, r Run) (stored Run, created bool, err error)
	GetRun(ctx context.Context, id int64) (Run, error)
	ListRuns(ctx context.Context, f RunFilter) ([]Run, error)

	// ClaimRun takes the oldest queued run and marks it in progress, so that
	// two callers never get the same one. ok is false when none is queued.
	ClaimRun(ctx context.Context) (run Run, ok bool, err error)
	// FinishRun ends a run in progress with a conclusion (success, failure,
	// cancelled).
	FinishRun(ctx context.Context, id int64, conclusion string) error
	// CancelQueued ends a run that has not started, as cancelled. ok is false
	// when it is not queued (it started, or ended, or does not exist).
	CancelQueued(ctx context.Context, id int64) (ok bool, err error)
	// InterruptRuns ends the runs (and their jobs) still in progress, which a
	// stopped server left, with the conclusion given. It returns how many.
	InterruptRuns(ctx context.Context, conclusion string) (int, error)

	// StartJob records that a job instance of a run began (again, if the name
	// is already there).
	StartJob(ctx context.Context, runID int64, name string) (Job, error)
	FinishJob(ctx context.Context, id int64, conclusion string) error
	ListJobs(ctx context.Context, runID int64) ([]Job, error)

	// AppendLog adds lines to a run's log, in order.
	AppendLog(ctx context.Context, runID int64, lines []LogLine) error
	// ReadLog returns up to limit lines of a run's log with an id above after.
	ReadLog(ctx context.Context, runID, after int64, limit int) ([]LogLine, error)

	Close() error
}
