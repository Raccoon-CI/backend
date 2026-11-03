package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Raccoon-CI/backend/internal/server/storage"
)

// StorageAdmin is storage.Service.
type StorageAdmin interface {
	Put(ctx context.Context, org string, st storage.Settings) ([]string, error)
	View(ctx context.Context, org string) (storage.View, error)
	Delete(ctx context.Context, org string) (bool, error)
}

func (a *API) storageOff(w http.ResponseWriter) bool {
	if a.Storage == nil {
		fail(w, http.StatusServiceUnavailable, "organization buckets are not available: the server has no master key")
		return true
	}
	return false
}

func answerStorage(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, storage.ErrInvalid):
		fail(w, http.StatusBadRequest, err.Error())
	default:
		fail(w, http.StatusInternalServerError, "could not read or keep the storage settings")
	}
	return true
}

func (a *API) getStorage(w http.ResponseWriter, r *http.Request) {
	if a.storageOff(w) {
		return
	}
	v, err := a.Storage.View(r.Context(), r.URL.Query().Get("org"))
	if answerStorage(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": v.Configured, "endpoint": v.Endpoint, "region": v.Region,
		"bucket": v.Bucket, "prefix": v.Prefix, "path_style": v.PathStyle})
}

// putStorage tries the bucket and keeps its settings only if it works.
func (a *API) putStorage(w http.ResponseWriter, r *http.Request) {
	if a.storageOff(w) {
		return
	}
	var b struct {
		Org string `json:"org"`
		storage.Settings
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		fail(w, http.StatusBadRequest, "the body must be a JSON object with org, bucket, access_key, secret_key and, as needed, endpoint, region, prefix, path_style")
		return
	}
	warnings, err := a.Storage.Put(r.Context(), strings.ToLower(b.Org), b.Settings)
	if answerStorage(w, err) {
		return
	}
	if warnings == nil {
		warnings = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"warnings": warnings})
}

func (a *API) deleteStorage(w http.ResponseWriter, r *http.Request) {
	if a.storageOff(w) {
		return
	}
	ok, err := a.Storage.Delete(r.Context(), r.URL.Query().Get("org"))
	if answerStorage(w, err) {
		return
	}
	if !ok {
		fail(w, http.StatusNotFound, "that organization has no bucket of its own")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
