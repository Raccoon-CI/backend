package execute

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/Raccoon-CI/backend/internal/server/store"
	"github.com/Raccoon-CI/lib/pkg/dispatch"
	"github.com/Raccoon-CI/lib/pkg/instabox"
	"github.com/Raccoon-CI/lib/pkg/runevent"
)

// kindNames are what the log says for runevent.EventKind, which is a number in
// Go: a reader of the API should not need the Go constants.
var kindNames = map[runevent.EventKind]string{
	runevent.JobStarted:   "job_started",
	runevent.StepStarted:  "step_started",
	runevent.Output:       "output",
	runevent.Debug:        "debug",
	runevent.Notice:       "notice",
	runevent.Warning:      "warning",
	runevent.Error:        "error",
	runevent.GroupStart:   "group_start",
	runevent.GroupEnd:     "group_end",
	runevent.StepFinished: "step_finished",
	runevent.JobFinished:  "job_finished",
}

// logEvent is a log line as stored and served.
type logEvent struct {
	Kind       string            `json:"kind"`
	Time       time.Time         `json:"time"`
	Job        string            `json:"job,omitempty"`
	Step       string            `json:"step,omitempty"`
	Depth      int               `json:"depth,omitempty"`
	Text       string            `json:"text,omitempty"`
	Props      map[string]string `json:"props,omitempty"`
	Conclusion string            `json:"conclusion,omitempty"`
}

// EncodeEvent is how a runevent.Event is stored in a run's log.
func EncodeEvent(e runevent.Event) string {
	kind, ok := kindNames[e.Kind]
	if !ok {
		kind = "unknown"
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	b, _ := json.Marshal(logEvent{Kind: kind, Time: e.Time.UTC(), Job: e.Job, Step: e.Step, Depth: e.Depth, Text: e.Text,
		Props: e.Props, Conclusion: string(e.Conclusion)})
	return string(b)
}

// sink collects a run's events and writes them to the store in batches, so a
// chatty step does not mean a database write per line.
type sink struct {
	store store.Store
	run   int64
	log   *slog.Logger

	mu   sync.Mutex
	buf  []store.LogLine
	full chan struct{}
	stop chan struct{}
	done chan struct{}
}

const (
	flushEvery = 300 * time.Millisecond
	flushAt    = 500
)

func newSink(st store.Store, run int64, log *slog.Logger) *sink {
	s := &sink{store: st, run: run, log: log, full: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		t := time.NewTicker(flushEvery)
		defer t.Stop()
		for {
			select {
			case <-t.C:
			case <-s.full:
			case <-s.stop:
				return
			}
			s.flush()
		}
	}()
	return s
}

// Add queues an event.
func (s *sink) Add(e runevent.Event) {
	line := store.LogLine{At: e.Time, Data: EncodeEvent(e)}
	s.mu.Lock()
	s.buf = append(s.buf, line)
	n := len(s.buf)
	s.mu.Unlock()
	if n >= flushAt {
		select {
		case s.full <- struct{}{}:
		default:
		}
	}
}

func (s *sink) flush() {
	s.mu.Lock()
	lines := s.buf
	s.buf = nil
	s.mu.Unlock()
	if len(lines) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.store.AppendLog(ctx, s.run, lines); err != nil {
		s.log.Error("storing log lines failed; they are lost", "lines", len(lines), "err", err)
	}
}

// Close writes what is left and stops the writer.
func (s *sink) Close() {
	close(s.stop)
	<-s.done
	s.flush()
}

// Instabox runs jobs on InstaBox machines; it adapts instabox.Provider to Machine.
type Instabox struct {
	P *instabox.Provider
}

// Run implements Machine.
func (i Instabox) Run(ctx context.Context, name string, job dispatch.Job) (dispatch.Result, error) {
	return i.P.Run(ctx, name, job, nil)
}
