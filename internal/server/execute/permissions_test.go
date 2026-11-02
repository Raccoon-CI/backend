package execute

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"

	"github.com/Raccoon-CI/raccoonci/internal/server/store"
	"github.com/Raccoon-CI/raccoonci/pkg/workflow"
)

func parseWF(t *testing.T, text string) *workflow.Workflow {
	t.Helper()
	w, err := workflow.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestTokenPermissions(t *testing.T) {
	const body = "jobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: x\n"
	const jobPerm = "jobs:\n  a:\n    runs-on: ubuntu-latest\n    permissions:\n      issues: write\n    steps:\n      - run: x\n"
	for _, c := range []struct {
		name, text string
		want       map[string]string
	}{
		{"nothing said: read-only contents", "on: push\n" + body, map[string]string{"contents": "read", "metadata": "read"}},
		{"workflow mapping; unnamed is none; id-token is not an App permission",
			"on: push\npermissions:\n  pull-requests: write\n  id-token: write\n  contents: none\n" + body,
			map[string]string{"pull_requests": "write", "metadata": "read"}},
		{"empty mapping: nothing but metadata", "on: push\npermissions: {}\n" + body, map[string]string{"metadata": "read"}},
		{"read-all", "on: push\npermissions: read-all\n" + body, nil},
		{"the job's own wins over the workflow's", "on: push\npermissions:\n  contents: write\n" + jobPerm,
			map[string]string{"issues": "write", "metadata": "read"}},
	} {
		w := parseWF(t, c.text)
		got := tokenPermissions(w, w.Jobs[0])
		if c.want == nil { // read-all: everything read
			if got["contents"] != "read" || got["pull_requests"] != "read" || got["security_events"] != "read" || got["metadata"] != "read" {
				t.Errorf("%s: %v", c.name, got)
			}
			for k, v := range got {
				if v != "read" {
					t.Errorf("%s: %s = %s", c.name, k, v)
				}
			}
			continue
		}
		if !maps.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	w := parseWF(t, "on: push\npermissions: write-all\n"+body)
	if got := tokenPermissions(w, w.Jobs[0]); got["contents"] != "write" || got["issues"] != "write" {
		t.Errorf("write-all: %v", got)
	}
	// The default must not be changed by a caller.
	d := tokenPermissions(nil, nil)
	d["contents"] = "write"
	if defaultPermissions["contents"] != "read" {
		t.Fatal("the default permissions were modified through a returned map")
	}
}

func TestWantsIDToken(t *testing.T) {
	const jobs = "jobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: x\n"
	for _, c := range []struct {
		name, text string
		want       bool
	}{
		{"nothing said", "on: push\n" + jobs, false},
		{"workflow id-token: write", "on: push\npermissions:\n  id-token: write\n" + jobs, true},
		{"id-token: read is not enough", "on: push\npermissions:\n  id-token: read\n" + jobs, false},
		{"other permissions only", "on: push\npermissions:\n  contents: write\n" + jobs, false},
		{"write-all", "on: push\npermissions: write-all\n" + jobs, true},
		{"read-all", "on: push\npermissions: read-all\n" + jobs, false},
		{"the job's own wins over the workflow's", "on: push\npermissions:\n  id-token: write\njobs:\n  a:\n    runs-on: ubuntu-latest\n    permissions:\n      contents: read\n    steps:\n      - run: x\n", false},
		{"and the other way", "on: push\npermissions:\n  contents: read\njobs:\n  a:\n    runs-on: ubuntu-latest\n    permissions:\n      id-token: write\n    steps:\n      - run: x\n", true},
	} {
		w := parseWF(t, c.text)
		if got := wantsIDToken(w, w.Jobs[0]); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

type fakeIDTokens struct {
	mu   sync.Mutex
	jobs []string
}

func (f *fakeIDTokens) Env(run int64, job string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = append(f.jobs, job)
	return map[string]string{"ACTIONS_ID_TOKEN_REQUEST_URL": "https://ci.example/oidc/token?api-version=2.0", "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "req-" + job}
}

const withIDToken = `on: push
permissions:
  id-token: write
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
  b:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      - run: echo hi
`

func TestOnlyAJobWithIDTokenWriteGetsTheOIDCEnvironmentAndNeverAFork(t *testing.T) {
	e := newEnv(t)
	ids := &fakeIDTokens{}
	e.ex.IDTokens = ids
	run := e.queue(t, "oidc", withIDToken, nil)
	e.start(t)
	e.wait(t, run.ID)
	e.m.mu.Lock()
	byJob := map[string]map[string]string{}
	for _, c := range e.m.calls {
		byJob[c.spec.Job] = c.spec.ServiceEnv
	}
	e.m.mu.Unlock()
	if byJob["a"]["ACTIONS_ID_TOKEN_REQUEST_URL"] == "" || !strings.HasPrefix(byJob["a"]["ACTIONS_ID_TOKEN_REQUEST_TOKEN"], "req-a-") {
		t.Errorf("job a (id-token: write) env = %v", byJob["a"])
	}
	if _, ok := byJob["b"]["ACTIONS_ID_TOKEN_REQUEST_URL"]; ok {
		t.Errorf("job b (permissions: contents: read) was given ID tokens: %v", byJob["b"])
	}
	if len(ids.jobs) != 1 {
		t.Errorf("IDTokens asked for %v", ids.jobs)
	}

	fe := newEnv(t)
	fe.ex.IDTokens = &fakeIDTokens{}
	fork := fe.queue(t, "fork", withIDToken, func(r *store.Run) { r.Fork = true; r.Event = "pull_request"; r.PullRequest = 3 })
	fe.start(t)
	fe.wait(t, fork.ID)
	fe.m.mu.Lock()
	defer fe.m.mu.Unlock()
	for _, c := range fe.m.calls {
		if _, ok := c.spec.ServiceEnv["ACTIONS_ID_TOKEN_REQUEST_URL"]; ok {
			t.Errorf("a fork's job %s was given ID tokens", c.spec.Job)
		}
	}
}

type fakeMinter struct {
	mu    sync.Mutex
	asked []map[string]string
	// refuse says which permission sets fail: a set holding this key fails.
	refuse  string
	all     bool
	revoked []string
}

func (f *fakeMinter) RevokeToken(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, token)
	return nil
}

func (f *fakeMinter) RunToken(_ context.Context, inst int64, repo string, perms map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, maps.Clone(perms))
	if f.all {
		return "", errors.New("github is down")
	}
	if _, bad := perms[f.refuse]; f.refuse != "" && bad {
		return "", errors.New("not granted")
	}
	return "ghs_job_" + repo, nil
}

const withPerm = `on: push
permissions:
  issues: write
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`

func (e *env) tokenOf(t *testing.T) (string, bool) {
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	tok, ok := e.m.calls[0].job.Secrets["GITHUB_TOKEN"]
	return tok, ok
}

func TestEachJobGetsAScopedGithubToken(t *testing.T) {
	e := newEnv(t)
	mint := &fakeMinter{}
	e.ex.Tokens = mint
	run := e.queue(t, "tok", withPerm, nil)
	e.start(t)
	e.wait(t, run.ID)
	if tok, ok := e.tokenOf(t); !ok || tok != "ghs_job_acme/web" {
		t.Fatalf("GITHUB_TOKEN = %q, %v", tok, ok)
	}
	if len(mint.asked) != 1 || mint.asked[0]["issues"] != "write" {
		t.Fatalf("asked for %v", mint.asked)
	}
	waitFor(t, "the token to be revoked", func() bool {
		mint.mu.Lock()
		defer mint.mu.Unlock()
		return len(mint.revoked) == 1
	})
	if mint.revoked[0] != "ghs_job_acme/web" {
		t.Fatalf("revoked %v", mint.revoked)
	}
	for _, ev := range e.log(t, run.ID) {
		if strings.Contains(ev.Text, "ghs_job") {
			t.Fatalf("the token is in the log: %q", ev.Text)
		}
	}
}

func TestNoGithubTokenForAForkWithoutAnInstallationOrWithoutAMinter(t *testing.T) {
	for _, c := range []struct {
		name string
		prep func(*env)
		edit func(*store.Run)
	}{
		{"fork", nil, func(r *store.Run) { r.Fork = true; r.Event = "pull_request"; r.PullRequest = 3 }},
		{"no installation", func(e *env) { z := int64(0); e.installation = &z }, nil},
		{"no minter", func(e *env) { e.ex.Tokens = nil }, nil},
	} {
		e := newEnv(t)
		e.ex.Tokens = &fakeMinter{}
		if c.prep != nil {
			c.prep(e)
		}
		run := e.queue(t, "n", withPerm, c.edit)
		e.start(t)
		e.wait(t, run.ID)
		if tok, ok := e.tokenOf(t); ok {
			t.Errorf("%s: got GITHUB_TOKEN %q", c.name, tok)
		}
	}
}

func TestARefusedPermissionFallsBackToTheDefaultTokenWithAWarning(t *testing.T) {
	e := newEnv(t)
	mint := &fakeMinter{refuse: "issues"}
	e.ex.Tokens = mint
	run := e.queue(t, "fb", withPerm, nil)
	e.start(t)
	if done := e.wait(t, run.ID); done.Conclusion != "success" {
		t.Fatalf("conclusion %q", done.Conclusion)
	}
	if tok, ok := e.tokenOf(t); !ok || tok == "" {
		t.Fatal("no fallback token")
	}
	if len(mint.asked) != 2 || !maps.Equal(mint.asked[1], defaultPermissions) {
		t.Fatalf("asked for %v", mint.asked)
	}
	if !logHas(e, t, run.ID, "cannot give the permissions") {
		t.Fatalf("no warning: %+v", e.log(t, run.ID))
	}
}

func TestAJobRunsWithoutATokenWhenNoneCanBeMade(t *testing.T) {
	e := newEnv(t)
	e.ex.Tokens = &fakeMinter{all: true}
	run := e.queue(t, "dead", withPerm, nil)
	e.start(t)
	if done := e.wait(t, run.ID); done.Conclusion != "success" {
		t.Fatalf("conclusion %q", done.Conclusion)
	}
	if _, ok := e.tokenOf(t); ok {
		t.Fatal("a token appeared from nowhere")
	}
	if !logHas(e, t, run.ID, "no GITHUB_TOKEN could be made") {
		t.Fatalf("no warning: %+v", e.log(t, run.ID))
	}
}
