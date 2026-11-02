package retention

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"raccoonci/internal/server/store"
)

func TestSweepDeletesOnlyLogsPastTheirTimeAndKeepsEverythingElse(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	t0 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	st.Now = func() time.Time { return t0 }
	ev, _, _ := st.RecordEvent(ctx, store.Event{DeliveryID: "d", Kind: "push", Repo: "o/r", Payload: []byte("{}")})
	// 250 finished runs: more than two batches, half of them recent.
	var ids []int64
	for i := 0; i < 250; i++ {
		r, _, err := st.CreateRun(ctx, store.Run{EventID: ev.ID, Repo: "o/r", WorkflowPath: "w" + time.Duration(i).String(), Event: "push", SHA: "s", WorkflowText: "x"})
		if err != nil {
			t.Fatal(err)
		}
		st.AppendLog(ctx, r.ID, []store.LogLine{{Data: "{}"}})
		ids = append(ids, r.ID)
	}
	for i, id := range ids {
		if i == 125 {
			st.Now = func() time.Time { return t0.Add(80 * 24 * time.Hour) }
		}
		if _, ok, _ := st.ClaimRun(ctx); !ok {
			t.Fatal("claim")
		}
		st.FinishRun(ctx, id, "success")
	}

	j := &Janitor{Store: st, LogKeep: 30 * 24 * time.Hour}
	runs, lines, err := j.Sweep(ctx, t0.Add(100*24*time.Hour)) // cutoff day 70: the first 125 ended on day 0, the rest on day 80
	if err != nil || runs != 125 || lines != 125 {
		t.Fatalf("Sweep = %d runs, %d lines, %v; want 125 and 125 across two batches", runs, lines, err)
	}
	if got, _ := st.GetRun(ctx, ids[0]); !got.LogPurged || got.Conclusion != "success" {
		t.Fatalf("the run stays, with its result: %+v", got)
	}
	if got, _ := st.GetRun(ctx, ids[200]); got.LogPurged {
		t.Fatal("a recent run lost its log")
	}
	if runs, _, _ := j.Sweep(ctx, t0.Add(100*24*time.Hour)); runs != 0 {
		t.Fatalf("a second sweep purged %d", runs)
	}

	keep := &Janitor{Store: st} // LogKeep 0: for ever
	if runs, _, err := keep.Sweep(ctx, t0.Add(1000*24*time.Hour)); err != nil || runs != 0 {
		t.Fatalf("with no retention time nothing may go: %d, %v", runs, err)
	}
}
