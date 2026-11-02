package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"raccoonci/internal/server/store"
)

type fakeSignIn struct{ logins, callbacks int }

func (f *fakeSignIn) Login(w http.ResponseWriter, r *http.Request) { f.logins++; w.WriteHeader(302) }
func (f *fakeSignIn) Callback(w http.ResponseWriter, r *http.Request) {
	f.callbacks++
	w.WriteHeader(200)
}

func TestTheSignInRoutesNeedNoTokenAndAreOffUntilSetUp(t *testing.T) {
	a, h := setup(t)
	for _, p := range []string{"/v1/auth/github/login", "/v1/auth/github/callback"} {
		if rec := call(h, "GET", p, "", ""); rec.Code != 503 {
			t.Errorf("%s while off: %d", p, rec.Code)
		}
	}
	f := &fakeSignIn{}
	a.Auth = f
	h = a.Handler()
	if rec := call(h, "GET", "/v1/auth/github/login", "", ""); rec.Code != 302 || f.logins != 1 {
		t.Errorf("login: %d", rec.Code)
	}
	if rec := call(h, "GET", "/v1/auth/github/callback?code=x", "", ""); rec.Code != 200 || f.callbacks != 1 {
		t.Errorf("callback: %d", rec.Code)
	}
}

func session(t *testing.T, a *API, repos ...string) string {
	t.Helper()
	tok := "rcs_test_" + strings.Join(repos, "_")
	if _, err := a.Store.CreateSession(ctx(), "github:alice", "alice", store.RoleRead, HashToken(tok), repos, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestMeSaysWhoTheTokenIsAndALimitedSessionMayAskAndSignOut(t *testing.T) {
	a, h := setup(t)
	sess := session(t, a, "acme/api")
	rec := call(h, "GET", "/v1/auth/me", sess, "")
	var me struct {
		Kind      string   `json:"kind"`
		User      string   `json:"user"`
		Role      string   `json:"role"`
		Repos     []string `json:"repos"`
		ExpiresAt string   `json:"expires_at"`
	}
	json.Unmarshal(rec.Body.Bytes(), &me)
	if rec.Code != 200 || me.Kind != "session" || me.User != "alice" || me.Role != "read" || len(me.Repos) != 1 || me.ExpiresAt == "" {
		t.Fatalf("me (session) = %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "GET", "/v1/auth/me", token, ""); !strings.Contains(rec.Body.String(), `"kind":"bootstrap"`) || !strings.Contains(rec.Body.String(), `"role":"admin"`) {
		t.Errorf("me (bootstrap) = %s", rec.Body)
	}
	apiTok := limited(t, h, "acme/*")
	if rec := call(h, "GET", "/v1/auth/me", apiTok, ""); !strings.Contains(rec.Body.String(), `"kind":"api"`) || !strings.Contains(rec.Body.String(), `"name":"dash"`) {
		t.Errorf("me (api) = %s", rec.Body)
	}
	if rec := call(h, "GET", "/v1/auth/me", "", ""); rec.Code != 401 {
		t.Errorf("me without a token: %d", rec.Code)
	}
	// A session sees what it may and is signed out on request.
	if rec := call(h, "GET", "/v1/runs/2", sess, ""); rec.Code != 200 {
		t.Errorf("its own repository: %d", rec.Code)
	}
	if rec := call(h, "POST", "/v1/auth/logout", token, ""); rec.Code != 400 {
		t.Errorf("signing the bootstrap token out: %d", rec.Code)
	}
	if rec := call(h, "POST", "/v1/auth/logout", sess, ""); rec.Code != 204 {
		t.Fatalf("logout: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "GET", "/v1/runs/2", sess, ""); rec.Code != 401 {
		t.Errorf("after logout: %d, want 401", rec.Code)
	}
}

func TestASessionOfAnotherUserListsOnlyItsRepositories(t *testing.T) {
	a, h := setup(t)
	sess := session(t, a, "acme/api")
	rec := call(h, "GET", "/v1/runs", sess, "")
	names := repoNames(t, rec.Body)
	if rec.Code != 200 || len(names) != 1 || names[0] != "acme/api" {
		t.Fatalf("%d %v", rec.Code, names)
	}
	if rec := call(h, "GET", "/v1/runs/1", sess, ""); rec.Code != 404 {
		t.Errorf("another repository's run: %d", rec.Code)
	}
}
