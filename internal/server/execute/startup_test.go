package execute

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Raccoon-CI/backend/internal/server/store"
)

// failingStart makes RequeueInterrupted, the first thing Run does, fail the
// first n times it is called.
type failingStart struct {
	store.Store
	left  atomic.Int32
	calls atomic.Int32
}

func (f *failingStart) RequeueInterrupted(ctx context.Context, maxAttempts int, conclusion string) (int, int, error) {
	f.calls.Add(1)
	if f.left.Add(-1) >= 0 {
		return 0, 0, errors.New("database is locked (517)")
	}
	return f.Store.RequeueInterrupted(ctx, maxAttempts, conclusion)
}

func TestTheExecutorSurvivesATroubledStart(t *testing.T) {
	e := newEnv(t)
	f := &failingStart{Store: e.st}
	f.left.Store(2)
	e.ex.Store, e.ex.StartRetry = f, time.Millisecond
	e.start(t)
	run := e.queue(t, "late", "on: push\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n", onRef("main"))
	if r := e.wait(t, run.ID); r.Conclusion != "success" {
		t.Fatalf("%q, after %d tries", r.Conclusion, f.calls.Load())
	}
	if f.calls.Load() != 3 {
		t.Fatalf("RequeueInterrupted was called %d times, want 3", f.calls.Load())
	}
}

func TestTheExecutorReturnsTheErrorWhenStartingKeepsFailing(t *testing.T) {
	e := newEnv(t)
	f := &failingStart{Store: e.st}
	f.left.Store(1000)
	e.ex.Store, e.ex.StartRetry = f, time.Millisecond
	done := make(chan error, 1)
	go func() { done <- e.ex.Run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil although it never got started")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not give up")
	}
	if f.calls.Load() != startTries {
		t.Fatalf("RequeueInterrupted was called %d times, want %d", f.calls.Load(), startTries)
	}
}

func TestTheRetriesAtStartEndWhenTheServerIsStopped(t *testing.T) {
	e := newEnv(t)
	f := &failingStart{Store: e.st}
	f.left.Store(1000)
	e.ex.Store, e.ex.StartRetry = f, time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.ex.Run(ctx) }()
	for f.calls.Load() < 1 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run kept waiting to retry after the server was stopped")
	}
}
