package caches

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/artifact"
	"github.com/Raccoon-CI/raccoonci/internal/blob"
)

func TestSpaceRules(t *testing.T) {
	main := SpaceFor("acme/web", "refs/heads/main", "main")
	if main.Own != RepoID("acme/web")+"/default" || len(main.Read) != 1 || main.Read[0] != main.Own {
		t.Fatalf("default branch = %+v", main)
	}
	feat := SpaceFor("acme/web", "refs/heads/feature", "main")
	if feat.Own == main.Own || len(feat.Read) != 2 || feat.Read[0] != feat.Own || feat.Read[1] != main.Own {
		t.Fatalf("feature branch = %+v: own space first, then the default branch's", feat)
	}
	for _, ref := range []string{"refs/tags/main", "refs/pull/1/merge", "refs/heads/main2", "refs/heads/Main"} {
		if got := SpaceFor("acme/web", ref, "main"); got.Own == main.Own {
			t.Errorf("%s shares the default branch's space", ref)
		}
	}
	if a, b := SpaceFor("acme/web", "refs/pull/1/merge", "main"), SpaceFor("acme/web", "refs/pull/2/merge", "main"); a.Own == b.Own {
		t.Error("two pull requests share a space")
	}
	if SpaceFor("acme/web", "refs/heads/x", "main").Own == SpaceFor("acme/api", "refs/heads/x", "main").Own {
		t.Error("two repositories share a space")
	}
	if SpaceFor("Acme/Web", "refs/heads/main", "main").Own != main.Own {
		t.Error("repository names are not case-sensitive")
	}
	// An unknown default branch costs hits, never safety: nothing is taken for the default.
	unknown := SpaceFor("acme/web", "refs/heads/main", "")
	if unknown.Own == main.Own || unknown.Read[len(unknown.Read)-1] != main.Own {
		t.Errorf("unknown default branch = %+v", unknown)
	}
	for _, id := range []string{main.Own, feat.Own, RepoID("a/b") + "/default"} {
		if !validSpace(id) {
			t.Errorf("%q is not valid", id)
		}
	}
	for _, id := range []string{"", "../x", "abc/default", RepoID("a/b") + "/../default", RepoID("a/b") + "/r-xyz", RepoID("a/b") + "/other"} {
		if validSpace(id) {
			t.Errorf("%q is valid", id)
		}
	}
}

type harness struct {
	t      *testing.T
	dir    string
	svc    *Service
	srv    *httptest.Server
	signer *artifact.Signer
	mu     sync.Mutex
	spaces map[int64]Space
	mux    *http.ServeMux
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, dir: t.TempDir(), spaces: map[int64]Space{}}
	signer, err := artifact.NewSigner(bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatal(err)
	}
	h.signer = signer
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }))
	t.Cleanup(h.srv.Close)
	h.start()
	return h
}

// start (re)makes the service on the same directory.
func (h *harness) start() {
	h.t.Helper()
	svc, err := New(Config{Dir: h.dir, PublicURL: h.srv.URL, Signer: h.signer, MaxBytes: 1 << 20,
		Spaces: func(_ context.Context, run int64) (Space, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if sp, ok := h.spaces[run]; ok {
				return sp, nil
			}
			return Space{}, errors.New("no such run")
		}})
	if err != nil {
		h.t.Fatal(err)
	}
	h.svc = svc
	h.mux = http.NewServeMux()
	svc.Mount(h.mux)
}

func (h *harness) run(id int64, repo, ref string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.spaces[id] = SpaceFor(repo, ref, "main")
}

func (h *harness) token(run int64) string {
	tok, err := h.signer.Token(artifact.Scope{Run: fmt.Sprint(run), Job: "j"}, time.Hour)
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

func (h *harness) do(method, path, token string, hdr map[string]string, body io.Reader) (int, []byte) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+apiPrefix+path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// save stores data under key as the run: reserve, upload, commit.
func (h *harness) save(run int64, key, data string) int {
	h.t.Helper()
	tok := h.token(run)
	b, _ := json.Marshal(map[string]any{"key": key, "version": "v1"})
	code, body := h.do("POST", "caches", tok, map[string]string{"Content-Type": "application/json"}, bytes.NewReader(b))
	if code != 201 {
		return code
	}
	var res struct {
		CacheID int64 `json:"cacheId"`
	}
	json.Unmarshal(body, &res)
	if code, _ := h.do("PATCH", fmt.Sprintf("caches/%d", res.CacheID), tok,
		map[string]string{"Content-Type": "application/octet-stream", "Content-Range": fmt.Sprintf("bytes 0-%d/*", len(data)-1)},
		io.NopCloser(strings.NewReader(data))); code != 204 {
		h.t.Fatalf("patch = %d", code)
	}
	cb, _ := json.Marshal(map[string]int{"size": len(data)})
	if code, body := h.do("POST", fmt.Sprintf("caches/%d", res.CacheID), tok, map[string]string{"Content-Type": "application/json"}, bytes.NewReader(cb)); code != 204 {
		h.t.Fatalf("commit = %d %s", code, body)
	}
	return 201
}

// restore returns what the run finds for key, "" for nothing.
func (h *harness) restore(run int64, key string) string {
	h.t.Helper()
	code, body := h.do("GET", "cache?keys="+key+"&version=v1", h.token(run), nil, nil)
	if code == 204 {
		return ""
	}
	if code != 200 {
		h.t.Fatalf("lookup = %d %s", code, body)
	}
	var hit struct {
		ArchiveLocation string `json:"archiveLocation"`
	}
	json.Unmarshal(body, &hit)
	if !strings.HasPrefix(hit.ArchiveLocation, h.srv.URL+dlPrefix) {
		h.t.Fatalf("archive location %q", hit.ArchiveLocation)
	}
	resp, err := http.Get(hit.ArchiveLocation)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		h.t.Fatalf("download = %d", resp.StatusCode)
	}
	return string(data)
}

func TestRunsShareOnlyWhatTheDefaultBranchSavedAndSaveOnlyForThemselves(t *testing.T) {
	h := newHarness(t)
	h.run(1, "acme/web", "refs/heads/main")
	h.run(5, "acme/web", "refs/heads/main")
	h.run(2, "acme/web", "refs/heads/feature-a")
	h.run(3, "acme/web", "refs/heads/feature-b")
	h.run(4, "acme/web", "refs/pull/9/merge") // a pull request from a fork looks the same from here
	h.run(6, "acme/api", "refs/heads/main")

	if h.save(1, "deps", "main's deps") != 201 {
		t.Fatal("the default branch could not save")
	}
	for _, run := range []int64{1, 5, 2, 3, 4} {
		if got := h.restore(run, "deps"); got != "main's deps" {
			t.Errorf("run %d restored %q, want what the default branch saved", run, got)
		}
	}
	if got := h.restore(6, "deps"); got != "" {
		t.Errorf("another repository restored %q", got)
	}

	// A feature branch saves for itself only.
	h.save(2, "only-a", "from a")
	if got := h.restore(2, "only-a"); got != "from a" {
		t.Errorf("a does not see its own cache: %q", got)
	}
	for _, run := range []int64{1, 5, 3, 4, 6} {
		if got := h.restore(run, "only-a"); got != "" {
			t.Errorf("run %d restored %q from feature-a", run, got)
		}
	}

	// A pull request cannot overwrite the default branch's cache: saving the
	// same key lands in its own space, and the default branch keeps its own.
	if h.save(4, "deps", "POISON") != 201 {
		t.Fatal("the pull request could not save into its own space")
	}
	if got := h.restore(4, "deps"); got != "POISON" {
		t.Errorf("the pull request restored %q, its own entry comes first", got)
	}
	for _, run := range []int64{1, 5, 2, 3} {
		if got := h.restore(run, "deps"); got != "main's deps" {
			t.Errorf("run %d restored %q after the pull request saved: the default branch's cache was changed", run, got)
		}
	}
	// The same key by two spaces of one run's chain is not a conflict in the default space either.
	if code := h.save(5, "deps", "again"); code == 201 {
		t.Error("the default branch saved the same key and version twice: entries are immutable")
	}
}

func TestSpacesAreKeptOnDiskAndSurviveARestart(t *testing.T) {
	h := newHarness(t)
	h.run(1, "acme/web", "refs/heads/main")
	h.run(2, "acme/web", "refs/heads/feature")
	h.save(1, "deps", "kept")
	h.save(2, "mine", "feature's")
	h.svc.Close()
	h.start()
	if got := h.restore(2, "deps"); got != "kept" {
		t.Errorf("after a restart the feature branch restored %q from the default branch", got)
	}
	if got := h.restore(2, "mine"); got != "feature's" {
		t.Errorf("after a restart the feature branch restored %q of its own", got)
	}
	if got := h.restore(1, "mine"); got != "" {
		t.Errorf("after a restart the default branch restored %q from a feature branch", got)
	}
}

func TestTheOrganizationsBucketIsAskedForAndAFailureMeansNoCache(t *testing.T) {
	h := newHarness(t)
	h.run(1, "Acme/Web", "refs/heads/main")
	var orgs []string
	version, fail := "v1", error(nil)
	h.svc.cfg.S3For = func(_ context.Context, org string) (*blob.S3Config, string, error) {
		orgs = append(orgs, org)
		return nil, version, fail // no bucket: the objects stay on disk, but the version is followed
	}
	if h.save(1, "deps", "kept") != 201 || h.restore(1, "deps") != "kept" {
		t.Fatal("a save and a restore with a bucket callback")
	}
	for _, o := range orgs {
		if o != "acme" {
			t.Fatalf("asked for the bucket of %q, want the repository's owner, lower-cased", o)
		}
	}
	first := h.svc.peek(SpaceFor("Acme/Web", "refs/heads/main", "main").Own)
	version = "v2" // the organization changed its bucket
	if h.restore(1, "deps") != "kept" {
		t.Fatal("the entry is gone after the version changed on the same disk")
	}
	if now := h.svc.peek(SpaceFor("Acme/Web", "refs/heads/main", "main").Own); now == first || now.version != "v2" {
		t.Fatal("the space was not made again for the new version")
	}
	fail = errors.New("the settings cannot be opened")
	if code, _ := h.do("GET", "cache?keys=deps&version=v1", h.token(1), nil, nil); code != 500 {
		t.Fatalf("a bucket that cannot be had: %d, want 500 (and never a quiet fall back to the server's disk)", code)
	}
}

func TestRefusals(t *testing.T) {
	h := newHarness(t)
	h.run(1, "acme/web", "refs/heads/main")
	if code, _ := h.do("GET", "cache?keys=k&version=v1", "", nil, nil); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code, _ := h.do("GET", "cache?keys=k&version=v1", "garbage", nil, nil); code != 401 {
		t.Errorf("garbage token: %d", code)
	}
	if code, _ := h.do("GET", "cache?keys=k&version=v1", h.token(99), nil, nil); code != 403 {
		t.Errorf("a token of an unknown run: %d, want 403", code)
	}
	other, _ := artifact.NewSigner(bytes.Repeat([]byte("z"), 32))
	forged, _ := other.Token(artifact.Scope{Run: "1", Job: "j"}, time.Hour)
	if code, _ := h.do("GET", "cache?keys=k&version=v1", forged, nil, nil); code != 401 {
		t.Errorf("a token signed with another key: %d", code)
	}
	for _, p := range []string{"/_cache_dl/", "/_cache_dl/x", "/_cache_dl/../x/y", "/_cache_dl/" + RepoID("a/b") + "/default/obj"} {
		resp, err := http.Get(h.srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("GET %s = %d, want 404", p, resp.StatusCode)
		}
	}
}

func TestEnvAndExtend(t *testing.T) {
	h := newHarness(t)
	env := h.svc.Env()
	if env["ACTIONS_CACHE_URL"] != h.srv.URL+"/" || env["ACTIONS_CACHE_SERVICE_V2"] != "" {
		t.Fatalf("env = %v", env)
	}
	got, err := h.svc.Extend(fakeEnv{}).JobEnv(7, "build")
	if err != nil || got["ACTIONS_RUNTIME_TOKEN"] != "tok-7-build" || got["ACTIONS_CACHE_URL"] != h.srv.URL+"/" {
		t.Fatalf("env = %v, %v", got, err)
	}
	if _, err := h.svc.Extend(failingEnv{}).JobEnv(7, "x"); err == nil {
		t.Fatal("an error of the inner service was lost")
	}
}

type fakeEnv struct{}

func (fakeEnv) JobEnv(run int64, job string) (map[string]string, error) {
	return map[string]string{"ACTIONS_RUNTIME_TOKEN": fmt.Sprintf("tok-%d-%s", run, job)}, nil
}

type failingEnv struct{}

func (failingEnv) JobEnv(int64, string) (map[string]string, error) { return nil, errors.New("no") }
