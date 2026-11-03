package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Raccoon-CI/backend/internal/server/store"
	"github.com/Raccoon-CI/lib/pkg/artifact"
)

// ArtifactStore is what the artifact routes need (artifacts.Service).
type ArtifactStore interface {
	List(ctx context.Context, runID int64) ([]artifact.Info, error)
	// DownloadURL presigns a link to one artifact; artifact.ErrNoArtifact when
	// the run has no such one.
	DownloadURL(ctx context.Context, runID, artifactID int64) (string, error)
}

type artifactJSON struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Job       string    `json:"job"`
	Size      int64     `json:"size"`
	Digest    string    `json:"digest,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// runExists answers 404 or 500 itself when the run cannot be used.
func (a *API) runExists(w http.ResponseWriter, r *http.Request, id int64) bool {
	if _, err := a.Store.GetRun(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "no such run")
		return false
	} else if err != nil {
		fail(w, http.StatusInternalServerError, "could not read the run")
		return false
	}
	return true
}

// listArtifacts is GET /v1/runs/{id}/artifacts.
func (a *API) listArtifacts(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if a.Artifacts == nil {
		fail(w, http.StatusServiceUnavailable, "artifacts are not available: the server does not run jobs")
		return
	}
	if !a.runExists(w, r, id) {
		return
	}
	list, err := a.Artifacts.List(r.Context(), id)
	if err != nil {
		fail(w, http.StatusBadGateway, "could not read the artifacts")
		return
	}
	out := struct {
		Artifacts []artifactJSON `json:"artifacts"`
	}{Artifacts: make([]artifactJSON, 0, len(list))}
	for _, x := range list {
		out.Artifacts = append(out.Artifacts, artifactJSON{ID: x.ID, Name: x.Name, Job: x.Job, Size: x.Size, Digest: x.Digest,
			CreatedAt: x.Created, ExpiresAt: x.Expires})
	}
	writeJSON(w, http.StatusOK, out)
}

// downloadArtifact is GET /v1/runs/{id}/artifacts/{artifact}/download: a
// redirect to a link that works for an hour, straight to the bucket.
func (a *API) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	artID, err := strconv.ParseInt(r.PathValue("artifact"), 10, 64)
	if err != nil || artID < 1 {
		fail(w, http.StatusBadRequest, "the artifact id must be a number")
		return
	}
	if a.Artifacts == nil {
		fail(w, http.StatusServiceUnavailable, "artifacts are not available: the server does not run jobs")
		return
	}
	if !a.runExists(w, r, id) {
		return
	}
	u, err := a.Artifacts.DownloadURL(r.Context(), id, artID)
	switch {
	case errors.Is(err, artifact.ErrNoArtifact):
		fail(w, http.StatusNotFound, "no such artifact in this run")
	case err != nil:
		fail(w, http.StatusBadGateway, "could not make a download link")
	default:
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, u, http.StatusFound)
	}
}
