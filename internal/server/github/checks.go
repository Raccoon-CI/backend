package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// Check run statuses and conclusions, as GitHub names them.
const (
	CheckQueued     = "queued"
	CheckInProgress = "in_progress"
	CheckCompleted  = "completed"

	CheckSuccess   = "success"
	CheckFailure   = "failure"
	CheckCancelled = "cancelled"
	CheckSkipped   = "skipped"
)

// CheckRun is what is written to a check run. Conclusion is set only with
// status "completed".
type CheckRun struct {
	Name        string
	Status      string
	Conclusion  string
	Title       string
	Summary     string
	StartedAt   time.Time
	CompletedAt time.Time
	DetailsURL  string
	// Annotations are shown on the check run and, with a path of the
	// repository, on the files of the commit. At most MaxAnnotations go in one
	// request; the rest are dropped.
	Annotations []Annotation
}

// MaxAnnotations is GitHub's limit per check run request.
const MaxAnnotations = 50

// Annotation levels, as GitHub names them.
const (
	LevelNotice  = "notice"
	LevelWarning = "warning"
	LevelFailure = "failure"
)

// Annotation is one message tied to a place in the commit.
type Annotation struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Level     string `json:"annotation_level"`
	Title     string `json:"title,omitempty"`
	Message   string `json:"message"`
}

// GitHub's limits for one annotation's text.
const (
	maxAnnotationTitle   = 255
	maxAnnotationMessage = 64 << 10
)

// maxCheckText is GitHub's limit for a check run's summary (65535 characters).
const maxCheckText = 65535

func (r CheckRun) body(withSHA string) map[string]any {
	b := map[string]any{"name": r.Name, "status": r.Status}
	if withSHA != "" {
		b["head_sha"] = withSHA
	}
	if r.Conclusion != "" {
		b["conclusion"] = r.Conclusion
	}
	if r.DetailsURL != "" {
		b["details_url"] = r.DetailsURL
	}
	if !r.StartedAt.IsZero() {
		b["started_at"] = r.StartedAt.UTC().Format(time.RFC3339)
	}
	if !r.CompletedAt.IsZero() {
		b["completed_at"] = r.CompletedAt.UTC().Format(time.RFC3339)
	}
	if r.Title != "" || r.Summary != "" {
		s := r.Summary
		if len(s) > maxCheckText {
			s = s[:maxCheckText-3] + "..."
		}
		title := r.Title
		if title == "" {
			title = r.Name
		}
		out := map[string]any{"title": title, "summary": s}
		if len(r.Annotations) > 0 {
			list := r.Annotations[:min(len(r.Annotations), MaxAnnotations)]
			clean := make([]Annotation, len(list))
			for i, a := range list {
				if len(a.Title) > maxAnnotationTitle {
					a.Title = a.Title[:maxAnnotationTitle]
				}
				if len(a.Message) > maxAnnotationMessage {
					a.Message = a.Message[:maxAnnotationMessage]
				}
				clean[i] = a
			}
			out["annotations"] = clean
		}
		b["output"] = out
	}
	return b
}

// CreateCheckRun makes a check run for the commit sha of repo and returns its
// id. Only a GitHub App installation may write check runs (a plain token gets
// 403/404), so the caller must not use it without one.
func (c *Client) CreateCheckRun(ctx context.Context, installationID int64, repo, sha string, r CheckRun) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := c.do(ctx, installationID, "POST", "/repos/"+repo+"/check-runs", r.body(sha), &out)
	if errors.Is(err, ErrNotFound) {
		return 0, errors.New("github: the check run could not be made: the repository was not found, or the App cannot write checks")
	}
	if err != nil {
		return 0, err
	}
	if out.ID == 0 {
		return 0, errors.New("github: the answer to a check run has no id")
	}
	return out.ID, nil
}

// UpdateCheckRun changes a check run made by CreateCheckRun.
func (c *Client) UpdateCheckRun(ctx context.Context, installationID int64, repo string, id int64, r CheckRun) error {
	p := fmt.Sprintf("/repos/%s/check-runs/%s", repo, url.PathEscape(strconv.FormatInt(id, 10)))
	err := c.do(ctx, installationID, "PATCH", p, r.body(""), nil)
	if errors.Is(err, ErrNotFound) {
		return errors.New("github: the check run was not found")
	}
	return err
}
