package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Raccoon-CI/raccoonci/internal/server/secrets"
	"github.com/Raccoon-CI/raccoonci/internal/server/storage"
	"github.com/Raccoon-CI/raccoonci/internal/server/store"
	"github.com/Raccoon-CI/raccoonci/internal/server/vault"
	"github.com/Raccoon-CI/raccoonci/pkg/blob"
)

func secretsAPI(t *testing.T) (*API, *store.SQLite) {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	v, _ := vault.New(map[string][]byte{"1": bytes.Repeat([]byte{1}, 32)}, "1")
	return &API{Store: st, Token: token, Secrets: &secrets.Service{Store: st, Vault: v}}, st
}

func call(h http.Handler, method, path, bearer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSecretsCanBeWrittenAndListedButNeverReadBack(t *testing.T) {
	a, st := secretsAPI(t)
	h := a.Handler()
	if rec := call(h, "PUT", "/v1/secrets", token, `{"scope":"Repo:Acme/Web","name":"deploy_token","value":"hunter2"}`); rec.Code != 204 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	raw, err := st.GetSecret(context.Background(), "repo:acme/web", "DEPLOY_TOKEN")
	if err != nil || bytes.Contains(raw.Value, []byte("hunter2")) {
		t.Fatalf("stored = %+v, %v", raw, err)
	}
	rec := call(h, "GET", "/v1/secrets?scope=repo:acme/web", token, "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "hunter2") || !strings.Contains(rec.Body.String(), `"DEPLOY_TOKEN"`) || strings.Contains(rec.Body.String(), `"value"`) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "PUT", "/v1/vars", token, `{"scope":"org:acme","name":"region","value":"eu"}`); rec.Code != 204 {
		t.Fatalf("put var: %d %s", rec.Code, rec.Body)
	}
	rec = call(h, "GET", "/v1/vars?scope=org:acme", token, "")
	var vars struct {
		Vars []struct{ Name, Value string } `json:"vars"`
	}
	json.Unmarshal(rec.Body.Bytes(), &vars)
	if len(vars.Vars) != 1 || vars.Vars[0].Name != "REGION" || vars.Vars[0].Value != "eu" {
		t.Fatalf("vars: %s", rec.Body)
	}
	// A variable is not listed among the secrets and a secret is not among the variables.
	if rec := call(h, "GET", "/v1/secrets?scope=org:acme", token, ""); strings.Contains(rec.Body.String(), "REGION") {
		t.Fatalf("a variable listed as a secret: %s", rec.Body)
	}
	if rec := call(h, "DELETE", "/v1/vars?scope=repo:acme/web&name=DEPLOY_TOKEN", token, ""); rec.Code != 404 {
		t.Fatalf("a secret deleted as a variable: %d", rec.Code)
	}
	if rec := call(h, "DELETE", "/v1/secrets?scope=repo:acme/web&name=deploy_token", token, ""); rec.Code != 204 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "DELETE", "/v1/secrets?scope=repo:acme/web&name=deploy_token", token, ""); rec.Code != 404 {
		t.Fatalf("delete twice: %d", rec.Code)
	}
}

func TestBadSecretRequestsAreRefusedWithoutEchoingTheValue(t *testing.T) {
	a, _ := secretsAPI(t)
	h := a.Handler()
	for _, body := range []string{
		`not json`, `{"scope":"repo:acme/web","name":"A","value":"v","extra":1}`,
		`{"scope":"nonsense","name":"A","value":"topsecretvalue"}`, `{"scope":"repo:acme/web","name":"1bad","value":"topsecretvalue"}`,
		`{"scope":"repo:acme/web","name":"GITHUB_X","value":"topsecretvalue"}`, `{"scope":"repo:acme/web","name":"A","value":""}`,
	} {
		rec := call(h, "PUT", "/v1/secrets", token, body)
		if rec.Code != 400 || strings.Contains(rec.Body.String(), "topsecretvalue") {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
	a.Secrets = nil
	for _, p := range [][2]string{{"PUT", "/v1/secrets"}, {"GET", "/v1/secrets?scope=org:a"}, {"DELETE", "/v1/vars?scope=org:a&name=A"}} {
		if rec := call(a.Handler(), p[0], p[1], token, `{}`); rec.Code != 503 {
			t.Errorf("%v without a vault: %d", p, rec.Code)
		}
	}
}

func TestTheServersOwnBucketIsSetThroughTheAPIWithOrgStarAndNeverReadBack(t *testing.T) {
	a, st := secretsAPI(t)
	v, _ := vault.New(map[string][]byte{"1": bytes.Repeat([]byte{1}, 32)}, "1")
	a.Storage = &storage.Service{Store: st, Vault: v, Check: func(context.Context, blob.S3Config) ([]string, error) { return nil, nil }}
	h := a.Handler()
	body := `{"org":"*","bucket":"shared-ci","region":"auto","access_key":"AKIASERVER","secret_key":"server-secret-key-material"}`
	if rec := call(h, "PUT", "/v1/storage", token, body); rec.Code != 200 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	rec := call(h, "GET", "/v1/storage?org=*", token, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"configured":true`) || !strings.Contains(rec.Body.String(), "shared-ci") ||
		strings.Contains(rec.Body.String(), "server-secret") || strings.Contains(rec.Body.String(), "AKIASERVER") {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	for _, scope := range []string{"server", "org:server", "org:*"} {
		if rec := call(h, "GET", "/v1/secrets?scope="+scope, token, ""); strings.Contains(rec.Body.String(), "STORAGE") {
			t.Errorf("scope %s lists the bucket's settings: %s", scope, rec.Body)
		}
	}
	if rec := call(h, "DELETE", "/v1/storage?org=*", token, ""); rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := call(h, "GET", "/v1/storage?org=*", token, ""); !strings.Contains(rec.Body.String(), `"configured":false`) {
		t.Fatalf("after delete: %s", rec.Body)
	}
}

func TestAnOrganizationsBucketIsTriedKeptSealedAndNeverReadBack(t *testing.T) {
	a, st := secretsAPI(t)
	var tried []blob.S3Config
	var fail error
	v, _ := vault.New(map[string][]byte{"1": bytes.Repeat([]byte{1}, 32)}, "1")
	a.Storage = &storage.Service{Store: st, Vault: v, Check: func(_ context.Context, c blob.S3Config) ([]string, error) {
		tried = append(tried, c)
		return []string{"the bucket gives its objects to anyone"}, fail
	}}
	h := a.Handler()
	body := `{"org":"Acme","bucket":"acme-ci","endpoint":"https://x.r2.cloudflarestorage.com","region":"auto","access_key":"AKIAEXAMPLE","secret_key":"very-secret-key-material"}`
	rec := call(h, "PUT", "/v1/storage", token, body)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "anyone") || strings.Contains(rec.Body.String(), "very-secret") {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if len(tried) != 1 || tried[0].Bucket != "acme-ci" || tried[0].SecretKey != "very-secret-key-material" {
		t.Fatalf("tried %+v", tried)
	}
	rec = call(h, "GET", "/v1/storage?org=acme", token, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"configured":true`) || !strings.Contains(rec.Body.String(), "acme-ci") ||
		strings.Contains(rec.Body.String(), "very-secret") || strings.Contains(rec.Body.String(), "AKIAEXAMPLE") {
		t.Fatalf("get: %d %s: the keys must never be read back", rec.Code, rec.Body)
	}
	// It is not among the organization's secrets.
	if rec := call(h, "GET", "/v1/secrets?scope=org:acme", token, ""); strings.Contains(rec.Body.String(), "STORAGE") || strings.Contains(rec.Body.String(), "acme-ci") {
		t.Fatalf("the bucket's settings are listed as a secret: %s", rec.Body)
	}
	// A bucket that does not work is refused and not kept.
	fail = errors.New("writing an object: access denied")
	rec = call(h, "PUT", "/v1/storage", token, strings.ReplaceAll(body, "acme-ci", "broken-bucket"))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "access denied") || strings.Contains(rec.Body.String(), "very-secret") {
		t.Fatalf("a broken bucket: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "GET", "/v1/storage?org=acme", token, ""); !strings.Contains(rec.Body.String(), "acme-ci") {
		t.Fatalf("the earlier settings were lost: %s", rec.Body)
	}
	for _, b := range []string{`junk`, `{"org":"acme","bucket":"x","extra":1}`, `{"org":"acme","bucket":"","access_key":"a","secret_key":"b"}`} {
		if rec := call(h, "PUT", "/v1/storage", token, b); rec.Code != 400 {
			t.Errorf("%s: %d", b, rec.Code)
		}
	}
	if rec := call(h, "DELETE", "/v1/storage?org=acme", token, ""); rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := call(h, "DELETE", "/v1/storage?org=acme", token, ""); rec.Code != 404 {
		t.Fatalf("delete twice: %d", rec.Code)
	}
	if rec := call(h, "GET", "/v1/storage?org=acme", token, ""); !strings.Contains(rec.Body.String(), `"configured":false`) {
		t.Fatalf("after delete: %s", rec.Body)
	}
	// Only an admin touches it; without a vault it is off.
	read := call(h, "POST", "/v1/tokens", token, `{"name":"r","role":"read"}`)
	var made struct{ Token string }
	json.Unmarshal(read.Body.Bytes(), &made)
	if rec := call(h, "GET", "/v1/storage?org=acme", made.Token, ""); rec.Code != 403 {
		t.Fatalf("read token: %d", rec.Code)
	}
	a.Storage = nil
	if rec := call(a.Handler(), "GET", "/v1/storage?org=acme", token, ""); rec.Code != 503 {
		t.Fatalf("no vault: %d", rec.Code)
	}
}

func TestTokensMadeThroughTheAPIHaveRolesAndAreShownOnce(t *testing.T) {
	a, st := secretsAPI(t)
	h := a.Handler()
	rec := call(h, "POST", "/v1/tokens", token, `{"name":"dashboard","role":"read"}`)
	var made struct {
		ID    int64  `json:"id"`
		Role  string `json:"role"`
		Token string `json:"token"`
	}
	json.Unmarshal(rec.Body.Bytes(), &made)
	if rec.Code != 201 || !strings.HasPrefix(made.Token, "rct_") || made.Role != "read" {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	// Only a hash is kept, and the token is not shown again.
	if _, err := st.LookupToken(context.Background(), made.Token); err == nil {
		t.Fatal("the token itself is stored")
	}
	if rec := call(h, "GET", "/v1/tokens", token, ""); strings.Contains(rec.Body.String(), made.Token) || !strings.Contains(rec.Body.String(), "dashboard") {
		t.Fatalf("list: %s", rec.Body)
	}
	// A read token reads, and cannot change anything or touch secrets and tokens.
	if rec := call(h, "GET", "/v1/runs", made.Token, ""); rec.Code != 200 {
		t.Fatalf("read with a read token: %d", rec.Code)
	}
	for _, p := range [][3]string{
		{"POST", "/v1/runs", `{}`}, {"POST", "/v1/runs/1/cancel", ``}, {"POST", "/v1/runs/1/rerun", ``},
		{"GET", "/v1/secrets?scope=org:a", ``}, {"PUT", "/v1/secrets", `{}`}, {"GET", "/v1/vars?scope=org:a", ``},
		{"GET", "/v1/tokens", ``}, {"POST", "/v1/tokens", `{"name":"x","role":"admin"}`}, {"DELETE", "/v1/tokens/1", ``},
	} {
		if rec := call(h, p[0], p[1], made.Token, p[2]); rec.Code != 403 {
			t.Errorf("%s %s with a read token: %d, want 403", p[0], p[1], rec.Code)
		}
	}
	// An admin token made through the API works as the bootstrap one does.
	rec = call(h, "POST", "/v1/tokens", token, `{"name":"ops","role":"admin"}`)
	var ops struct{ Token string }
	json.Unmarshal(rec.Body.Bytes(), &ops)
	if rec := call(h, "PUT", "/v1/vars", ops.Token, `{"scope":"org:acme","name":"A","value":"b"}`); rec.Code != 204 {
		t.Fatalf("admin token: %d", rec.Code)
	}
	// Revoked, a token stops working at once.
	if rec := call(h, "DELETE", "/v1/tokens/"+itoa(made.ID), token, ""); rec.Code != 204 {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := call(h, "GET", "/v1/runs", made.Token, ""); rec.Code != 401 {
		t.Fatalf("a revoked token: %d, want 401", rec.Code)
	}
	if rec := call(h, "DELETE", "/v1/tokens/"+itoa(made.ID), token, ""); rec.Code != 404 {
		t.Fatalf("revoke twice: %d", rec.Code)
	}
	for _, body := range []string{`{"name":"","role":"read"}`, `{"name":"x","role":"root"}`, `{"name":"x"}`, `junk`} {
		if rec := call(h, "POST", "/v1/tokens", token, body); rec.Code != 400 {
			t.Errorf("%s: %d, want 400", body, rec.Code)
		}
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
