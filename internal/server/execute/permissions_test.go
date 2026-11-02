package execute

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"

	"raccoonci/internal/server/store"
	"raccoonci/internal/workflow"
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

type fakeMinter struct {
	mu    sync.Mutex
	asked []map[string]string
	// refuse says which permission sets fail: a set holding this key fails.
	refuse string
	all    bool
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
