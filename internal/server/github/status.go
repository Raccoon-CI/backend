package github

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

// Commit status states, as GitHub names them.
const (
	StatePending = "pending"
	StateSuccess = "success"
	StateFailure = "failure"
	StateError   = "error"
)

// SetStatus sets the commit status of sha in repo for a context (the name of
// the check as shown on the commit and the pull request). A later call with
// the same context replaces the earlier one. Statuses, unlike check runs, can
// be written with a plain token as well as by a GitHub App, so they work while
// developing; the App also needs the "Commit statuses" permission.
func (c *Client) SetStatus(ctx context.Context, installationID int64, repo, sha, state, statusContext, description, targetURL string) error {
	if len(description) > 140 { // GitHub's limit
		description = description[:137] + "..."
	}
	body := map[string]string{"state": state, "context": statusContext, "description": description}
	if targetURL != "" {
		body["target_url"] = targetURL
	}
	err := c.do(ctx, installationID, "POST", "/repos/"+repo+"/statuses/"+url.PathEscape(sha), body, nil)
	if errors.Is(err, ErrNotFound) {
		return errors.New("github: the status could not be set: the commit or repository was not found, or the token cannot write statuses")
	}
	return err
}

// StatusContext is the context a workflow's run reports under.
func StatusContext(workflowName string) string {
	name := strings.TrimSpace(workflowName)
	if name == "" {
		name = "workflow"
	}
	return "raccoonci / " + name
}
