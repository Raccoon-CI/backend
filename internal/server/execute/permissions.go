package execute

import (
	"context"

	"github.com/Raccoon-CI/lib/pkg/value"
	"github.com/Raccoon-CI/lib/pkg/workflow"
)

// TokenMinter makes the token a job gets as GITHUB_TOKEN (github.App): one for
// a repository, limited to the permissions asked for. Its answer is never logged.
type TokenMinter interface {
	RunToken(ctx context.Context, installationID int64, repo string, permissions map[string]string) (string, error)
	// RevokeToken ends a token made by RunToken before it would expire.
	RevokeToken(ctx context.Context, token string) error
}

// IDTokens makes the environment of a job that may ask for ID tokens.
type IDTokens interface {
	Env(run int64, job string) map[string]string
}

// wantsIDToken says whether a job has `id-token: write`: its own
// `permissions:`, else the workflow's, as for the GITHUB_TOKEN. `write-all`
// includes it; without any `permissions:` a job has no right to ID tokens.
func wantsIDToken(w *workflow.Workflow, j *workflow.Job) bool {
	var spec any
	switch {
	case j != nil && j.Permissions != nil:
		spec = j.Permissions
	case w != nil && w.Permissions != nil:
		spec = w.Permissions
	}
	switch s := spec.(type) {
	case string:
		return s == "write-all"
	case value.Object:
		v, _ := s.Get("id-token")
		return v == "write"
	}
	return false
}

// defaultPermissions is what a job gets when neither it nor its workflow says
// otherwise. GitHub's own default may be read and write, depending on the
// repository's settings; the server's is read-only, which is the safe end of
// that range (a workflow that needs more says so with `permissions:`).
var defaultPermissions = map[string]string{"contents": "read", "metadata": "read"}

// apiPermissions maps the names of a workflow's `permissions:` to the names of
// the installation token API. A name that is not here (id-token, attestations,
// models) is not something an App token can have and is dropped.
var apiPermissions = map[string]string{
	"actions": "actions", "checks": "checks", "contents": "contents", "deployments": "deployments",
	"discussions": "discussions", "issues": "issues", "packages": "packages", "pages": "pages",
	"pull-requests": "pull_requests", "statuses": "statuses", "security-events": "security_events",
}

// tokenPermissions is the permission set of a job's token: the job's own
// `permissions:`, else the workflow's, else the default. read-all and
// write-all cover every name above; a mapping names what it wants and
// anything unnamed is none, as on GitHub. metadata: read is always there.
func tokenPermissions(w *workflow.Workflow, j *workflow.Job) map[string]string {
	var spec any
	switch {
	case j != nil && j.Permissions != nil:
		spec = j.Permissions
	case w != nil && w.Permissions != nil:
		spec = w.Permissions
	default:
		out := map[string]string{}
		for k, v := range defaultPermissions {
			out[k] = v
		}
		return out
	}
	out := map[string]string{"metadata": "read"}
	switch s := spec.(type) {
	case string:
		level := ""
		switch s {
		case "read-all":
			level = "read"
		case "write-all":
			level = "write"
		}
		if level != "" {
			for _, api := range apiPermissions {
				out[api] = level
			}
		}
	case value.Object:
		for _, f := range s {
			api, ok := apiPermissions[f.Key]
			level, _ := f.Value.(string)
			if ok && (level == "read" || level == "write") {
				out[api] = level
			}
		}
	}
	return out
}
