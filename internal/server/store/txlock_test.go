package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// A transaction that reads and then writes must not fail with "database is
// locked" because another connection committed in between (SQLITE_BUSY_SNAPSHOT
// in WAL mode, which busy_timeout does not retry). The executor used to stop
// for good when RequeueInterrupted met this at start-up, with runs being queued
// at the same moment.
func TestReadThenWriteTransactionsSurviveConcurrentWrites(t *testing.T) {
	s, ctx := open(t), context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // webhook deliveries arriving all the time
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			ev, _, err := s.RecordEvent(ctx, Event{DeliveryID: fmt.Sprintf("d%d", i), Kind: "push", Repo: "acme/web", Payload: []byte("{}")})
			if err != nil {
				t.Errorf("record: %v", err)
				return
			}
			if _, _, err := s.CreateRun(ctx, Run{EventID: ev.ID, Repo: "acme/web", WorkflowPath: "w", Event: "push", SHA: "s", WorkflowText: "x"}); err != nil {
				t.Errorf("create: %v", err)
				return
			}
		}
	}()
	for i := 0; i < 300; i++ {
		if _, _, err := s.RequeueInterrupted(ctx, 3, "failure"); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("RequeueInterrupted #%d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
}
