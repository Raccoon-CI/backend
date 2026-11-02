package intake

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"raccoonci/internal/server/github"
	"raccoonci/internal/server/store"
	"raccoonci/internal/trigger"
	"raccoonci/internal/workflow"
)

// RequestError is a refusal the caller can act on: Status is the HTTP status
// to answer with.
type RequestError struct {
	Status int
	Msg    string
}

func (e *RequestError) Error() string { return e.Msg }

func refuse(status int, format string, args ...any) error {
	return &RequestError{Status: status, Msg: fmt.Sprintf(format, args...)}
}

// DispatchRequest asks for a run of one workflow by hand, as the "Run
// workflow" button of GitHub does.
type DispatchRequest struct {
	Repo string `json:"repo"` // owner/name
	// Ref is a branch name, or a full ref (refs/heads/x, refs/tags/x).
	Ref string `json:"ref"`
	// Workflow is the file's path in the repository.
	Workflow string            `json:"workflow"`
	Inputs   map[string]string `json:"inputs"`
	// Actor is who asked, for the run's actor.
	Actor string `json:"-"`
}

var (
	repoName = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
	// refChars are what a branch or tag name may use here: GitHub allows more,
	// but nothing that changes the meaning of a URL path.
	refChars = regexp.MustCompile(`^[A-Za-z0-9._/@+-]+$`)
)

// installationFor says which installation of the GitHub App serves a
// repository: the one a stored delivery named, else Installation's answer.
// 0 (no installation) is right for a token, which needs none.
func (h *Handler) installationFor(ctx context.Context, repo string) (int64, error) {
	// What the App's own installation events said comes first.
	if id, err := h.Store.InstallationOfRepo(ctx, repo); err == nil {
		return id, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	known, err := h.Store.Repos(ctx)
	if err != nil {
		return 0, err
	}
	for _, r := range known {
		if r.Name == repo && r.InstallationID != 0 {
			return r.InstallationID, nil
		}
	}
	if h.Installation != nil {
		return h.Installation(ctx, repo)
	}
	return 0, nil
}

// Dispatch creates a run of a workflow that has a workflow_dispatch trigger,
// for the commit a ref names now, and returns it.
func (h *Handler) Dispatch(ctx context.Context, req DispatchRequest) (store.Run, error) {
	if !repoName.MatchString(req.Repo) {
		return store.Run{}, refuse(http.StatusBadRequest, "repo must be owner/name")
	}
	if !h.allowed(req.Repo) {
		return store.Run{}, refuse(http.StatusForbidden, "the repository is not enabled")
	}
	if req.Ref == "" || !refChars.MatchString(req.Ref) || strings.Contains(req.Ref, "..") {
		return store.Run{}, refuse(http.StatusBadRequest, "ref must be a branch name or a full ref")
	}
	if !strings.HasPrefix(req.Workflow, ".github/workflows/") || strings.Contains(req.Workflow, "..") ||
		!(strings.HasSuffix(req.Workflow, ".yml") || strings.HasSuffix(req.Workflow, ".yaml")) {
		return store.Run{}, refuse(http.StatusBadRequest, "workflow must be a path like .github/workflows/ci.yml")
	}
	fullRef := req.Ref
	if !strings.HasPrefix(fullRef, "refs/") {
		fullRef = "refs/heads/" + fullRef
	}
	// A pull request's merge ref is not something to dispatch on.
	if strings.HasPrefix(fullRef, "refs/pull/") {
		return store.Run{}, refuse(http.StatusBadRequest, "a pull request ref cannot be dispatched")
	}
	// The full ref is unambiguous where a bare name could be a branch or a tag
	// (the commits API takes it, checked against GitHub).
	inst, err := h.installationFor(ctx, req.Repo)
	if errors.Is(err, github.ErrNotFound) {
		return store.Run{}, refuse(http.StatusNotFound, "the GitHub App is not installed on %s", req.Repo)
	}
	if err != nil {
		return store.Run{}, fmt.Errorf("finding the installation for %s: %w", req.Repo, err)
	}
	sha, err := h.Source.ResolveRef(ctx, inst, req.Repo, fullRef)
	if errors.Is(err, github.ErrNotFound) {
		return store.Run{}, refuse(http.StatusNotFound, "no such ref %q in %s", req.Ref, req.Repo)
	}
	if err != nil {
		return store.Run{}, fmt.Errorf("resolving %s: %w", req.Ref, err)
	}
	text, err := h.Source.FileAt(ctx, inst, req.Repo, req.Workflow, sha)
	if errors.Is(err, github.ErrNotFound) {
		return store.Run{}, refuse(http.StatusNotFound, "%s does not exist at %s", req.Workflow, sha)
	}
	if err != nil {
		return store.Run{}, fmt.Errorf("reading %s: %w", req.Workflow, err)
	}
	w, err := workflow.Parse(text)
	if err != nil {
		return store.Run{}, refuse(http.StatusUnprocessableEntity, "%s is not a valid workflow: %v", req.Workflow, err)
	}
	res, err := trigger.Match(w.On, trigger.Event{Name: "workflow_dispatch", Ref: fullRef})
	if err != nil {
		return store.Run{}, refuse(http.StatusUnprocessableEntity, "%s: %v", req.Workflow, err)
	}
	if !res.Triggered {
		return store.Run{}, refuse(http.StatusUnprocessableEntity, "%s: %s", req.Workflow, res.Reason)
	}
	inputs, err := trigger.DispatchInputs(w.On, req.Inputs)
	if err != nil {
		return store.Run{}, refuse(http.StatusUnprocessableEntity, "%v", err)
	}
	inputsJSON, err := json.Marshal(inputs)
	if err != nil {
		return store.Run{}, err
	}
	actor := req.Actor
	if actor == "" {
		actor = "api"
	}
	payload, _ := json.Marshal(map[string]any{
		"ref": fullRef, "inputs": json.RawMessage(inputsJSON),
		"repository": map[string]any{"full_name": req.Repo}, "sender": map[string]any{"login": actor},
	})
	var id [12]byte
	rand.Read(id[:])
	ev, _, err := h.Store.RecordEvent(ctx, store.Event{
		DeliveryID: "dispatch-" + hex.EncodeToString(id[:]), Kind: "workflow_dispatch", Repo: req.Repo, InstallationID: inst,
		Payload: payload, Status: store.EventHandled, Note: "started by hand through the API",
	})
	if err != nil {
		return store.Run{}, err
	}
	run, _, err := h.Store.CreateRun(ctx, store.Run{
		EventID: ev.ID, Repo: req.Repo, WorkflowPath: req.Workflow, WorkflowName: nameOf(w, req.Workflow), Event: "workflow_dispatch",
		Ref: fullRef, SHA: sha, Actor: actor, Inputs: string(inputsJSON), WorkflowText: string(text),
	})
	if err != nil {
		return store.Run{}, err
	}
	h.log().Info("run dispatched", "run", run.ID, "repo", req.Repo, "workflow", req.Workflow, "ref", fullRef)
	if h.Notify != nil {
		h.Notify()
	}
	return run, nil
}
