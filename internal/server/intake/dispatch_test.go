package intake

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"raccoonci/internal/server/github"
)

const dispatchable = `on:
  workflow_dispatch:
    inputs:
      level:
        type: choice
        options: [debug, info]
        default: info
      fast:
        type: boolean
        default: false
      who:
        required: true
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`

func dispatchEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, github.File{Path: ".github/workflows/manual.yml", Content: []byte(dispatchable)}, wf("push-only.yml", "push"))
	e.src.refs = map[string]string{"refs/heads/main": strings.Repeat("9", 40), "refs/tags/v1": strings.Repeat("8", 40)}
	return e
}

func statusOf(err error) int {
	var re *RequestError
	if errors.As(err, &re) {
		return re.Status
	}
	return 0
}

func TestDispatchStartsAWorkflowAtTheCommitTheRefNamesNow(t *testing.T) {
	e := dispatchEnv(t)
	notified := 0
	e.h.Notify = func() { notified++ }
	run, err := e.h.Dispatch(context.Background(), DispatchRequest{Repo: "acme/web", Ref: "main", Workflow: ".github/workflows/manual.yml",
		Inputs: map[string]string{"who": "me", "fast": "true"}, Actor: "carol"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Event != "workflow_dispatch" || run.Ref != "refs/heads/main" || run.SHA != strings.Repeat("9", 40) || run.Actor != "carol" || run.Status != "queued" {
		t.Fatalf("run = %+v", run)
	}
	var inputs map[string]any
	if err := json.Unmarshal([]byte(run.Inputs), &inputs); err != nil {
		t.Fatal(err)
	}
	if inputs["who"] != "me" || inputs["fast"] != true || inputs["level"] != "info" {
		t.Fatalf("inputs = %v: given ones typed, defaults filled", inputs)
	}
	if !strings.Contains(run.WorkflowText, "workflow_dispatch") {
		t.Fatal("the run must keep the workflow text")
	}
	ev := e.event(t, run.EventID)
	if ev.Kind != "workflow_dispatch" || !strings.Contains(string(ev.Payload), `"ref":"refs/heads/main"`) {
		t.Fatalf("event = %+v", ev)
	}
	if notified != 1 {
		t.Fatalf("notified %d times", notified)
	}
	tag, err := e.h.Dispatch(context.Background(), DispatchRequest{Repo: "acme/web", Ref: "refs/tags/v1", Workflow: ".github/workflows/manual.yml", Inputs: map[string]string{"who": "x"}})
	if err != nil || tag.Ref != "refs/tags/v1" || tag.SHA != strings.Repeat("8", 40) || tag.Actor != "api" {
		t.Fatalf("tag run = %+v, %v", tag, err)
	}
}

func TestRerunRepeatsAFinishedRunExactly(t *testing.T) {
	e := dispatchEnv(t)
	notified := 0
	e.h.Notify = func() { notified++ }
	first, err := e.h.Dispatch(context.Background(), DispatchRequest{Repo: "acme/web", Ref: "main", Workflow: ".github/workflows/manual.yml",
		Inputs: map[string]string{"who": "me"}, Actor: "carol"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.Rerun(context.Background(), first.ID); statusOf(err) != http.StatusConflict {
		t.Fatalf("rerunning a queued run: %v", err)
	}
	e.st.ClaimRun(context.Background())
	e.st.FinishRun(context.Background(), first.ID, "failure")
	// The workflow file changes on GitHub after the first run: the rerun must use the old text.
	e.src.files[0].Content = []byte("on: push\njobs: {}\n")
	again, err := e.h.Rerun(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID == first.ID || again.RerunOf != first.ID || again.Status != "queued" || again.SHA != first.SHA || again.Inputs != first.Inputs ||
		again.Event != "workflow_dispatch" || again.Actor != "carol" || again.WorkflowText != first.WorkflowText || again.Ref != first.Ref {
		t.Fatalf("rerun = %+v of %+v", again, first)
	}
	if notified != 2 {
		t.Fatalf("notified %d times", notified)
	}
	e.st.ClaimRun(context.Background())
	e.st.FinishRun(context.Background(), again.ID, "success")
	third, err := e.h.Rerun(context.Background(), again.ID)
	if err != nil || third.RerunOf != first.ID {
		t.Fatalf("a rerun of a rerun must point at the first run: %+v, %v", third, err)
	}
	if ev := e.event(t, third.EventID); ev.Kind != "workflow_dispatch" || !strings.Contains(string(ev.Payload), "refs/heads/main") || !strings.Contains(ev.Note, "rerun") {
		t.Fatalf("event = %+v", ev)
	}
	if _, err := e.h.Rerun(context.Background(), 999); statusOf(err) != http.StatusNotFound {
		t.Fatalf("a missing run: %v", err)
	}
}

func TestDispatchRefusesWhatCannotBeRun(t *testing.T) {
	e := dispatchEnv(t)
	base := DispatchRequest{Repo: "acme/web", Ref: "main", Workflow: ".github/workflows/manual.yml", Inputs: map[string]string{"who": "me"}}
	cases := map[string]struct {
		edit func(*DispatchRequest)
		want int
	}{
		"not enabled":       {func(r *DispatchRequest) { r.Repo = "evil/repo" }, http.StatusForbidden},
		"bad repo":          {func(r *DispatchRequest) { r.Repo = "no-slash" }, http.StatusBadRequest},
		"bad ref":           {func(r *DispatchRequest) { r.Ref = "a b" }, http.StatusBadRequest},
		"dotdot ref":        {func(r *DispatchRequest) { r.Ref = "a/../b" }, http.StatusBadRequest},
		"pull ref":          {func(r *DispatchRequest) { r.Ref = "refs/pull/7/merge" }, http.StatusBadRequest},
		"outside workflows": {func(r *DispatchRequest) { r.Workflow = "README.md" }, http.StatusBadRequest},
		"dotdot workflow":   {func(r *DispatchRequest) { r.Workflow = ".github/workflows/../x.yml" }, http.StatusBadRequest},
		"unknown ref":       {func(r *DispatchRequest) { r.Ref = "nope" }, http.StatusNotFound},
		"missing file":      {func(r *DispatchRequest) { r.Workflow = ".github/workflows/none.yml" }, http.StatusNotFound},
		"no dispatch":       {func(r *DispatchRequest) { r.Workflow = ".github/workflows/push-only.yml" }, http.StatusUnprocessableEntity},
		"unknown input":     {func(r *DispatchRequest) { r.Inputs = map[string]string{"who": "x", "extra": "y"} }, http.StatusUnprocessableEntity},
		"missing input":     {func(r *DispatchRequest) { r.Inputs = nil }, http.StatusUnprocessableEntity},
		"bad choice":        {func(r *DispatchRequest) { r.Inputs = map[string]string{"who": "x", "level": "loud"} }, http.StatusUnprocessableEntity},
		"bad boolean":       {func(r *DispatchRequest) { r.Inputs = map[string]string{"who": "x", "fast": "maybe"} }, http.StatusUnprocessableEntity},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			req := base
			c.edit(&req)
			_, err := e.h.Dispatch(context.Background(), req)
			if got := statusOf(err); got != c.want {
				t.Fatalf("status %d (%v), want %d", got, err, c.want)
			}
		})
	}
	if len(e.runs(t)) != 0 {
		t.Fatal("a refused dispatch left a run")
	}
}
