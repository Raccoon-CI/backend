package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// verifyJWT checks the signature with the public key and returns the claims.
func verifyJWT(t *testing.T, tok string, pub *rsa.PublicKey) map[string]any {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %d parts", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("bad signature: %v", err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	json.Unmarshal(raw, &claims)
	return claims
}

func TestAppTradesItsJWTForAnInstallationTokenAndKeepsIt(t *testing.T) {
	key := newKey(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/app/installations/42/access_tokens" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		claims := verifyJWT(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), &key.PublicKey)
		if claims["iss"] != float64(777) {
			t.Errorf("iss = %v, want the App id", claims["iss"])
		}
		exp, iat := int64(claims["exp"].(float64)), int64(claims["iat"].(float64))
		if exp-now.Unix() > 10*60 || iat > now.Unix() {
			t.Errorf("exp/iat out of GitHub's limits: iat %d exp %d now %d", iat, exp, now.Unix())
		}
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"token":"ghs_%d","expires_at":%q}`, calls.Load(), now.Add(time.Hour).Format(time.RFC3339))
	}))
	defer srv.Close()
	app := &App{ID: 777, Key: key, Base: srv.URL, Now: func() time.Time { return now }}

	tok1, err := app.Token(context.Background(), 42)
	if err != nil || tok1 != "ghs_1" {
		t.Fatalf("token = %q, %v", tok1, err)
	}
	if tok2, _ := app.Token(context.Background(), 42); tok2 != tok1 || calls.Load() != 1 {
		t.Fatalf("a token still valid must be reused: %q after %d calls", tok2, calls.Load())
	}
	now = now.Add(61 * time.Minute) // expired
	if tok3, _ := app.Token(context.Background(), 42); tok3 != "ghs_2" {
		t.Fatalf("an expired token must be renewed, got %q", tok3)
	}
}

func TestAppRefusesADeliveryWithoutInstallationAndSurfacesGitHubsReason(t *testing.T) {
	key := newKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"message":"A JSON web token could not be decoded"}`)
	}))
	defer srv.Close()
	app := &App{ID: 1, Key: key, Base: srv.URL}
	if _, err := app.Token(context.Background(), 0); err == nil {
		t.Fatal("no installation must be an error")
	}
	_, err := app.Token(context.Background(), 5)
	if err == nil || !strings.Contains(err.Error(), "could not be decoded") {
		t.Fatalf("error = %v, want GitHub's message in it", err)
	}
}

func TestAppFindsItsInstallationOnARepository(t *testing.T) {
	key := newKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifyJWT(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), &key.PublicKey) // the App's own JWT, not an installation token
		switch r.URL.Path {
		case "/repos/acme/web/installation":
			fmt.Fprint(w, `{"id":4242,"account":{"login":"acme"}}`)
		case "/repos/acme/broken/installation":
			w.WriteHeader(500)
			fmt.Fprint(w, `{"message":"boom"}`)
		default:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		}
	}))
	defer srv.Close()
	app := &App{ID: 1, Key: key, Base: srv.URL}
	if id, err := app.InstallationOf(context.Background(), "acme/web"); err != nil || id != 4242 {
		t.Fatalf("id = %d, %v", id, err)
	}
	if _, err := app.InstallationOf(context.Background(), "acme/none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("not installed: err = %v, want ErrNotFound", err)
	}
	if _, err := app.InstallationOf(context.Background(), "acme/broken"); err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("a server error must not look like 'not installed': %v", err)
	}
}

func TestParsePrivateKeyTakesBothPEMForms(t *testing.T) {
	key := newKey(t)
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	der8, _ := x509.MarshalPKCS8PrivateKey(key)
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der8})
	for name, in := range map[string][]byte{"pkcs1": pkcs1, "pkcs8": pkcs8} {
		got, err := ParsePrivateKey(in)
		if err != nil || !got.Equal(key) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ParsePrivateKey([]byte("not pem")); err == nil {
		t.Error("garbage was accepted")
	}
}

func contentsServer(t *testing.T) *httptest.Server {
	t.Helper()
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/repos/o/r/contents/.github/workflows":
			if r.URL.Query().Get("ref") != "abc" {
				t.Errorf("ref = %q", r.URL.Query().Get("ref"))
			}
			fmt.Fprint(w, `[{"name":"ci.yml","path":".github/workflows/ci.yml","type":"file","size":10},
				{"name":"README.md","path":".github/workflows/README.md","type":"file","size":3},
				{"name":"sub","path":".github/workflows/sub","type":"dir","size":0},
				{"name":"lint.yaml","path":".github/workflows/lint.yaml","type":"file","size":10}]`)
		case "/repos/o/r/contents/.github/workflows/ci.yml":
			fmt.Fprintf(w, `{"encoding":"base64","content":%q}`, enc("on: push\n")[:6]+"\n"+enc("on: push\n")[6:])
		case "/repos/o/r/contents/.github/workflows/lint.yaml":
			fmt.Fprintf(w, `{"encoding":"base64","content":%q}`, enc("on: pull_request\n"))
		case "/repos/o/empty/contents/.github/workflows":
			w.WriteHeader(404)
		case "/repos/o/r/compare/aaa...bbb":
			fmt.Fprint(w, `{"files":[{"filename":"a.go"},{"filename":"dir/b.go"}]}`)
		case "/repos/o/r/compare/zero...bbb":
			w.WriteHeader(404)
		default:
			t.Errorf("unexpected %s", r.URL.RequestURI())
			w.WriteHeader(500)
		}
	}))
}

func TestWorkflowFilesReadsOnlyTheYAMLFilesOfTheDirectory(t *testing.T) {
	srv := contentsServer(t)
	defer srv.Close()
	c := &Client{Base: srv.URL, Tokens: StaticToken("tok")}
	files, err := c.WorkflowFiles(context.Background(), 0, "o/r", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != ".github/workflows/ci.yml" || string(files[0].Content) != "on: push\n" ||
		files[1].Path != ".github/workflows/lint.yaml" || string(files[1].Content) != "on: pull_request\n" {
		t.Fatalf("files = %+v", files)
	}
	none, err := c.WorkflowFiles(context.Background(), 0, "o/empty", "abc")
	if err != nil || len(none) != 0 {
		t.Fatalf("a repository without the directory: %v, %v", none, err)
	}
}

func TestSetStatusPostsTheStateForTheCommit(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/repos/o/r/statuses/abc" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("request %s %s auth %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":1}`)
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, Tokens: StaticToken("tok")}
	long := strings.Repeat("x", 300)
	if err := c.SetStatus(context.Background(), 0, "o/r", "abc", StateSuccess, StatusContext("CI"), long, "https://ci/run/1"); err != nil {
		t.Fatal(err)
	}
	if got["state"] != "success" || got["context"] != "raccoonci / CI" || got["target_url"] != "https://ci/run/1" || len(got["description"]) > 140 {
		t.Fatalf("body = %v", got)
	}
	if StatusContext("  ") != "raccoonci / workflow" {
		t.Fatal("an unnamed workflow needs a context")
	}
	notFound := httptest.NewServer(http.NotFoundHandler())
	defer notFound.Close()
	c = &Client{Base: notFound.URL, Tokens: StaticToken("tok")}
	if err := c.SetStatus(context.Background(), 0, "o/r", "abc", StatePending, "x", "", ""); err == nil || !strings.Contains(err.Error(), "cannot write statuses") {
		t.Fatalf("err = %v", err)
	}
}

func TestChangedFilesAndUnknown(t *testing.T) {
	srv := contentsServer(t)
	defer srv.Close()
	c := &Client{Base: srv.URL, Tokens: StaticToken("tok")}
	got, err := c.ChangedFiles(context.Background(), 0, "o/r", "aaa", "bbb")
	if err != nil || len(got) != 2 || got[1] != "dir/b.go" {
		t.Fatalf("changed = %v, %v", got, err)
	}
	unknown, err := c.ChangedFiles(context.Background(), 0, "o/r", "zero", "bbb")
	if err != nil || unknown != nil {
		t.Fatalf("an uncomparable base must be unknown (nil), got %v, %v", unknown, err)
	}
}
