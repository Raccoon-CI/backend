package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunTokenAsksForOneRepositoryWithTheGivenPermissionsOnly(t *testing.T) {
	var got struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("no App JWT: %q", r.Header.Get("Authorization"))
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(201)
		fmt.Fprint(w, `{"token":"ghs_job","expires_at":"2030-01-01T00:00:00Z"}`)
	}))
	defer srv.Close()
	app := &App{ID: 1, Key: newKey(t), Base: srv.URL}
	tok, err := app.RunToken(context.Background(), 42, "acme/web", map[string]string{"contents": "read", "issues": "write"})
	if err != nil || tok != "ghs_job" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	if path != "/app/installations/42/access_tokens" || len(got.Repositories) != 1 || got.Repositories[0] != "web" ||
		got.Permissions["issues"] != "write" || got.Permissions["contents"] != "read" || len(got.Permissions) != 2 {
		t.Fatalf("path %q, body %+v", path, got)
	}
	// It is not cached: a second call is a second token request.
	got.Permissions = nil // decoding into a map would merge
	app.RunToken(context.Background(), 42, "acme/web", map[string]string{"contents": "read"})
	if len(got.Permissions) != 1 {
		t.Fatalf("the second call did not ask GitHub: %+v", got)
	}
}

func TestRunTokenRefusalsAreErrorsWithoutACredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		fmt.Fprint(w, `{"message":"The permissions requested are not granted to this installation."}`)
	}))
	defer srv.Close()
	app := &App{ID: 1, Key: newKey(t), Base: srv.URL}
	_, err := app.RunToken(context.Background(), 42, "acme/web", map[string]string{"issues": "write"})
	if err == nil || !strings.Contains(err.Error(), "not granted") {
		t.Fatalf("err = %v", err)
	}
	if _, err := app.RunToken(context.Background(), 0, "acme/web", nil); err == nil {
		t.Fatal("no installation must be an error")
	}
	if _, err := app.RunToken(context.Background(), 42, "noslash", nil); err == nil {
		t.Fatal("a name without an owner must be an error")
	}
}
