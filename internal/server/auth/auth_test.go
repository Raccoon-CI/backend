package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/server/store"
)

// fakeGitHub is GitHub's side of the flow.
type fakeGitHub struct {
	mu       sync.Mutex
	codes    map[string]string  // code -> the user's token it gives (used once)
	users    map[string]string  // user token -> login
	installs map[string][]int64 // login -> installations
	repos    map[int64][]string // installation -> repositories the user reaches
	revoked  []string           // user tokens revoked
	asked    []string
	basic    string
}

func (f *fakeGitHub) handler(t *testing.T, clientID, secret string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Form.Get("client_id") != clientID || r.Form.Get("client_secret") != secret || r.Form.Get("redirect_uri") != "https://ci.example.com/v1/auth/github/callback" {
			t.Errorf("token request: %v", r.Form)
		}
		tok, ok := f.codes[r.Form.Get("code")]
		if !ok {
			json.NewEncoder(w).Encode(map[string]string{"error": "bad_verification_code"})
			return
		}
		delete(f.codes, r.Form.Get("code")) // single use
		json.NewEncoder(w).Encode(map[string]string{"access_token": tok, "token_type": "bearer"})
	})
	user := func(r *http.Request) (string, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.asked = append(f.asked, r.URL.Path)
		l, ok := f.users[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		return l, ok
	}
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		l, ok := user(r)
		if !ok {
			w.WriteHeader(401)
			return
		}
		fmt.Fprintf(w, `{"login":%q}`, l)
	})
	mux.HandleFunc("GET /user/installations", func(w http.ResponseWriter, r *http.Request) {
		l, ok := user(r)
		if !ok {
			w.WriteHeader(401)
			return
		}
		var parts []string
		for _, id := range f.installs[l] {
			parts = append(parts, fmt.Sprintf(`{"id":%d}`, id))
		}
		fmt.Fprintf(w, `{"total_count":%d,"installations":[%s]}`, len(parts), strings.Join(parts, ","))
	})
	mux.HandleFunc("GET /user/installations/{id}/repositories", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := user(r); !ok {
			w.WriteHeader(401)
			return
		}
		var id int64
		fmt.Sscan(r.PathValue("id"), &id)
		var parts []string
		for _, n := range f.repos[id] {
			parts = append(parts, fmt.Sprintf(`{"full_name":%q}`, n))
		}
		fmt.Fprintf(w, `{"total_count":%d,"repositories":[%s]}`, len(parts), strings.Join(parts, ","))
	})
	mux.HandleFunc("DELETE /applications/{id}/token", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			AccessToken string `json:"access_token"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		u, p, _ := r.BasicAuth()
		f.mu.Lock()
		f.revoked = append(f.revoked, b.AccessToken)
		f.basic = u + ":" + p
		f.mu.Unlock()
		w.WriteHeader(204)
	})
	return mux
}

type env struct {
	t   *testing.T
	st  *store.SQLite
	gh  *fakeGitHub
	svc *Service
	now time.Time
}

func newEnv(t *testing.T, edit func(*Config)) *env {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC), gh: &fakeGitHub{
		codes:    map[string]string{"code-alice": "gho_alice", "code-bob": "gho_bob", "code-admin": "gho_admin", "code-nobody": "gho_nobody"},
		users:    map[string]string{"gho_alice": "Alice", "gho_bob": "bob", "gho_admin": "Boss", "gho_nobody": "nobody"},
		installs: map[string][]int64{"Alice": {1, 2}, "bob": {1}, "Boss": {1}, "nobody": {}},
		repos:    map[int64][]string{1: {"Acme/web", "acme/api"}, 2: {"Solo/tool", "acme/web"}},
	}}
	// The store judges whether a session has expired, so it must tell the same
	// time as the service; with the real clock these tests failed once the real
	// date was a day past the fixed one.
	st.Now = func() time.Time { return e.now }
	srv := httptest.NewServer(e.gh.handler(t, "Iv1.client", "client-secret"))
	t.Cleanup(srv.Close)
	cfg := Config{ClientID: "Iv1.client", ClientSecret: "client-secret", PublicURL: "https://ci.example.com", Admins: []string{" Boss "},
		Redirects: []string{"https://app.example.com/"}, Sessions: st, WebBase: srv.URL, APIBase: srv.URL, Now: func() time.Time { return e.now }}
	if edit != nil {
		edit(&cfg)
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	return e
}

// start runs /login and returns the state GitHub would send back and the cookie
// the browser keeps.
func (e *env) start(query string) (state string, cookie *http.Cookie, rec *httptest.ResponseRecorder) {
	e.t.Helper()
	rec = httptest.NewRecorder()
	e.svc.Login(rec, httptest.NewRequest("GET", "/v1/auth/github/login"+query, nil))
	if rec.Code != 302 {
		return "", nil, rec
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		e.t.Fatal(err)
	}
	state = loc.Query().Get("state")
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	return state, cookie, rec
}

func (e *env) callback(code, state string, cookie *http.Cookie) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest("GET", "/v1/auth/github/callback?"+url.Values{"code": {code}, "state": {state}}.Encode(), nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	e.svc.Callback(rec, req)
	return rec
}

func hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func TestLoginSendsTheBrowserToGitHubWithASignedStateAndACookie(t *testing.T) {
	e := newEnv(t, nil)
	state, cookie, rec := e.start("")
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Path != "/login/oauth/authorize" || loc.Query().Get("client_id") != "Iv1.client" ||
		loc.Query().Get("redirect_uri") != "https://ci.example.com/v1/auth/github/callback" || state == "" {
		t.Fatalf("location = %s", loc)
	}
	if cookie == nil || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/v1/auth/github" {
		t.Fatalf("cookie = %+v", cookie)
	}
	if strings.Contains(rec.Header().Get("Location"), "client-secret") {
		t.Fatal("the client secret is in the redirect")
	}
	if e.svc.CallbackURL() != "https://ci.example.com/v1/auth/github/callback" {
		t.Errorf("callback = %s", e.svc.CallbackURL())
	}
}

func TestSigningInGivesASessionLimitedToWhatTheUserCanReachAndDropsGitHubsToken(t *testing.T) {
	e := newEnv(t, nil)
	state, cookie, _ := e.start("")
	rec := e.callback("code-alice", state, cookie)
	if rec.Code != 200 {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body)
	}
	var s Session
	json.Unmarshal(rec.Body.Bytes(), &s)
	if s.User != "alice" || s.Role != "read" || !strings.HasPrefix(s.Token, "rcs_") || !s.ExpiresAt.Equal(e.now.Add(12*time.Hour)) ||
		!slices.Equal(s.Repos, []string{"acme/api", "acme/web", "solo/tool"}) {
		t.Fatalf("session = %+v: repos of both installations, once each, lower case", s)
	}
	if strings.Contains(rec.Body.String(), "gho_alice") {
		t.Fatal("GitHub's token is in the answer")
	}
	// The session is a stored token that limits and expires.
	got, err := e.st.LookupToken(context.Background(), hash(s.Token))
	if err != nil || got.User != "alice" || got.Role != "read" || !slices.Equal(got.Repos, s.Repos) || !got.ExpiresAt.Equal(s.ExpiresAt) {
		t.Fatalf("stored = %+v, %v", got, err)
	}
	e.now = e.now.Add(13 * time.Hour)
	e.st.Now = func() time.Time { return e.now }
	if _, err := e.st.LookupToken(context.Background(), hash(s.Token)); err == nil {
		t.Error("a session works after it expired")
	}
	// GitHub's token was revoked with the client's own credentials.
	if !slices.Equal(e.gh.revoked, []string{"gho_alice"}) || e.gh.basic != "Iv1.client:client-secret" {
		t.Errorf("revoked %v with %q", e.gh.revoked, e.gh.basic)
	}
	if cleared := rec.Result().Cookies(); len(cleared) == 0 || cleared[0].MaxAge >= 0 {
		t.Errorf("the state cookie was not cleared: %+v", cleared)
	}
}

func TestOnlyTheListedLoginsAreAdministrators(t *testing.T) {
	e := newEnv(t, nil)
	state, cookie, _ := e.start("")
	rec := e.callback("code-admin", state, cookie)
	var s Session
	json.Unmarshal(rec.Body.Bytes(), &s)
	if rec.Code != 200 || s.Role != "admin" || s.User != "boss" || len(s.Repos) != 0 {
		t.Fatalf("admin = %d %+v", rec.Code, s)
	}
	got, _ := e.st.LookupToken(context.Background(), hash(s.Token))
	if got.Role != "admin" || len(got.Repos) != 0 {
		t.Fatalf("stored = %+v", got)
	}
	// Bob reaches the same repositories and is not one.
	state, cookie, _ = e.start("")
	rec = e.callback("code-bob", state, cookie)
	json.Unmarshal(rec.Body.Bytes(), &s)
	if s.Role != "read" {
		t.Fatalf("bob = %+v", s)
	}
}

func TestSomeoneWhoReachesNothingGetsNoSession(t *testing.T) {
	e := newEnv(t, nil)
	state, cookie, _ := e.start("")
	rec := e.callback("code-nobody", state, cookie)
	if rec.Code != 403 {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body)
	}
	if all, _ := e.st.ListTokens(context.Background()); len(all) != 0 {
		t.Fatalf("a session was made: %+v", all)
	}
	if len(e.gh.revoked) != 1 {
		t.Errorf("GitHub's token was not revoked after a refusal: %v", e.gh.revoked)
	}
}

func TestACallbackNotStartedByThisBrowserIsRefused(t *testing.T) {
	e := newEnv(t, nil)
	state, cookie, _ := e.start("")
	other, otherCookie, _ := e.start("")
	for name, c := range map[string]struct {
		state  string
		cookie *http.Cookie
	}{
		"no cookie (an attacker's link opened by a victim)": {state, nil},
		"another sign-in's cookie":                          {state, otherCookie},
		"a state nobody signed":                             {"AAAA.BBBB", cookie},
		"no state":                                          {"", cookie},
		"a state with its signature cut":                    {strings.Split(state, ".")[0], cookie},
	} {
		if rec := e.callback("code-alice", c.state, c.cookie); rec.Code != 400 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	_ = other
	if all, _ := e.st.ListTokens(context.Background()); len(all) != 0 || len(e.gh.revoked) != 0 {
		t.Fatalf("a refused callback still did something: %+v %v", all, e.gh.revoked)
	}
	// An expired state.
	e.now = e.now.Add(11 * time.Minute)
	if rec := e.callback("code-alice", state, cookie); rec.Code != 400 {
		t.Errorf("an expired state: %d", rec.Code)
	}
}

func TestACodeWorksOnceAndGitHubsRefusalsAreNotEchoed(t *testing.T) {
	e := newEnv(t, nil)
	state, cookie, _ := e.start("")
	if rec := e.callback("code-alice", state, cookie); rec.Code != 200 {
		t.Fatalf("first: %d", rec.Code)
	}
	if rec := e.callback("code-alice", state, cookie); rec.Code != 400 || !strings.Contains(rec.Body.String(), "start the sign-in again") {
		t.Errorf("the same code again: %d %s", rec.Code, rec.Body)
	}
	if rec := e.callback("made-up", state, cookie); rec.Code != 400 {
		t.Errorf("a made-up code: %d", rec.Code)
	}
	// The user said no at GitHub.
	rec := httptest.NewRecorder()
	e.svc.Callback(rec, httptest.NewRequest("GET", "/v1/auth/github/callback?error=access_denied", nil))
	if rec.Code != 403 {
		t.Errorf("denied: %d", rec.Code)
	}
}

func TestARedirectIsOnlyFollowedWhenTheServerWasGivenItAndTheSessionGoesInTheFragment(t *testing.T) {
	e := newEnv(t, nil)
	for _, bad := range []string{"https://evil.example.com/", "https://app.example.com.evil.com/", "http://app.example.com/", "//evil.com", "https://app.example.com/#x"} {
		if _, _, rec := e.start("?redirect=" + url.QueryEscape(bad)); rec.Code != 400 {
			t.Errorf("redirect %q: %d, want 400", bad, rec.Code)
		}
	}
	state, cookie, rec := e.start("?redirect=" + url.QueryEscape("https://app.example.com/signed-in"))
	if rec.Code != 302 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	rec = e.callback("code-alice", state, cookie)
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if rec.Code != 302 || loc.Host != "app.example.com" || loc.Path != "/signed-in" || loc.RawQuery != "" {
		t.Fatalf("callback: %d %s: the session must not be in the query", rec.Code, rec.Header().Get("Location"))
	}
	frag, _ := url.ParseQuery(loc.Fragment)
	if !strings.HasPrefix(frag.Get("token"), "rcs_") || frag.Get("user") != "alice" || frag.Get("role") != "read" || frag.Get("expires_at") == "" {
		t.Fatalf("fragment = %v", frag)
	}
}

func TestTheConfigurationIsChecked(t *testing.T) {
	st, _ := store.OpenSQLite(filepath.Join(t.TempDir(), "s.db"))
	defer st.Close()
	for name, c := range map[string]Config{
		"no client id":     {ClientSecret: "s", PublicURL: "https://x", Sessions: st},
		"no secret":        {ClientID: "i", PublicURL: "https://x", Sessions: st},
		"no sessions":      {ClientID: "i", ClientSecret: "s", PublicURL: "https://x"},
		"an http address":  {ClientID: "i", ClientSecret: "s", PublicURL: "http://ci.example.com", Sessions: st},
		"an http redirect": {ClientID: "i", ClientSecret: "s", PublicURL: "https://x", Sessions: st, Redirects: []string{"http://app.example.com/"}},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("accepted %s", name)
		}
	}
}
