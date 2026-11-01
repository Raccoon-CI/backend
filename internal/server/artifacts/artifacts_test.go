package artifacts

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestNeedsADirectoryAndAnAddress(t *testing.T) {
	if _, err := New(Config{Dir: t.TempDir()}); err == nil {
		t.Fatal("no public URL was accepted")
	}
	if _, err := New(Config{PublicURL: "https://x"}); err == nil {
		t.Fatal("no directory was accepted")
	}
}
