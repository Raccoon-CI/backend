package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"raccoonci/internal/server/intake"
	"raccoonci/internal/server/store"
)

type fakeDispatcher struct {
	got   intake.DispatchRequest
	rerun int64
	run   store.Run
	err   error
}

func (f *fakeDispatcher) Dispatch(_ context.Context, req intake.DispatchRequest) (store.Run, error) {
	f.got = req
	return f.run, f.err
}

func (f *fakeDispatcher) Rerun(_ context.Context, id int64) (store.Run, error) {
	f.rerun = id
	return f.run, f.err
}

type fakeCanceller struct{ running map[int64]bool }

func (f *fakeCanceller) Cancel(id int64) bool { return f.running[id] }

func TestCancelAQueuedAndARunningRunButNotAFinishedOne(t *testing.T) {
	a, st, running := runSetup(t) // the one run of the setup is claimed below
	ctx := context.Background()
	if got, ok, _ := st.ClaimRun(ctx); !ok || got.ID != running.ID {
		t.Fatal("setup: claiming the first run")
	}
	ev, _, _ := st.RecordEvent(ctx, store.Event{DeliveryID: "d2", Kind: "push", Repo: "acme/web", Payload: []byte("{}")})
	done, _, _ := st.CreateRun(ctx, store.Run{EventID: ev.ID, Repo: "acme/web", WorkflowPath: "c.yml", Event: "push", SHA: "abc", WorkflowText: "x"})
	st.ClaimRun(ctx)
	st.FinishRun(ctx, done.ID, "success")
	queued, _, _ := st.CreateRun(ctx, store.Run{EventID: ev.ID, Repo: "acme/web", WorkflowPath: "b.yml", Event: "push", SHA: "abc", WorkflowText: "x"})
	a.Canceller = &fakeCanceller{running: map[int64]bool{running.ID: true}}
	h := a.Handler()

	if rec := do(h, "POST", fmt.Sprintf("/v1/runs/%d/cancel", queued.ID), ""); rec.Code != 200 {
		t.Fatalf("queued: %d %s", rec.Code, rec.Body)
	}
	if got, _ := st.GetRun(ctx, queued.ID); got.Status != store.RunCompleted || got.Conclusion != "cancelled" {
		t.Fatalf("queued run = %+v", got)
	}
	if rec := do(h, "POST", fmt.Sprintf("/v1/runs/%d/cancel", running.ID), ""); rec.Code != 202 {
		t.Fatalf("running: %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, "POST", fmt.Sprintf("/v1/runs/%d/cancel", done.ID), ""); rec.Code != 409 || !strings.Contains(rec.Body.String(), "completed") {
		t.Fatalf("finished: %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, "POST", fmt.Sprintf("/v1/runs/%d/cancel", queued.ID), ""); rec.Code != 409 {
		t.Fatalf("cancelled twice: %d", rec.Code)
	}
	if rec := do(h, "POST", "/v1/runs/999/cancel", ""); rec.Code != 404 {
		t.Fatalf("missing: %d", rec.Code)
	}
	if rec := do(h, "POST", "/v1/runs/x/cancel", ""); rec.Code != 400 {
		t.Fatalf("bad id: %d", rec.Code)
	}
}

func TestRerunRoute(t *testing.T) {
	a, _, run := runSetup(t)
	f := &fakeDispatcher{run: store.Run{ID: 7, RerunOf: run.ID, Repo: "acme/web", WorkflowText: "on: push\n"}}
	a.Dispatcher = f
	rec := do(a.Handler(), "POST", fmt.Sprintf("/v1/runs/%d/rerun", run.ID), "")
	if rec.Code != 201 || f.rerun != run.ID || !strings.Contains(rec.Body.String(), `"rerun_of":`) || strings.Contains(rec.Body.String(), "on: push") {
		t.Fatalf("%d %s (asked for run %d)", rec.Code, rec.Body, f.rerun)
	}
	f.err = &intake.RequestError{Status: 409, Msg: "still running"}
	if rec := do(a.Handler(), "POST", "/v1/runs/1/rerun", ""); rec.Code != 409 {
		t.Fatalf("refusal: %d", rec.Code)
	}
	a.Dispatcher = nil
	if rec := do(a.Handler(), "POST", "/v1/runs/1/rerun", ""); rec.Code != 503 {
		t.Fatalf("no dispatcher: %d", rec.Code)
	}
}

func runSetup(t *testing.T) (*API, *store.SQLite, store.Run) {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	ev, _, _ := st.RecordEvent(ctx, store.Event{DeliveryID: "d", Kind: "push", Repo: "acme/web", Payload: []byte("{}")})
	run, _, err := st.CreateRun(ctx, store.Run{EventID: ev.ID, Repo: "acme/web", WorkflowPath: "ci.yml", Event: "push", SHA: "abc", WorkflowText: "on: push\n"})
	if err != nil {
		t.Fatal(err)
	}
	return &API{Store: st, Token: token}, st, run
}

func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestJobsOfARun(t *testing.T) {
	a, st, run := runSetup(t)
	ctx := context.Background()
	j, _ := st.StartJob(ctx, run.ID, "build")
	st.FinishJob(ctx, j.ID, "success")
	st.StartJob(ctx, run.ID, "test")
	rec := do(a.Handler(), "GET", fmt.Sprintf("/v1/runs/%d/jobs", run.ID), "")
	var out struct {
		Jobs []map[string]any `json:"jobs"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 || len(out.Jobs) != 2 || out.Jobs[0]["conclusion"] != "success" || out.Jobs[0]["completed_at"] == nil ||
		out.Jobs[1]["status"] != "in_progress" || out.Jobs[1]["completed_at"] != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := do(a.Handler(), "GET", "/v1/runs/999/jobs", ""); rec.Code != 404 {
		t.Fatalf("missing run: %d", rec.Code)
	}
}

func TestLogPagesAndSaysWhenItIsDone(t *testing.T) {
	a, st, run := runSetup(t)
	ctx := context.Background()
	var lines []store.LogLine
	for i := 1; i <= 5; i++ {
		lines = append(lines, store.LogLine{Data: fmt.Sprintf(`{"kind":"output","text":"line %d"}`, i)})
	}
	st.AppendLog(ctx, run.ID, lines)
	type page struct {
		Lines []struct {
			ID    int64           `json:"id"`
			Event json.RawMessage `json:"event"`
		} `json:"lines"`
		NextAfter int64 `json:"next_after"`
		Done      bool  `json:"done"`
	}
	read := func(q string) page {
		rec := do(a.Handler(), "GET", fmt.Sprintf("/v1/runs/%d/log%s", run.ID, q), "")
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", q, rec.Code, rec.Body)
		}
		var p page
		json.Unmarshal(rec.Body.Bytes(), &p)
		return p
	}
	p := read("?limit=2")
	if len(p.Lines) != 2 || p.Done || !strings.Contains(string(p.Lines[0].Event), "line 1") {
		t.Fatalf("page 1 = %+v", p)
	}
	p = read(fmt.Sprintf("?limit=10&after=%d", p.NextAfter))
	if len(p.Lines) != 3 || p.Done {
		t.Fatalf("page 2 = %+v: the run is still going, so it is not done", p)
	}
	st.ClaimRun(ctx)
	st.FinishRun(ctx, run.ID, "success")
	last := p.NextAfter
	p = read(fmt.Sprintf("?after=%d", last))
	if len(p.Lines) != 0 || !p.Done || p.NextAfter != last {
		t.Fatalf("after the end = %+v", p)
	}
	for _, bad := range []string{"?after=x", "?after=-1", "?limit=0", "?limit=99999"} {
		if rec := do(a.Handler(), "GET", fmt.Sprintf("/v1/runs/%d/log%s", run.ID, bad), ""); rec.Code != 400 {
			t.Errorf("%s: %d, want 400", bad, rec.Code)
		}
	}
}

func TestAPurgedLogIsGoneNotEmpty(t *testing.T) {
	a, st, run := runSetup(t)
	ctx := context.Background()
	st.AppendLog(ctx, run.ID, []store.LogLine{{Data: `{"kind":"output","text":"hi"}`}})
	st.ClaimRun(ctx)
	st.FinishRun(ctx, run.ID, "success")
	if _, _, err := st.PurgeLogs(ctx, time.Now().Add(time.Hour), 10); err != nil {
		t.Fatal(err)
	}
	h := a.Handler()
	path := fmt.Sprintf("/v1/runs/%d/log", run.ID)
	if rec := do(h, "GET", path, ""); rec.Code != 410 || !strings.Contains(rec.Body.String(), "deleted") {
		t.Fatalf("log: %d %s", rec.Code, rec.Body)
	}
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 410 {
		t.Fatalf("stream: %d", rec.Code)
	}
	if rec := do(h, "GET", fmt.Sprintf("/v1/runs/%d", run.ID), ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"log_purged":true`) || !strings.Contains(rec.Body.String(), `"conclusion":"success"`) {
		t.Fatalf("the run still shows, saying its log is gone: %d %s", rec.Code, rec.Body)
	}
}

func TestLogStreamsUntilTheRunEnds(t *testing.T) {
	a, st, run := runSetup(t)
	ctx := context.Background()
	st.ClaimRun(ctx)
	st.AppendLog(ctx, run.ID, []store.LogLine{{Data: `{"kind":"output","text":"first"}`}})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/v1/runs/%d/log", srv.URL, run.ID), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	events := make(chan string, 20)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		var cur []string
		for sc.Scan() {
			if sc.Text() == "" {
				events <- strings.Join(cur, "|")
				cur = nil
				continue
			}
			cur = append(cur, sc.Text())
		}
		close(events)
	}()
	next := func() string {
		select {
		case e := <-events:
			return e
		case <-time.After(5 * time.Second):
			t.Fatal("no event within 5 s")
			return ""
		}
	}
	if e := next(); !strings.Contains(e, `"first"`) || !strings.HasPrefix(e, "id: ") {
		t.Fatalf("first event = %q", e)
	}
	// A line written while the stream is open arrives, then the end.
	st.AppendLog(ctx, run.ID, []store.LogLine{{Data: `{"kind":"output","text":"second"}`}})
	if e := next(); !strings.Contains(e, `"second"`) {
		t.Fatalf("second event = %q", e)
	}
	st.FinishRun(ctx, run.ID, "failure")
	if e := next(); !strings.HasPrefix(e, "event: end") || !strings.Contains(e, `"failure"`) {
		t.Fatalf("end event = %q", e)
	}
	if _, open := <-events; open {
		t.Fatal("the stream stayed open after the end")
	}
}

func TestStreamResumesAfterLastEventID(t *testing.T) {
	a, st, run := runSetup(t)
	ctx := context.Background()
	st.ClaimRun(ctx)
	st.AppendLog(ctx, run.ID, []store.LogLine{{Data: `{"text":"one"}`}, {Data: `{"text":"two"}`}})
	st.FinishRun(ctx, run.ID, "success")
	lines, _ := st.ReadLog(ctx, run.ID, 0, 10)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/v1/runs/%d/log", srv.URL, run.ID), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Last-Event-ID", fmt.Sprint(lines[0].ID))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var all strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		all.WriteString(sc.Text() + "\n")
	}
	if strings.Contains(all.String(), `"one"`) || !strings.Contains(all.String(), `"two"`) || !strings.Contains(all.String(), "event: end") {
		t.Fatalf("stream = %q", all.String())
	}
}

func TestDispatchRoute(t *testing.T) {
	a, _, run := runSetup(t)
	f := &fakeDispatcher{run: run}
	a.Dispatcher = f
	rec := do(a.Handler(), "POST", "/v1/runs", `{"repo":"acme/web","ref":"main","workflow":".github/workflows/ci.yml","inputs":{"a":"b"}}`)
	if rec.Code != http.StatusCreated || strings.Contains(rec.Body.String(), "on: push") {
		t.Fatalf("%d %s: the created run must not carry the workflow text", rec.Code, rec.Body)
	}
	if f.got.Repo != "acme/web" || f.got.Ref != "main" || f.got.Inputs["a"] != "b" || f.got.Actor != "api" {
		t.Fatalf("request = %+v", f.got)
	}
	f.err = &intake.RequestError{Status: 422, Msg: "input \"who\" is required"}
	if rec := do(a.Handler(), "POST", "/v1/runs", `{"repo":"acme/web","ref":"main","workflow":"x"}`); rec.Code != 422 || !strings.Contains(rec.Body.String(), "required") {
		t.Fatalf("refusal: %d %s", rec.Code, rec.Body)
	}
	f.err = fmt.Errorf("github is down")
	if rec := do(a.Handler(), "POST", "/v1/runs", `{"repo":"acme/web","ref":"main","workflow":"x"}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("other error: %d", rec.Code)
	}
	for _, bad := range []string{`not json`, `{"repo":"a/b","surprise":1}`} {
		if rec := do(a.Handler(), "POST", "/v1/runs", bad); rec.Code != 400 {
			t.Errorf("%q: %d, want 400", bad, rec.Code)
		}
	}
	a.Dispatcher = nil
	if rec := do(a.Handler(), "POST", "/v1/runs", `{}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no dispatcher: %d", rec.Code)
	}
	noAuth := httptest.NewRecorder()
	a.Handler().ServeHTTP(noAuth, httptest.NewRequest("POST", "/v1/runs", strings.NewReader(`{}`)))
	if noAuth.Code != http.StatusUnauthorized {
		t.Fatalf("without a token: %d", noAuth.Code)
	}
}
