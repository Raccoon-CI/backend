package execute

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"raccoonci/internal/dispatch"
	"raccoonci/internal/runner"
	"raccoonci/internal/server/github"
	"raccoonci/internal/wire"
)

func TestAnnotationOf(t *testing.T) {
	ev := func(k runner.EventKind, text string, props map[string]string) runner.Event {
		return runner.Event{Kind: k, Text: text, Props: props}
	}
	for _, c := range []struct {
		name string
		e    runner.Event
		want github.Annotation
		ok   bool
	}{
		{"an error with a place", ev(runner.Error, "boom", map[string]string{"file": "./src/a.go", "line": "3", "endLine": "5", "title": "T"}),
			github.Annotation{Path: "src/a.go", StartLine: 3, EndLine: 5, Level: "failure", Title: "T", Message: "boom"}, true},
		{"a warning without a place goes to .github line 1", ev(runner.Warning, "careful", nil),
			github.Annotation{Path: ".github", StartLine: 1, EndLine: 1, Level: "warning", Message: "careful"}, true},
		{"a notice", ev(runner.Notice, "fyi", map[string]string{"file": "x.txt"}),
			github.Annotation{Path: "x.txt", StartLine: 1, EndLine: 1, Level: "notice", Message: "fyi"}, true},
		{"a line without a file has no place of its own", ev(runner.Error, "x", map[string]string{"line": "9"}),
			github.Annotation{Path: ".github", StartLine: 1, EndLine: 1, Level: "failure", Message: "x"}, true},
		{"a file outside the tree is not trusted", ev(runner.Error, "x", map[string]string{"file": "../../etc/passwd", "line": "2"}),
			github.Annotation{Path: ".github", StartLine: 1, EndLine: 1, Level: "failure", Message: "x"}, true},
		{"an absolute file is not trusted", ev(runner.Error, "x", map[string]string{"file": "/etc/passwd", "line": "2"}),
			github.Annotation{Path: ".github", StartLine: 1, EndLine: 1, Level: "failure", Message: "x"}, true},
		{"an end line before the start is ignored", ev(runner.Error, "x", map[string]string{"file": "a", "line": "7", "endLine": "2"}),
			github.Annotation{Path: "a", StartLine: 7, EndLine: 7, Level: "failure", Message: "x"}, true},
		{"output is not an annotation", ev(runner.Output, "hello", nil), github.Annotation{}, false},
		{"an empty message is not one", ev(runner.Error, "  ", nil), github.Annotation{}, false},
	} {
		got, ok := annotationOf(c.e)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got %+v, %v; want %+v, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestOnlyTheFirstFiftyAnnotationsAreKeptAndTheRestCounted(t *testing.T) {
	c := &annotations{}
	for i := 0; i < 60; i++ {
		c.add(runner.Event{Kind: runner.Warning, Text: "w"})
	}
	list, dropped := c.get()
	if len(list) != github.MaxAnnotations || dropped != 10 {
		t.Fatalf("kept %d, dropped %d", len(list), dropped)
	}
}

func TestACheckRunGetsTheJobSummaryAndTheAnnotationsOfItsSteps(t *testing.T) {
	e := newEnv(t)
	checks := &fakeChecks{}
	e.ex.Checks = checks
	e.m.handle = func(c call, out *wire.Encoder) (dispatch.Result, error) {
		now := time.Now()
		out.Write(wire.Line{Event: &runner.Event{Kind: runner.Error, Time: now, Job: c.spec.Job, Text: "boom", Props: map[string]string{"file": "a.go", "line": "3"}}})
		out.Write(wire.Line{Event: &runner.Event{Kind: runner.Output, Time: now, Job: c.spec.Job, Text: "just output"}})
		out.Write(wire.Line{Result: &wire.Result{Conclusion: runner.Failure, Outputs: json.RawMessage(`{}`), Summary: "## Results\n3 failed"}})
		return dispatch.Result{}, nil
	}
	run := e.queue(t, "ann", twoJobs, nil)
	e.start(t)
	e.wait(t, run.ID)
	waitLines(checks, 2) // build's create and update
	checks.mu.Lock()
	defer checks.mu.Unlock()
	if len(checks.updates) == 0 {
		t.Fatal("no check run was completed")
	}
	u := checks.updates[0]
	if u.Summary != "## Results\n3 failed" {
		t.Errorf("summary = %q, want the job summary", u.Summary)
	}
	if len(u.Annotations) != 1 || u.Annotations[0].Path != "a.go" || u.Annotations[0].StartLine != 3 || u.Annotations[0].Message != "boom" {
		t.Errorf("annotations = %+v", u.Annotations)
	}
}

func TestACheckRunWithoutASummaryKeepsItsTitleLine(t *testing.T) {
	e := newEnv(t)
	checks := &fakeChecks{}
	e.ex.Checks = checks
	run := e.queue(t, "nosum", twoJobs, nil)
	e.start(t)
	e.wait(t, run.ID)
	waitLines(checks, 2)
	checks.mu.Lock()
	defer checks.mu.Unlock()
	if len(checks.updates) == 0 || !strings.Contains(checks.updates[0].Summary, "raccoonci / CI / build") {
		t.Fatalf("updates = %+v", checks.updates)
	}
}
