package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Raccoon-CI/raccoonci/pkg/artifact"
)

type fakeArtifacts struct {
	list map[int64][]artifact.Info
	err  error
}

func (f *fakeArtifacts) List(_ context.Context, run int64) ([]artifact.Info, error) {
	return f.list[run], f.err
}

func (f *fakeArtifacts) DownloadURL(_ context.Context, run, id int64) (string, error) {
	for _, a := range f.list[run] {
		if a.ID == id {
			return fmt.Sprintf("https://bucket.example/%d/%d?sig=x", run, id), nil
		}
	}
	return "", artifact.ErrNoArtifact
}

func TestListAndDownloadAnArtifactOfARun(t *testing.T) {
	a, _, run := runSetup(t)
	exp := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	a.Artifacts = &fakeArtifacts{list: map[int64][]artifact.Info{run.ID: {
		{ID: 3, Name: "logs", Job: "build", Size: 1234, Digest: "sha256:ab", Created: exp.Add(-90 * 24 * time.Hour), Expires: exp},
	}}}
	h := a.Handler()
	rec := do(h, "GET", fmt.Sprintf("/v1/runs/%d/artifacts", run.ID), "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"logs"`) || !strings.Contains(rec.Body.String(), `"size":1234`) ||
		!strings.Contains(rec.Body.String(), `"expires_at":"2027-01-01T00:00:00Z"`) || !strings.Contains(rec.Body.String(), `"job":"build"`) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	rec = do(h, "GET", fmt.Sprintf("/v1/runs/%d/artifacts/3/download", run.ID), "")
	if rec.Code != 302 || rec.Header().Get("Location") != fmt.Sprintf("https://bucket.example/%d/3?sig=x", run.ID) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("download: %d %v", rec.Code, rec.Header())
	}
	if rec := do(h, "GET", fmt.Sprintf("/v1/runs/%d/artifacts/4/download", run.ID), ""); rec.Code != 404 {
		t.Fatalf("an artifact the run does not have: %d", rec.Code)
	}
	if rec := do(h, "GET", fmt.Sprintf("/v1/runs/%d/artifacts/x/download", run.ID), ""); rec.Code != 400 {
		t.Fatalf("a bad artifact id: %d", rec.Code)
	}
	if rec := do(h, "GET", "/v1/runs/999/artifacts", ""); rec.Code != 404 {
		t.Fatalf("a run that does not exist: %d", rec.Code)
	}
	// A run without artifacts has an empty list, not null.
	other := &fakeArtifacts{list: map[int64][]artifact.Info{}}
	a.Artifacts = other
	if rec := do(h, "GET", fmt.Sprintf("/v1/runs/%d/artifacts", run.ID), ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"artifacts":[]`) {
		t.Fatalf("empty: %d %s", rec.Code, rec.Body)
	}
	other.err = errors.New("bucket down")
	if rec := do(h, "GET", fmt.Sprintf("/v1/runs/%d/artifacts", run.ID), ""); rec.Code != 502 || strings.Contains(rec.Body.String(), "bucket down") {
		t.Fatalf("a storage error: %d %s (the message must not leak)", rec.Code, rec.Body)
	}
}

func TestArtifactRoutesAreOffWithoutAService(t *testing.T) {
	a, _, run := runSetup(t)
	h := a.Handler()
	for _, p := range []string{"/v1/runs/%d/artifacts", "/v1/runs/%d/artifacts/1/download"} {
		if rec := do(h, "GET", fmt.Sprintf(p, run.ID), ""); rec.Code != 503 {
			t.Fatalf("%s: %d, want 503", p, rec.Code)
		}
	}
}
