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

// RepoRef is a repository the server has seen an event for.
type RepoRef struct {
	Name           string // owner/name
	InstallationID int64
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
	// Attempts is how many times the executor has taken the run: 1 for a run
	// that ran once, more when a stopped server's run was queued again.
	Attempts int
	// LogPurged is set when the run's log lines were deleted after the
	// retention time.
	LogPurged bool
	CreatedAt time.Time
	UpdatedAt time.Time
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

// Document is a named blob the server keeps for itself (PutDocument).
type Document struct {
	Name      string
	Data      []byte
	UpdatedAt time.Time
}

// LogLine is one entry of a run's log: a runner.Event as JSON.
type LogLine struct {
	ID   int64 // set by the store; grows with the order of arrival
	At   time.Time
	Data string
}

// Secret kinds.
const (
	KindSecret = "secret"
	KindVar    = "var"
	// KindStorage is what the server keeps for itself under an organization's
	// scope (its bucket's settings, sealed); a workflow can never name it.
	KindStorage = "storage"
)

// Secret is a secret or a variable of a scope ("org:acme", "repo:acme/web").
// Value is sealed for a secret (internal/server/vault) and plain for a variable.
type Secret struct {
	Scope     string
	Name      string
	Kind      string
	KeyID     string
	Value     []byte
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Artifact is a finished artifact of a run. Scope names the artifact service
// that holds its data ("disk", or "<org>@<bucket version>"); IDs count per scope.
type Artifact struct {
	Scope     string
	ID        int64
	Run       string // the run's id, as the artifact protocol carries it
	Job       string
	Name      string
	Key       string // where the data is in the scope's store
	Mime      string
	Size      int64
	Digest    string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Token roles.
const (
	RoleRead  = "read"
	RoleAdmin = "admin"
)

// APIToken describes a token for the API; the token itself is never stored.
type APIToken struct {
	ID         int64
	Name       string
	Role       string
	CreatedAt  time.Time
	LastUsedAt time.Time // zero until used
	RevokedAt  time.Time // zero while it works
	// Repos limits a token of the read role to these repositories (patterns
	// owner/name or owner/*, lower case); empty means all of them.
	Repos []string
	// User and ExpiresAt are set for a session, a token made by signing in with
	// GitHub: the login it belongs to and when it stops working.
	User      string
	ExpiresAt time.Time
}

// RunFilter selects runs, newest first.
type RunFilter struct {
	Repo string // empty: any
	// Allow, if set, keeps only the runs of repositories these patterns cover
	// (owner/name or owner/*, lower case: NormalizeRepoPatterns).
	Allow  []string
	Before int64 // only runs with a smaller id, for paging; 0: from the newest
	Limit  int   // default 50
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
	// Repos lists the repositories any stored event is about, by name, each
	// with the newest installation id seen for it (0 if none): the repositories
	// the server has been told about, which is where it looks for schedules.
	Repos(ctx context.Context) ([]RepoRef, error)

	// CreateRun stores a run. created is false when that event already has a
	// run for the workflow file: handling an event again does not start a
	// second run.
	CreateRun(ctx context.Context, r Run) (stored Run, created bool, err error)
	GetRun(ctx context.Context, id int64) (Run, error)
	// RunNumbers gives a run its github.run_number and github.run_attempt as
	// GitHub counts them: the number counts the workflow's runs started by an
	// event (not reruns) up to and including the run, or the one it repeats;
	// the attempt is 1 for that run and one more for each rerun of it.
	RunNumbers(ctx context.Context, run Run) (number, attempt int, err error)
	ListRuns(ctx context.Context, f RunFilter) ([]Run, error)

	// ClaimRun takes the next queued run and marks it in progress, so that two
	// callers never get the same one. The next one is fair between repositories:
	// of the queued runs, the one whose repository has the fewest runs in
	// progress, the oldest among equals. A repository with a long queue cannot
	// starve the others, and one alone still gets everything. ok is false when
	// none is queued.
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
	// Requeue puts a run in progress back in the queue (and forgets its jobs;
	// its log stays) if it has been taken fewer than maxAttempts times. ok is
	// false when it was not in progress or has used its attempts, and the caller
	// ends it.
	Requeue(ctx context.Context, id int64, maxAttempts int) (ok bool, err error)
	// RequeueInterrupted is Requeue for every run a stopped server left in
	// progress; those that have used their attempts are ended with conclusion
	// (as InterruptRuns does). It returns how many of each.
	RequeueInterrupted(ctx context.Context, maxAttempts int, conclusion string) (requeued, ended int, err error)

	// StartJob records that a job instance of a run began (again, if the name
	// is already there).
	StartJob(ctx context.Context, runID int64, name string) (Job, error)
	FinishJob(ctx context.Context, id int64, conclusion string) error
	ListJobs(ctx context.Context, runID int64) ([]Job, error)

	// AppendLog adds lines to a run's log, in order.
	AppendLog(ctx context.Context, runID int64, lines []LogLine) error
	// PurgeLogs deletes the log lines of up to limit finished runs that ended
	// before the given time and marks them (Run.LogPurged). It returns how many
	// runs and lines; call it again until it returns no runs.
	PurgeLogs(ctx context.Context, before time.Time, limit int) (runs int, lines int64, err error)
	// ReadLog returns up to limit lines of a run's log with an id above after.
	ReadLog(ctx context.Context, runID, after int64, limit int) ([]LogLine, error)

	// PutSecret makes or replaces a secret or variable (the creation time of
	// an existing one stays).
	PutSecret(ctx context.Context, s Secret) error
	GetSecret(ctx context.Context, scope, name string) (Secret, error)
	// ListSecrets lists the secrets and variables of a scope, by name.
	ListSecrets(ctx context.Context, scope string) ([]Secret, error)
	// DeleteSecret reports whether there was one to delete.
	DeleteSecret(ctx context.Context, scope, name string) (bool, error)

	// PutArtifact stores a finished artifact (replacing one with the same scope and id).
	PutArtifact(ctx context.Context, a Artifact) error
	// ListArtifacts returns a scope's artifacts, by id.
	ListArtifacts(ctx context.Context, scope string) ([]Artifact, error)
	// DeleteArtifact forgets one; it is not an error if it is not there.
	DeleteArtifact(ctx context.Context, scope string, id int64) error
	// ExpiredArtifacts returns the artifacts of every scope whose time to keep
	// has passed, the oldest expiry first.
	ExpiredArtifacts(ctx context.Context, now time.Time) ([]Artifact, error)

	// AddUsage adds machine seconds (and finished runs) to a repository's row for
	// the UTC day `at` falls on.
	AddUsage(ctx context.Context, repo string, at time.Time, machineSeconds int64, runs int) error
	// UsedSince sums the machine seconds of the days from the one `from` falls
	// on, for a repository (owner/name) or all of an owner's (owner/*).
	UsedSince(ctx context.Context, from time.Time, scope string) (int64, error)
	// ListUsage returns the usage rows of the days from..to, both included.
	ListUsage(ctx context.Context, from, to time.Time) ([]Usage, error)

	// PutInstallation makes or updates an installation of the GitHub App; with
	// replaceRepos its repository list becomes repos, else repos are added.
	PutInstallation(ctx context.Context, in Installation, repos []string, replaceRepos bool) error
	// ChangeInstallationRepos applies an installation_repositories event
	// (selection "" keeps the selection); ErrNotFound for an unknown installation.
	ChangeInstallationRepos(ctx context.Context, id int64, selection string, add, remove []string) error
	SetInstallationSuspended(ctx context.Context, id int64, suspended bool) error
	DeleteInstallation(ctx context.Context, id int64) error
	ListInstallations(ctx context.Context) ([]Installation, error)
	// InstallationOfRepo is the working installation that covers a repository
	// as far as the App's events said; ErrNotFound when none is known.
	InstallationOfRepo(ctx context.Context, repo string) (int64, error)

	// PutDocument makes or replaces a named document the server keeps for itself.
	PutDocument(ctx context.Context, name string, data []byte) error
	// GetDocument returns one; ErrNotFound when there is none.
	GetDocument(ctx context.Context, name string) (Document, error)

	// CreateToken records a token by the SHA-256 (hex) of its value.
	CreateToken(ctx context.Context, name, role, hash string, repos []string) (APIToken, error)
	// CreateSession makes a token for a user who signed in: it expires, and
	// repos limits it (nil: all, for an administrator).
	CreateSession(ctx context.Context, name, user, role, hash string, repos []string, expires time.Time) (APIToken, error)
	// PurgeExpiredTokens deletes the tokens that expired before the time given
	// and returns how many.
	PurgeExpiredTokens(ctx context.Context, before time.Time) (int, error)
	// LookupToken finds a token that has not been revoked or expired and notes its use.
	LookupToken(ctx context.Context, hash string) (APIToken, error)
	ListTokens(ctx context.Context) ([]APIToken, error)
	// RevokeToken reports whether there was a working token with that id.
	RevokeToken(ctx context.Context, id int64) (bool, error)

	// Ping checks that the database can be written (it takes its write lock and
	// lets it go) without writing anything.
	Ping(ctx context.Context) error

	Close() error
}
