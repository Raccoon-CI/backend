package intake

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"raccoonci/internal/server/github"
	"raccoonci/internal/server/store"
)

const secret = "s3cret-for-tests"

// fakeSource stands in for GitHub: it holds the workflow files of every
// commit and the list of changed files, and counts how it was asked.
type fakeSource struct {
	mu           sync.Mutex
	files        []github.File
	changed      []string
	filesErr     error
	changedCalls int
	readAt       []string
	installation int64
	refs         map[string]string // ResolveRef
}

func (f *fakeSource) WorkflowFiles(_ context.Context, inst int64, repo, ref string) ([]github.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readAt = append(f.readAt, ref)
	f.installation = inst
	return f.files, f.filesErr
}

func (f *fakeSource) ResolveRef(_ context.Context, _ int64, _, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sha, ok := f.refs[ref]; ok {
		return sha, nil
	}
	return "", github.ErrNotFound
}

func (f *fakeSource) FileAt(_ context.Context, _ int64, _, filePath, ref string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, file := range f.files {
		if file.Path == filePath {
			f.readAt = append(f.readAt, "file@"+ref)
			return file.Content, nil
		}
	}
	return nil, github.ErrNotFound
}

func (f *fakeSource) ChangedFiles(_ context.Context, _ int64, _, _, _ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changedCalls++
	return f.changed, nil
}

func wf(path, on string) github.File {
	return github.File{Path: ".github/workflows/" + path, Content: []byte("on: " + on + "\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n")}
}

type env struct {
	h   *Handler
	st  *store.SQLite
	src *fakeSource
}

func newEnv(t *testing.T, files ...github.File) *env {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	src := &fakeSource{files: files}
	return &env{h: &Handler{Secret: []byte(secret), Store: st, Source: src, Allow: []string{"acme/*"}, Inline: true}, st: st, src: src}
}

// fixture loads a recorded delivery and applies edits to it.
func fixture(t *testing.T, name string, edit func(m map[string]any)) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if edit == nil {
		return raw
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	out, _ := json.Marshal(m)
	return out
}

// sign is computed here from the definition (HMAC-SHA256, hex, "sha256="
// prefix), not with Sign, so that a mistake in Sign cannot hide itself.
func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestSignAgreesWithTheDefinition(t *testing.T) {
	body := []byte(`{"a":1}`)
	if got := Sign([]byte(secret), body); got != sign(body) {
		t.Fatalf("Sign = %q, want %q", got, sign(body))
	}
}

func (e *env) deliver(kind, delivery string, body []byte) *httptest.ResponseRecorder {
	return e.deliverSigned(kind, delivery, body, sign(body))
}

func (e *env) deliverSigned(kind, delivery string, body []byte, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", kind)
	req.Header.Set("X-GitHub-Delivery", delivery)
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	e.h.Wait()
	return rec
}

func (e *env) runs(t *testing.T) []store.Run {
	t.Helper()
	runs, err := e.st.ListRuns(context.Background(), store.RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func (e *env) event(t *testing.T, id int64) store.Event {
	t.Helper()
	ev, err := e.st.GetEvent(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestPushStartsTheWorkflowsItTriggers(t *testing.T) {
	e := newEnv(t,
		wf("ci.yml", "{push: {branches: [main]}}"),
		wf("docs.yml", "{push: {paths: ['docs/**']}}"),
		wf("review.yml", "pull_request"),
	)
	e.src.changed = []string{"cmd/x.go"}
	rec := e.deliver("push", "d-1", fixture(t, "push.json", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	runs := e.runs(t)
	if len(runs) != 1 {
		t.Fatalf("%d runs, want 1 (ci.yml)", len(runs))
	}
	r := runs[0]
	if r.WorkflowPath != ".github/workflows/ci.yml" || r.Event != "push" || r.Ref != "refs/heads/main" ||
		r.SHA != strings.Repeat("2", 40) || r.BaseSHA != strings.Repeat("1", 40) || r.Actor != "alice" ||
		r.Status != store.RunQueued || r.Fork || r.PullRequest != 0 {
		t.Fatalf("run = %+v", r)
	}
	full, _ := e.st.GetRun(context.Background(), r.ID)
	if !strings.Contains(full.WorkflowText, "echo hi") {
		t.Fatal("the run must keep the workflow text it was started from")
	}
	if got := e.src.readAt; len(got) != 1 || got[0] != strings.Repeat("2", 40) {
		t.Fatalf("workflows were read at %v, want the pushed commit", got)
	}
	if e.src.installation != 42 {
		t.Fatalf("GitHub was called for installation %d, want 42", e.src.installation)
	}
	ev := e.event(t, 1)
	if ev.Status != store.EventHandled {
		t.Fatalf("event status %q", ev.Status)
	}
	var notes []outcome
	if err := json.Unmarshal([]byte(ev.Note), &notes); err != nil || len(notes) != 3 {
		t.Fatalf("note %q: %v", ev.Note, err)
	}
	if notes[0].Result != "run" || notes[0].Run != r.ID ||
		notes[1].Result != "skipped" || !strings.Contains(notes[1].Reason, "paths filter") ||
		notes[2].Result != "skipped" || !strings.Contains(notes[2].Reason, "does not listen") {
		t.Fatalf("outcomes = %+v", notes)
	}
}

func TestARedeliveryStartsNothingTwice(t *testing.T) {
	e := newEnv(t, wf("ci.yml", "push"))
	body := fixture(t, "push.json", nil)
	if rec := e.deliver("push", "same", body); rec.Code != http.StatusAccepted {
		t.Fatalf("first: %d", rec.Code)
	}
	rec := e.deliver("push", "same", body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "duplicate") {
		t.Fatalf("redelivery: %d %s", rec.Code, rec.Body)
	}
	if n := len(e.runs(t)); n != 1 {
		t.Fatalf("%d runs after a redelivery, want 1", n)
	}
	if len(e.src.readAt) != 1 {
		t.Fatalf("GitHub was asked %d times, a duplicate must not be handled again", len(e.src.readAt))
	}
}

func TestADeliveryWithoutAValidSignatureIsRefusedAndForgotten(t *testing.T) {
	e := newEnv(t, wf("ci.yml", "push"))
	body := fixture(t, "push.json", nil)
	for name, sig := range map[string]string{
		"missing":   "",
		"wrong":     "sha256=" + strings.Repeat("0", 64),
		"not hex":   "sha256=zz",
		"no prefix": strings.TrimPrefix(sign(body), "sha256="),
		"sha1":      "sha1=" + strings.TrimPrefix(sign(body), "sha256="),
	} {
		rec := e.deliverSigned("push", "x-"+name, body, sig)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, rec.Code)
		}
	}
	// Garbage with a bad signature gets the same answer as a good payload
	// with one: nothing about the body is looked at first.
	if rec := e.deliverSigned("push", "x-garbage", []byte("not json"), "sha256=00"); rec.Code != http.StatusUnauthorized {
		t.Errorf("garbage: status %d, want 401", rec.Code)
	}
	if pending, _ := e.st.PendingEvents(context.Background()); len(pending) != 0 || len(e.runs(t)) != 0 || len(e.src.readAt) != 0 {
		t.Fatal("a refused delivery left something behind")
	}
}

func TestWithoutASecretNothingIsAccepted(t *testing.T) {
	e := newEnv(t, wf("ci.yml", "push"))
	e.h.Secret = nil
	body := fixture(t, "push.json", nil)
	// The signature an attacker would compute with an empty key.
	mac := hmac.New(sha256.New, nil)
	mac.Write(body)
	rec := e.deliverSigned("push", "d", body, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
}

func TestPingIsAnsweredAndOtherEventsAreIgnored(t *testing.T) {
	e := newEnv(t, wf("ci.yml", "push"))
	if rec := e.deliver("ping", "p", []byte(`{"zen":"Keep it logically awesome."}`)); rec.Code != http.StatusOK {
		t.Fatalf("ping: %d", rec.Code)
	}
	rec := e.deliver("issues", "i", fixture(t, "push.json", nil))
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "ignored") {
		t.Fatalf("other event: %d %s", rec.Code, rec.Body)
	}
	if len(e.runs(t)) != 0 {
		t.Fatal("an event the server does not handle started a run")
	}
}

func TestOnlyEnabledRepositoriesAreServedAndNothingElseIsStored(t *testing.T) {
	e := newEnv(t, wf("ci.yml", "push"))
	other := fixture(t, "push.json", func(m map[string]any) { m["repository"].(map[string]any)["full_name"] = "evil/repo" })
	rec := e.deliver("push", "d", other)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "not enabled") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if _, err := e.st.GetEvent(context.Background(), 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a delivery for a repository that is not enabled was stored")
	}
	e.h.Allow = nil
	if rec := e.deliver("push", "d2", fixture(t, "push.json", nil)); !strings.Contains(rec.Body.String(), "not enabled") {
		t.Fatal("an empty allow list must allow nothing")
	}
	e.h.Allow = []string{"acme/web"}
	if rec := e.deliver("push", "d3", fixture(t, "push.json", nil)); !strings.Contains(rec.Body.String(), "accepted") {
		t.Fatalf("an exact name must be allowed: %s", rec.Body)
	}
}

func TestPullRequestRunsAtItsHeadAndMarksForks(t *testing.T) {
	e := newEnv(t, wf("ci.yml", "pull_request"))
	if rec := e.deliver("pull_request", "pr-1", fixture(t, "pull_request.json", nil)); rec.Code != http.StatusAccepted {
		t.Fatalf("%d", rec.Code)
	}
	r := e.runs(t)[0]
	if r.Event != "pull_request" || r.PullRequest != 7 || r.Ref != "refs/pull/7/merge" || r.SHA != strings.Repeat("3", 40) ||
		r.BaseSHA != strings.Repeat("4", 40) || r.Fork || r.Actor != "bob" {
		t.Fatalf("run = %+v", r)
	}

	fork := fixture(t, "pull_request.json", func(m map[string]any) {
		m["pull_request"].(map[string]any)["head"].(map[string]any)["repo"] = map[string]any{"full_name": "mallory/web"}
		m["action"] = "synchronize"
	})
	e.deliver("pull_request", "pr-2", fork)
	if latest := e.runs(t)[0]; !latest.Fork {
		t.Fatal("a pull request from another repository must be marked as a fork")
	}
	gone := fixture(t, "pull_request.json", func(m map[string]any) {
		m["pull_request"].(map[string]any)["head"].(map[string]any)["repo"] = nil
		m["action"] = "reopened"
	})
	e.deliver("pull_request", "pr-3", gone)
	if latest := e.runs(t)[0]; !latest.Fork {
		t.Fatal("a pull request whose fork was deleted must be treated as a fork")
	}
}

func TestPullRequestActivityTypesFollowGitHubsDefaults(t *testing.T) {
	e := newEnv(t, wf("ci.yml", "pull_request"))
	labeled := fixture(t, "pull_request.json", func(m map[string]any) { m["action"] = "labeled" })
	e.deliver("pull_request", "pr-l", labeled)
	if len(e.runs(t)) != 0 {
		t.Fatal("a label change must not start a workflow that does not list the labeled type")
	}
	if ev := e.event(t, 1); ev.Status != store.EventIgnored || !strings.Contains(ev.Note, "labeled") {
		t.Fatalf("event = %+v", ev)
	}
}

func TestPushThatDeletesARefOrCreatesABranch(t *testing.T) {
	e := newEnv(t, wf("docs.yml", "{push: {paths: ['docs/**']}}"))
	e.src.changed = []string{"cmd/x.go"}

	del := fixture(t, "push.json", func(m map[string]any) { m["deleted"] = true; m["after"] = strings.Repeat("0", 40) })
	e.deliver("push", "del", del)
	if len(e.runs(t)) != 0 || len(e.src.readAt) != 0 {
		t.Fatal("deleting a branch must neither run nor read anything")
	}

	created := fixture(t, "push.json", func(m map[string]any) { m["before"] = strings.Repeat("0", 40); m["created"] = true })
	e.deliver("push", "new", created)
	if e.src.changedCalls != 0 {
		t.Fatal("a new branch has no earlier commit to compare with")
	}
	if len(e.runs(t)) != 1 {
		t.Fatal("with the changed files unknown, a path filter must not block the run")
	}
}

func TestAnInvalidWorkflowDoesNotStopTheOthers(t *testing.T) {
	e := newEnv(t, github.File{Path: ".github/workflows/bad.yml", Content: []byte("on: push\njobs: {}\nunknown_key: 1\n")}, wf("ok.yml", "push"))
	e.deliver("push", "d", fixture(t, "push.json", nil))
	runs := e.runs(t)
	if len(runs) != 1 || runs[0].WorkflowPath != ".github/workflows/ok.yml" {
		t.Fatalf("runs = %+v", runs)
	}
	ev := e.event(t, 1)
	if !strings.Contains(ev.Note, `"result":"invalid"`) {
		t.Fatalf("the invalid file must be named in the note: %s", ev.Note)
	}
}

func TestGitHubFailingMarksTheDeliveryFailedAndRecoverTriesAgain(t *testing.T) {
	e := newEnv(t, wf("ci.yml", "push"))
	e.src.filesErr = errors.New("github is down")
	e.deliver("push", "d", fixture(t, "push.json", nil))
	if ev := e.event(t, 1); ev.Status != store.EventFailed || !strings.Contains(ev.Note, "github is down") {
		t.Fatalf("event = %+v", ev)
	}

	// A delivery the server stopped in the middle of stays "received".
	body := fixture(t, "push.json", nil)
	pending, _, err := e.st.RecordEvent(context.Background(), store.Event{DeliveryID: "left", Kind: "push", Repo: "acme/web", InstallationID: 42, Payload: body})
	if err != nil {
		t.Fatal(err)
	}
	e.src.filesErr = nil
	if err := e.h.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.h.Wait()
	if ev := e.event(t, pending.ID); ev.Status != store.EventHandled {
		t.Fatalf("recovered event = %+v", ev)
	}
	if len(e.runs(t)) != 1 {
		t.Fatalf("%d runs after recovery", len(e.runs(t)))
	}
	// Recovering again finds nothing to do and starts nothing twice.
	e.h.Recover(context.Background())
	e.h.Wait()
	if len(e.runs(t)) != 1 {
		t.Fatal("recovering twice started a second run")
	}
}

func TestTheAnswerComesBeforeTheWork(t *testing.T) {
	e := newEnv(t, wf("ci.yml", "push"))
	e.h.Inline = false
	release := make(chan struct{})
	slow := &slowSource{fakeSource: e.src, release: release}
	e.h.Source = slow
	rec := e.deliver2("push", "d", fixture(t, "push.json", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d", rec.Code)
	}
	if len(e.runs(t)) != 0 {
		t.Fatal("the run must not exist before the background handling is done")
	}
	close(release)
	e.h.Wait()
	if len(e.runs(t)) != 1 {
		t.Fatal("the background handling did not create the run")
	}
}

// slowSource blocks WorkflowFiles until released.
type slowSource struct {
	*fakeSource
	release chan struct{}
}

func (s *slowSource) WorkflowFiles(ctx context.Context, inst int64, repo, ref string) ([]github.File, error) {
	<-s.release
	return s.fakeSource.WorkflowFiles(ctx, inst, repo, ref)
}

// deliver2 is deliver without waiting for the background work.
func (e *env) deliver2(kind, delivery string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", kind)
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", sign(body))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func TestOnlyPostIsAccepted(t *testing.T) {
	e := newEnv(t)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest("GET", "/webhooks/github", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d", rec.Code)
	}
}
