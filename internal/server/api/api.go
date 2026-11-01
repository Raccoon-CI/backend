// Package api is the server's HTTP API: what the frontend and the CLI talk to
// and nothing else. JSON over HTTP; openapi.yaml, served at
// /v1/openapi.yaml, is its description and must be kept in step with this file.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"raccoonci/internal/server/intake"
	"raccoonci/internal/server/store"
)

//go:embed openapi.yaml
var openapiSpec []byte

const (
	defaultLimit = 50
	maxLimit     = 200
)

// API serves the read side of the server.
type API struct {
	Store store.Store
	// Dispatcher starts runs by hand and repeats them; nil switches that off.
	Dispatcher Dispatcher
	// Canceller stops a run that is running; nil means none can be stopped
	// (a queued run can still be cancelled).
	Canceller Canceller
	// Token is the bearer token the /v1 routes need. Empty switches them off
	// (503): the API is closed unless a token is configured.
	Token   string
	Version string
}

// Dispatcher creates runs on request (intake.Handler).
type Dispatcher interface {
	Dispatch(ctx context.Context, req intake.DispatchRequest) (store.Run, error)
	Rerun(ctx context.Context, runID int64) (store.Run, error)
}

// Canceller stops a running run (execute.Executor); false when it is not
// running one with that id.
type Canceller interface {
	Cancel(runID int64) bool
}

type route struct {
	pattern string // "METHOD /path", as http.ServeMux takes it
	handler http.HandlerFunc
	open    bool // needs no token
}

// routes is every route of the API. A test checks that each one is in
// openapi.yaml, so the file cannot drift from the code.
func (a *API) routes() []route {
	return []route{
		{"GET /healthz", a.health, true},
		{"GET /v1/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/yaml")
			w.Write(openapiSpec)
		}, true},
		{"GET /v1/runs", a.listRuns, false},
		{"POST /v1/runs", a.dispatchRun, false},
		{"GET /v1/runs/{id}", a.getRun, false},
		{"POST /v1/runs/{id}/cancel", a.cancelRun, false},
		{"POST /v1/runs/{id}/rerun", a.rerunRun, false},
		{"GET /v1/runs/{id}/jobs", a.listJobs, false},
		{"GET /v1/runs/{id}/log", a.readLog, false},
		{"GET /v1/events/{id}", a.getEvent, false},
	}
}

// Handler returns the routes.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, r := range a.routes() {
		h := r.handler
		if !r.open {
			h = a.auth(h)
		}
		mux.HandleFunc(r.pattern, h)
	}
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": a.Version})
}

// auth wraps a route with the bearer token check. Both sides are hashed first
// so that the comparison takes the same time whatever the guess looks like.
func (a *API) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.Token == "" {
			fail(w, http.StatusServiceUnavailable, "the API is closed: no API token is configured")
			return
		}
		given, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		want, have := sha256.Sum256([]byte(a.Token)), sha256.Sum256([]byte(given))
		if !ok || subtle.ConstantTimeCompare(want[:], have[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			fail(w, http.StatusUnauthorized, "missing or wrong token")
			return
		}
		next(w, r)
	}
}

type runJSON struct {
	ID           int64     `json:"id"`
	EventID      int64     `json:"event_id"`
	Repo         string    `json:"repo"`
	WorkflowPath string    `json:"workflow_path"`
	WorkflowName string    `json:"workflow_name"`
	Event        string    `json:"event"`
	Ref          string    `json:"ref"`
	SHA          string    `json:"sha"`
	BaseSHA      string    `json:"base_sha,omitempty"`
	PullRequest  int       `json:"pull_request,omitempty"`
	RerunOf      int64     `json:"rerun_of,omitempty"`
	Fork         bool      `json:"fork"`
	Actor        string    `json:"actor"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// Workflow is the workflow file the run started from; only the detail has it.
	Workflow string `json:"workflow,omitempty"`
}

func toRun(r store.Run) runJSON {
	return runJSON{ID: r.ID, EventID: r.EventID, Repo: r.Repo, WorkflowPath: r.WorkflowPath, WorkflowName: r.WorkflowName,
		Event: r.Event, Ref: r.Ref, SHA: r.SHA, BaseSHA: r.BaseSHA, PullRequest: r.PullRequest, RerunOf: r.RerunOf, Fork: r.Fork, Actor: r.Actor,
		Status: r.Status, Conclusion: r.Conclusion, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Workflow: r.WorkflowText}
}

func (a *API) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.RunFilter{Repo: q.Get("repo"), Limit: defaultLimit}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLimit {
			fail(w, http.StatusBadRequest, "limit must be a number from 1 to "+strconv.Itoa(maxLimit))
			return
		}
		f.Limit = n
	}
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			fail(w, http.StatusBadRequest, "before must be a run id")
			return
		}
		f.Before = n
	}
	runs, err := a.Store.ListRuns(r.Context(), f)
	if err != nil {
		fail(w, http.StatusInternalServerError, "could not read runs")
		return
	}
	out := struct {
		Runs []runJSON `json:"runs"`
		// NextBefore is the value of "before" for the next page; 0 when this was the last.
		NextBefore int64 `json:"next_before"`
	}{Runs: make([]runJSON, 0, len(runs))}
	for _, run := range runs {
		out.Runs = append(out.Runs, toRun(run))
	}
	if len(runs) == f.Limit {
		out.NextBefore = runs[len(runs)-1].ID
	}
	writeJSON(w, http.StatusOK, out)
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		fail(w, http.StatusBadRequest, "the id must be a number")
		return 0, false
	}
	return id, true
}

func (a *API) getRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	run, err := a.Store.GetRun(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		fail(w, http.StatusNotFound, "no such run")
	case err != nil:
		fail(w, http.StatusInternalServerError, "could not read the run")
	default:
		writeJSON(w, http.StatusOK, toRun(run))
	}
}

type eventJSON struct {
	ID         int64     `json:"id"`
	DeliveryID string    `json:"delivery_id"`
	Kind       string    `json:"kind"`
	Action     string    `json:"action,omitempty"`
	Repo       string    `json:"repo"`
	ReceivedAt time.Time `json:"received_at"`
	Status     string    `json:"status"`
	// Note says what was decided for each workflow file (a JSON text), or why
	// the delivery failed. The payload itself is not part of the API.
	Note string `json:"note,omitempty"`
}

func (a *API) getEvent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ev, err := a.Store.GetEvent(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		fail(w, http.StatusNotFound, "no such event")
	case err != nil:
		fail(w, http.StatusInternalServerError, "could not read the event")
	default:
		writeJSON(w, http.StatusOK, eventJSON{ID: ev.ID, DeliveryID: ev.DeliveryID, Kind: ev.Kind, Action: ev.Action,
			Repo: ev.Repo, ReceivedAt: ev.ReceivedAt, Status: ev.Status, Note: ev.Note})
	}
}
