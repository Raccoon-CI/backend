package artifacts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Raccoon-CI/raccoonci/internal/blob"
)

func newService(t *testing.T) (*Service, http.Handler) {
	t.Helper()
	s, err := New(Config{Dir: t.TempDir(), PublicURL: "https://ci.example"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	mux := http.NewServeMux()
	s.Mount(mux)
	return s, mux
}

func list(h http.Handler, token, run string) int {
	req := httptest.NewRequest("POST", "/twirp/github.actions.results.api.v1.ArtifactService/ListArtifacts",
		strings.NewReader(`{"workflow_run_backend_id":"`+run+`","workflow_job_run_backend_id":"build-1"}`))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestAJobsTokenOpensItsOwnRunOnly(t *testing.T) {
	s, h := newService(t)
	env, err := s.JobEnv(7, "build-1")
	if err != nil {
		t.Fatal(err)
	}
	if env["ACTIONS_RESULTS_URL"] != "https://ci.example/" || env["ACTIONS_RUNTIME_TOKEN"] == "" {
		t.Fatalf("env = %v", env)
	}
	tok := env["ACTIONS_RUNTIME_TOKEN"]
	if c := list(h, tok, "7"); c != http.StatusOK {
		t.Fatalf("its own run: %d", c)
	}
	if c := list(h, tok, "8"); c != http.StatusForbidden {
		t.Fatalf("another run: %d, want 403", c)
	}
	if c := list(h, "", "7"); c != http.StatusUnauthorized {
		t.Fatalf("no token: %d, want 401", c)
	}
	other, _ := s.JobEnv(8, "build-1")
	if c := list(h, other["ACTIONS_RUNTIME_TOKEN"], "7"); c != http.StatusForbidden {
		t.Fatalf("run 8's token on run 7: %d, want 403", c)
	}
}

func TestAnOrganizationsRunsGoToItsBucketAndOthersToTheDisk(t *testing.T) {
	var made []blob.S3Config
	orgs := map[int64]string{1: "acme", 2: "other", 3: "acme"}
	version := "v1"
	s, err := New(Config{
		Dir: t.TempDir(), PublicURL: "https://ci.example",
		RunOrg: func(_ context.Context, run int64) (string, error) {
			if o, ok := orgs[run]; ok {
				return o, nil
			}
			return "", errors.New("no such run")
		},
		S3For: func(_ context.Context, org string) (*blob.S3Config, string, error) {
			if org == "acme" {
				return &blob.S3Config{Bucket: "acme-bucket", Prefix: "ci/"}, version, nil
			}
			return nil, "disk", nil
		},
		NewStore: func(c blob.S3Config) (blob.Store, error) {
			made = append(made, c)
			return blob.NewDir(t.TempDir(), "https://ci.example", bytes.Repeat([]byte("k"), 32))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	mux := http.NewServeMux()
	s.Mount(mux)
	tok := func(run int64) string {
		env, _ := s.JobEnv(run, "build-1")
		return env["ACTIONS_RUNTIME_TOKEN"]
	}

	for _, run := range []int64{1, 3, 2, 1} {
		if c := list(mux, tok(run), fmt.Sprint(run)); c != 200 {
			t.Fatalf("run %d: %d", run, c)
		}
	}
	if len(made) != 1 || made[0].Bucket != "acme-bucket" || made[0].Prefix != "ci/artifacts/" {
		t.Fatalf("buckets made = %+v: one for the organization that has one, under its prefix and artifacts/", made)
	}
	// A new version of the bucket's settings is a new service; the old one is not asked again.
	version = "v2"
	list(mux, tok(1), "1")
	if len(made) != 2 {
		t.Fatalf("buckets made = %d after the settings changed", len(made))
	}
	// An upload is routed by the run in its path, and an unknown run is refused.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("PUT", "/_blob/1/abc?exp=1&sig=x", strings.NewReader("x")))
	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("an upload with a bad signature: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("PUT", "/_blob/99/abc?exp=1&sig=x", strings.NewReader("x")))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("an upload for a run that does not exist: %d, want 500 (the store cannot be told)", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("PUT", "/_blob/notanumber/abc", strings.NewReader("x")))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an upload naming no run: %d", rec.Code)
	}
}

func TestNeedsADirectoryAndAnAddress(t *testing.T) {
	if _, err := New(Config{Dir: t.TempDir()}); err == nil {
		t.Fatal("no public URL was accepted")
	}
	if _, err := New(Config{PublicURL: "https://x"}); err == nil {
		t.Fatal("no directory was accepted")
	}
}
