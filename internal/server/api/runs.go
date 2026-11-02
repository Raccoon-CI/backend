package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"raccoonci/internal/server/intake"
	"raccoonci/internal/server/store"
)

// dispatchRun is POST /v1/runs: start a workflow by hand.
func (a *API) dispatchRun(w http.ResponseWriter, r *http.Request) {
	if a.Dispatcher == nil {
		fail(w, http.StatusServiceUnavailable, "starting runs by hand is not available")
		return
	}
	var req intake.DispatchRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "the body must be a JSON object with repo, ref, workflow and inputs: "+err.Error())
		return
	}
	req.Actor = "api"
	run, err := a.Dispatcher.Dispatch(r.Context(), req)
	var re *intake.RequestError
	switch {
	case errors.As(err, &re):
		fail(w, re.Status, re.Msg)
	case err != nil:
		fail(w, http.StatusBadGateway, "could not start the run: "+err.Error())
	default:
		run.WorkflowText = ""
		writeJSON(w, http.StatusCreated, toRun(run))
	}
}

// cancelRun is POST /v1/runs/{id}/cancel: a queued run is ended at once, a
// running one is told to stop (it ends a moment later, as cancelled).
func (a *API) cancelRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	run, err := a.Store.GetRun(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "no such run")
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, "could not read the run")
		return
	}
	if cancelled, err := a.Store.CancelQueued(r.Context(), id); err != nil {
		fail(w, http.StatusInternalServerError, "could not cancel the run")
		return
	} else if cancelled {
		writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
		return
	}
	if a.Canceller != nil && a.Canceller.Cancel(id) {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
		return
	}
	fail(w, http.StatusConflict, "run "+strconv.FormatInt(id, 10)+" is "+run.Status+": only a queued or running run can be cancelled")
}

// rerunRun is POST /v1/runs/{id}/rerun: a new run like a finished one.
func (a *API) rerunRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if a.Dispatcher == nil {
		fail(w, http.StatusServiceUnavailable, "starting runs is not available")
		return
	}
	run, err := a.Dispatcher.Rerun(r.Context(), id)
	var re *intake.RequestError
	switch {
	case errors.As(err, &re):
		fail(w, re.Status, re.Msg)
	case err != nil:
		fail(w, http.StatusInternalServerError, "could not start the run")
	default:
		run.WorkflowText = ""
		writeJSON(w, http.StatusCreated, toRun(run))
	}
}

type jobJSON struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (a *API) listJobs(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := a.Store.GetRun(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "no such run")
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, "could not read the run")
		return
	}
	jobs, err := a.Store.ListJobs(r.Context(), id)
	if err != nil {
		fail(w, http.StatusInternalServerError, "could not read the jobs")
		return
	}
	out := struct {
		Jobs []jobJSON `json:"jobs"`
	}{Jobs: make([]jobJSON, 0, len(jobs))}
	for _, j := range jobs {
		out.Jobs = append(out.Jobs, jobJSON{ID: j.ID, Name: j.Name, Status: j.Status, Conclusion: j.Conclusion,
			StartedAt: timePtr(j.StartedAt), CompletedAt: timePtr(j.CompletedAt)})
	}
	writeJSON(w, http.StatusOK, out)
}

const (
	defaultLogLimit = 500
	maxLogLimit     = 5000
	streamPoll      = 300 * time.Millisecond
	streamKeepAlive = 15 * time.Second
)

type lineJSON struct {
	ID    int64           `json:"id"`
	At    time.Time       `json:"at"`
	Event json.RawMessage `json:"event"`
}

func toLine(l store.LogLine) lineJSON {
	return lineJSON{ID: l.ID, At: l.At, Event: json.RawMessage(l.Data)}
}

// readLog is GET /v1/runs/{id}/log. A plain request returns a page of the
// log after a position; a request that accepts text/event-stream gets the log
// as it grows until the run ends.
func (a *API) readLog(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	var after int64
	cursor := q.Get("after")
	if cursor == "" {
		cursor = r.Header.Get("Last-Event-ID") // an EventSource reconnecting
	}
	if cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || n < 0 {
			fail(w, http.StatusBadRequest, "after must be a log line id")
			return
		}
		after = n
	}
	limit := defaultLogLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLogLimit {
			fail(w, http.StatusBadRequest, "limit must be a number from 1 to "+strconv.Itoa(maxLogLimit))
			return
		}
		limit = n
	}
	if first, err := a.Store.GetRun(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "no such run")
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, "could not read the run")
		return
	} else if first.LogPurged {
		fail(w, http.StatusGone, "the log of this run was deleted after the retention time")
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		a.streamLog(w, r, id, after, limit)
		return
	}
	// The run's state is read before its lines: a run that is completed has
	// written all of them, so fewer lines than the limit then means the end.
	run, err := a.Store.GetRun(r.Context(), id)
	if err != nil {
		fail(w, http.StatusInternalServerError, "could not read the run")
		return
	}
	lines, err := a.Store.ReadLog(r.Context(), id, after, limit)
	if err != nil {
		fail(w, http.StatusInternalServerError, "could not read the log")
		return
	}
	out := struct {
		Lines     []lineJSON `json:"lines"`
		NextAfter int64      `json:"next_after"`
		// Done is true when the run has ended and nothing is left to read after next_after.
		Done bool `json:"done"`
	}{Lines: make([]lineJSON, 0, len(lines)), NextAfter: after, Done: run.Status == store.RunCompleted && len(lines) < limit}
	for _, l := range lines {
		out.Lines = append(out.Lines, toLine(l))
		out.NextAfter = l.ID
	}
	writeJSON(w, http.StatusOK, out)
}

// streamLog sends the log as server-sent events: one event per line, with the
// line's id as the event id (so a reconnecting client resumes), and an "end"
// event when the run is over.
func (a *API) streamLog(w http.ResponseWriter, r *http.Request, id, after int64, limit int) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, "streaming is not supported here")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // a reverse proxy must not hold it back
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	ctx := r.Context()
	lastSent := time.Now()
	for {
		run, err := a.Store.GetRun(ctx, id)
		if err != nil {
			return
		}
		lines, err := a.Store.ReadLog(ctx, id, after, limit)
		if err != nil {
			return
		}
		for _, l := range lines {
			b, _ := json.Marshal(toLine(l))
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", l.ID, b)
			after = l.ID
		}
		if len(lines) > 0 {
			flusher.Flush()
			lastSent = time.Now()
			if len(lines) == limit {
				continue // more may be waiting
			}
		}
		if run.Status == store.RunCompleted && len(lines) < limit {
			fmt.Fprintf(w, "event: end\ndata: {\"conclusion\":%q}\n\n", run.Conclusion)
			flusher.Flush()
			return
		}
		if time.Since(lastSent) > streamKeepAlive {
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
			lastSent = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(streamPoll):
		}
	}
}
