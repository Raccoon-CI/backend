package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Raccoon-CI/raccoonci/internal/server/store"
)

const token = "api-token-for-tests"

func setup(t *testing.T) (*API, http.Handler) {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	ev, _, _ := st.RecordEvent(ctx, store.Event{DeliveryID: "d", Kind: "push", Repo: "acme/web", Payload: []byte(`{"secret":"in the payload"}`)})
	st.FinishEvent(ctx, ev.ID, store.EventHandled, `[{"path":"ci.yml","result":"run"}]`)
	for i, repo := range []string{"acme/web", "acme/api", "acme/web"} {
		if _, _, err := st.CreateRun(ctx, store.Run{EventID: ev.ID, Repo: repo, WorkflowPath: "w" + string(rune('a'+i)), WorkflowName: "CI",
			Event: "push", Ref: "refs/heads/main", SHA: "abc", WorkflowText: "on: push\n"}); err != nil {
			t.Fatal(err)
		}
	}
	a := &API{Store: st, Token: token, Version: "test"}
	return a, a.Handler()
}

func get(h http.Handler, path, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestTheAPIIsClosedWithoutTheRightToken(t *testing.T) {
	a, h := setup(t)
	for _, path := range []string{"/v1/runs", "/v1/runs/1", "/v1/events/1"} {
		if rec := get(h, path, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a token: %d", path, rec.Code)
		}
		if rec := get(h, path, "Bearer wrong"); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with a wrong token: %d", path, rec.Code)
		}
		if rec := get(h, path, token); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with the token but no Bearer: %d", path, rec.Code)
		}
		if rec := get(h, path, "Bearer "+token); rec.Code != http.StatusOK {
			t.Errorf("%s with the token: %d", path, rec.Code)
		}
	}
	a.Token = ""
	if rec := get(a.Handler(), "/v1/runs", "Bearer "); rec.Code != http.StatusUnauthorized {
		t.Errorf("with no bootstrap token an empty token must not open the API: %d", rec.Code)
	}
	if rec := get(a.Handler(), "/v1/runs", "Bearer "+token); rec.Code != http.StatusUnauthorized {
		t.Errorf("the bootstrap token works though none is configured: %d", rec.Code)
	}
	closed := &API{}
	if rec := get(closed.Handler(), "/v1/runs", "Bearer anything"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("with no way to tell tokens the API is closed: %d", rec.Code)
	}
}

// The spec must describe every route, with its method, and every operation
// it describes must be a route.
func TestTheSpecAndTheRoutesAgree(t *testing.T) {
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(openapiSpec, &spec); err != nil {
		t.Fatal(err)
	}
	a := &API{}
	have := map[string]bool{}
	for _, r := range a.routes() {
		method, path, _ := strings.Cut(r.pattern, " ")
		have[strings.ToLower(method)+" "+path] = true
		if _, ok := spec.Paths[path][strings.ToLower(method)]; !ok {
			t.Errorf("%s is a route but not in openapi.yaml", r.pattern)
		}
	}
	for path, ops := range spec.Paths {
		for method := range ops {
			if !have[method+" "+path] {
				t.Errorf("openapi.yaml has %s %s, which is not a route", strings.ToUpper(method), path)
			}
		}
	}
}

func TestHealthAndSpecNeedNoToken(t *testing.T) {
	_, h := setup(t)
	if rec := get(h, "/healthz", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"version":"test"`) {
		t.Fatalf("healthz: %d %s", rec.Code, rec.Body)
	}
	if rec := get(h, "/v1/openapi.yaml", ""); rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "openapi: 3.0.3") {
		t.Fatalf("spec: %d", rec.Code)
	}
}

func TestRunsAreListedNewestFirstWithPaging(t *testing.T) {
	_, h := setup(t)
	var page struct {
		Runs       []map[string]any `json:"runs"`
		NextBefore int64            `json:"next_before"`
	}
	rec := get(h, "/v1/runs?limit=2", "Bearer "+token)
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != 2 || page.Runs[0]["id"].(float64) != 3 || page.NextBefore != 2 {
		t.Fatalf("page 1 = %s", rec.Body)
	}
	if _, has := page.Runs[0]["workflow"]; has {
		t.Fatal("a list must not carry the workflow text")
	}
	rec = get(h, "/v1/runs?limit=2&before=2", "Bearer "+token)
	json.Unmarshal(rec.Body.Bytes(), &page)
	if len(page.Runs) != 1 || page.NextBefore != 0 {
		t.Fatalf("page 2 = %s", rec.Body)
	}
	rec = get(h, "/v1/runs?repo=acme/api", "Bearer "+token)
	json.Unmarshal(rec.Body.Bytes(), &page)
	if len(page.Runs) != 1 || page.Runs[0]["repo"] != "acme/api" {
		t.Fatalf("repo filter = %s", rec.Body)
	}
	for _, bad := range []string{"limit=0", "limit=201", "limit=x", "before=0", "before=x"} {
		if rec := get(h, "/v1/runs?"+bad, "Bearer "+token); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", bad, rec.Code)
		}
	}
}

func TestOneRunHasItsWorkflowAndAnEventHasNoPayload(t *testing.T) {
	_, h := setup(t)
	rec := get(h, "/v1/runs/2", "Bearer "+token)
	var run map[string]any
	json.Unmarshal(rec.Body.Bytes(), &run)
	if run["workflow"] != "on: push\n" || run["repo"] != "acme/api" || run["status"] != "queued" {
		t.Fatalf("run = %s", rec.Body)
	}
	rec = get(h, "/v1/events/1", "Bearer "+token)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "in the payload") || !strings.Contains(rec.Body.String(), `"status":"handled"`) {
		t.Fatalf("event = %d %s", rec.Code, rec.Body)
	}
	for _, p := range []string{"/v1/runs/99", "/v1/events/99"} {
		if rec := get(h, p, "Bearer "+token); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, rec.Code)
		}
	}
	for _, p := range []string{"/v1/runs/abc", "/v1/runs/0", "/v1/events/-1"} {
		if rec := get(h, p, "Bearer "+token); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", p, rec.Code)
		}
	}
}
