package oidc

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/server/store"
	"github.com/Raccoon-CI/raccoonci/internal/server/vault"
)

type env struct {
	t   *testing.T
	st  *store.SQLite
	v   *vault.Vault
	svc *Service
	srv *httptest.Server
	now time.Time
	run store.Run
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	v, _ := vault.New(map[string][]byte{"1": bytes.Repeat([]byte{7}, 32)}, "1")
	e := &env{t: t, st: st, v: v, now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	ev, _, err := st.RecordEvent(context.Background(), store.Event{DeliveryID: "d", Kind: "push", Repo: "acme/web", Payload: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	e.run, _, err = st.CreateRun(context.Background(), store.Run{EventID: ev.ID, Repo: "acme/web", WorkflowPath: ".github/workflows/deploy.yml", WorkflowName: "Deploy",
		Event: "push", Ref: "refs/heads/main", SHA: strings.Repeat("a", 40), Actor: "alice", WorkflowText: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.ClaimRun(context.Background()); err != nil || !ok { // in progress
		t.Fatalf("claim: %v %v", ok, err)
	}
	e.svc = e.newService(v)
	mux := http.NewServeMux()
	e.svc.Mount(mux)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) newService(v *vault.Vault) *Service {
	e.t.Helper()
	svc, err := New(context.Background(), Config{Runs: e.st, Docs: e.st, Vault: v, PublicURL: "https://ci.example.com", Now: func() time.Time { return e.now }})
	if err != nil {
		e.t.Fatal(err)
	}
	return svc
}

func (e *env) get(path, token string) (int, map[string]any) {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// requestURL and token are what a job is given.
func (e *env) job(run int64) (path, token string) {
	envs := e.svc.Env(run, "build")
	u, err := url.Parse(envs["ACTIONS_ID_TOKEN_REQUEST_URL"])
	if err != nil || u.Path != "/oidc/token" || u.Query().Get("api-version") != "2.0" {
		e.t.Fatalf("request url = %q", envs["ACTIONS_ID_TOKEN_REQUEST_URL"])
	}
	return "/oidc/token?" + u.RawQuery, envs["ACTIONS_ID_TOKEN_REQUEST_TOKEN"]
}

// verify checks a JWT against the published key set, independently of the service.
func (e *env) verify(jwt string) (header, claims map[string]any) {
	e.t.Helper()
	_, set := e.get("/oidc/jwks", "")
	keys := set["keys"].([]any)
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		e.t.Fatalf("not a JWT: %q", jwt)
	}
	dec := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			e.t.Fatal(err)
		}
		return b
	}
	json.Unmarshal(dec(parts[0]), &header)
	json.Unmarshal(dec(parts[1]), &claims)
	for _, k := range keys {
		jwk := k.(map[string]any)
		if jwk["kid"] != header["kid"] {
			continue
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(dec(jwk["n"].(string))), E: int(new(big.Int).SetBytes(dec(jwk["e"].(string))).Int64())}
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], dec(parts[2])); err != nil {
			e.t.Fatalf("the signature does not verify against the published key: %v", err)
		}
		return header, claims
	}
	e.t.Fatalf("no key %v in the key set", header["kid"])
	return nil, nil
}

func TestADiscoveryDocumentAndAKeySetAnyVerifierCanUse(t *testing.T) {
	e := newEnv(t)
	code, doc := e.get("/oidc/.well-known/openid-configuration", "")
	if code != 200 || doc["issuer"] != "https://ci.example.com/oidc" || doc["jwks_uri"] != "https://ci.example.com/oidc/jwks" {
		t.Fatalf("discovery = %d %v", code, doc)
	}
	algs := doc["id_token_signing_alg_values_supported"].([]any)
	if len(algs) != 1 || algs[0] != "RS256" {
		t.Fatalf("algs = %v", algs)
	}
}

func TestAJobGetsATokenWithGitHubsClaimsSignedByThePublishedKey(t *testing.T) {
	e := newEnv(t)
	path, tok := e.job(e.run.ID)
	code, out := e.get(path+"&audience=sts.amazonaws.com", tok)
	if code != 200 {
		t.Fatalf("token = %d %v", code, out)
	}
	h, c := e.verify(out["value"].(string))
	if h["alg"] != "RS256" || h["typ"] != "JWT" {
		t.Errorf("header = %v", h)
	}
	for k, want := range map[string]any{
		"iss": "https://ci.example.com/oidc", "aud": "sts.amazonaws.com",
		"sub": "repo:acme/web:ref:refs/heads/main", "repository": "acme/web", "repository_owner": "acme",
		"ref": "refs/heads/main", "ref_type": "branch", "sha": strings.Repeat("a", 40), "actor": "alice",
		"workflow": "Deploy", "event_name": "push", "run_id": "1", "run_number": "1", "run_attempt": "1",
		"job_workflow_ref": "acme/web/.github/workflows/deploy.yml@refs/heads/main", "runner_environment": "self-hosted",
	} {
		if c[k] != want {
			t.Errorf("claim %s = %v, want %v", k, c[k], want)
		}
	}
	if exp, iat := c["exp"].(float64), c["iat"].(float64); exp-iat != 600 || int64(iat) != e.now.Unix() {
		t.Errorf("iat %v exp %v", iat, exp)
	}
	if c["jti"] == "" || c["jti"] == nil {
		t.Error("no jti")
	}
	// Two tokens never share a jti.
	_, out2 := e.get(path, tok)
	_, c2 := e.verify(out2["value"].(string))
	if c2["jti"] == c["jti"] {
		t.Error("the jti repeats")
	}
	if c2["aud"] != "https://github.com/acme" {
		t.Errorf("the default audience = %v, want GitHub's", c2["aud"])
	}
}

func TestSubjectForms(t *testing.T) {
	for _, c := range []struct {
		run  store.Run
		want string
	}{
		{store.Run{Repo: "o/r", Event: "push", Ref: "refs/heads/main"}, "repo:o/r:ref:refs/heads/main"},
		{store.Run{Repo: "o/r", Event: "push", Ref: "refs/tags/v1"}, "repo:o/r:ref:refs/tags/v1"},
		{store.Run{Repo: "o/r", Event: "workflow_dispatch", Ref: "refs/heads/dev"}, "repo:o/r:ref:refs/heads/dev"},
		{store.Run{Repo: "o/r", Event: "pull_request", Ref: "refs/pull/3/merge"}, "repo:o/r:pull_request"},
	} {
		if got := Subject(c.run); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.run.Event, c.run.Ref, got, c.want)
		}
	}
}

func TestOnlyAValidRequestTokenOfAnUnfinishedRunGetsAToken(t *testing.T) {
	e := newEnv(t)
	path, tok := e.job(e.run.ID)
	if code, _ := e.get(path, ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code, _ := e.get(path, "garbage"); code != 401 {
		t.Errorf("garbage: %d", code)
	}
	// The runtime token of the artifact service is not a request token, and a
	// request token with one letter changed is not either.
	payload, sig, _ := strings.Cut(tok, ".")
	flipped := byte('A')
	if sig[len(sig)-1] == 'A' {
		flipped = 'B'
	}
	if code, _ := e.get(path, payload+"."+sig[:len(sig)-1]+string(flipped)); code != 401 {
		t.Errorf("a changed signature: %d", code)
	}
	if code, _ := e.get(path, "x."+sig); code != 401 {
		t.Errorf("a changed payload: %d", code)
	}
	// Another process (another key) cannot have made it.
	other := e.newService(e.v)
	_, foreign := func() (string, string) {
		m := other.Env(e.run.ID, "build")
		return "", m["ACTIONS_ID_TOKEN_REQUEST_TOKEN"]
	}()
	if code, _ := e.get(path, foreign); code != 401 {
		t.Errorf("a token of another process: %d", code)
	}
	// Expired.
	e.now = e.now.Add(requestTTL + time.Second)
	if code, _ := e.get(path, tok); code != 401 {
		t.Errorf("expired: %d", code)
	}
	e.now = e.now.Add(-requestTTL - time.Second)
	// A run that does not exist.
	_, gone := e.job(999)
	if code, _ := e.get(path, gone); code != 404 {
		t.Errorf("a missing run: %d", code)
	}
	// A run that is over.
	if err := e.st.FinishRun(context.Background(), e.run.ID, "success"); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.get(path, tok); code != 403 {
		t.Errorf("a finished run: %d, want 403", code)
	}
}

func TestAnAudienceThatIsTooLongOrMalformedIsRefused(t *testing.T) {
	e := newEnv(t)
	path, tok := e.job(e.run.ID)
	if code, _ := e.get(path+"&audience="+strings.Repeat("a", 300), tok); code != 400 {
		t.Errorf("a long audience: %d", code)
	}
	if code, _ := e.get(path+"&audience=a%0Ab", tok); code != 400 {
		t.Errorf("an audience with a newline: %d", code)
	}
}

func TestTheSigningKeyIsKeptSealedAndSurvivesARestartButNotAChangedMasterKey(t *testing.T) {
	e := newEnv(t)
	again := e.newService(e.v)
	if again.kid != e.svc.kid {
		t.Fatalf("a restart changed the key: %s -> %s", e.svc.kid, again.kid)
	}
	doc, err := e.st.GetDocument(context.Background(), documentName)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(doc.Data, []byte("PRIVATE")) || len(doc.Data) < 100 {
		t.Errorf("the stored key looks unsealed: %q", doc.Data[:min(60, len(doc.Data))])
	}
	// A new master key cannot open it: a new key is made, and kept.
	v2, _ := vault.New(map[string][]byte{"2": bytes.Repeat([]byte{9}, 32)}, "2")
	changed := e.newService(v2)
	if changed.kid == e.svc.kid {
		t.Fatal("a key that cannot be opened was used")
	}
	if again2 := e.newService(v2); again2.kid != changed.kid {
		t.Fatal("the replacement key was not kept")
	}
}

func TestWithoutAVaultTheKeyIsPerProcess(t *testing.T) {
	e := newEnv(t)
	a := e.newService(nil)
	b := e.newService(nil)
	if a.kid == b.kid {
		t.Fatal("two processes without a master key share a key")
	}
}

func TestAServerNeedsAnHTTPSAddressAndRuns(t *testing.T) {
	e := newEnv(t)
	for _, bad := range []string{"", "http://ci.example.com", "ci.example.com"} {
		if _, err := New(context.Background(), Config{Runs: e.st, PublicURL: bad}); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := New(context.Background(), Config{PublicURL: "https://x"}); err == nil {
		t.Error("accepted no runs")
	}
}
