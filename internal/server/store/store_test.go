package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *SQLite {
	t.Helper()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestEventIsStoredOncePerDelivery(t *testing.T) {
	s, ctx := open(t), context.Background()
	first, created, err := s.RecordEvent(ctx, Event{DeliveryID: "d1", Kind: "push", Repo: "o/r", Payload: []byte(`{"a":1}`)})
	if err != nil || !created {
		t.Fatalf("first: created=%v err=%v", created, err)
	}
	if first.Status != EventReceived || first.ReceivedAt.IsZero() || string(first.Payload) != `{"a":1}` {
		t.Fatalf("stored event = %+v", first)
	}
	again, created, err := s.RecordEvent(ctx, Event{DeliveryID: "d1", Kind: "push", Repo: "o/r", Payload: []byte(`{"a":2}`)})
	if err != nil || created {
		t.Fatalf("redelivery: created=%v err=%v", created, err)
	}
	if again.ID != first.ID || string(again.Payload) != `{"a":1}` {
		t.Fatalf("redelivery returned %+v, want the stored event", again)
	}
}

func TestReposAreThoseAnEventNamed(t *testing.T) {
	s, ctx := open(t), context.Background()
	for i, e := range []Event{
		{DeliveryID: "a", Repo: "o/web", InstallationID: 3},
		{DeliveryID: "b", Repo: "o/web", InstallationID: 9},
		{DeliveryID: "c", Repo: "o/web"}, // a delivery without an installation does not hide it
		{DeliveryID: "d", Repo: "o/api"},
	} {
		e.Kind, e.Payload = "push", []byte("{}")
		if _, _, err := s.RecordEvent(ctx, e); err != nil {
			t.Fatal(i, err)
		}
	}
	got, err := s.Repos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (RepoRef{"o/api", 0}) || got[1] != (RepoRef{"o/web", 9}) {
		t.Fatalf("repos = %+v", got)
	}
}

func TestPendingEventsAreTheUnfinishedOnes(t *testing.T) {
	s, ctx := open(t), context.Background()
	a, _, _ := s.RecordEvent(ctx, Event{DeliveryID: "a", Kind: "push", Repo: "o/r", Payload: []byte("{}")})
	b, _, _ := s.RecordEvent(ctx, Event{DeliveryID: "b", Kind: "push", Repo: "o/r", Payload: []byte("{}")})
	if err := s.FinishEvent(ctx, a.ID, EventHandled, "1 run"); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingEvents(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != b.ID {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	got, _ := s.GetEvent(ctx, a.ID)
	if got.Status != EventHandled || got.Note != "1 run" {
		t.Fatalf("finished event = %+v", got)
	}
	if err := s.FinishEvent(ctx, 999, EventHandled, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finishing a missing event: %v", err)
	}
	if _, err := s.GetEvent(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("getting a missing event: %v", err)
	}
}

func TestRunIsCreatedOncePerEventAndWorkflow(t *testing.T) {
	s, ctx := open(t), context.Background()
	ev, _, _ := s.RecordEvent(ctx, Event{DeliveryID: "d", Kind: "push", Repo: "o/r", Payload: []byte("{}")})
	run := Run{EventID: ev.ID, Repo: "o/r", WorkflowPath: ".github/workflows/ci.yml", WorkflowName: "CI", Event: "push",
		Ref: "refs/heads/main", SHA: "abc", Fork: true, PullRequest: 7, WorkflowText: "on: push\n"}
	first, created, err := s.CreateRun(ctx, run)
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	if first.Status != RunQueued || !first.Fork || first.PullRequest != 7 || first.WorkflowText != "on: push\n" {
		t.Fatalf("stored run = %+v", first)
	}
	again, created, err := s.CreateRun(ctx, run)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("second create: id=%d created=%v err=%v", again.ID, created, err)
	}
	other := run
	other.WorkflowPath = ".github/workflows/lint.yml"
	if _, created, err := s.CreateRun(ctx, other); err != nil || !created {
		t.Fatalf("another workflow of the same event: created=%v err=%v", created, err)
	}
}

func TestListRunsNewestFirstWithPagingAndFilter(t *testing.T) {
	s, ctx := open(t), context.Background()
	ev, _, _ := s.RecordEvent(ctx, Event{DeliveryID: "d", Kind: "push", Repo: "o/r", Payload: []byte("{}")})
	for i, repo := range []string{"o/a", "o/b", "o/a", "o/b", "o/a"} {
		_, _, err := s.CreateRun(ctx, Run{EventID: ev.ID, Repo: repo, WorkflowPath: "w" + string(rune('0'+i)), Event: "push", SHA: "s", WorkflowText: "x"})
		if err != nil {
			t.Fatal(err)
		}
	}
	all, _ := s.ListRuns(ctx, RunFilter{})
	if len(all) != 5 || all[0].ID <= all[4].ID {
		t.Fatalf("all = %d runs, ids %d..%d, want 5 newest first", len(all), all[0].ID, all[len(all)-1].ID)
	}
	if all[0].WorkflowText != "" {
		t.Fatal("a list must leave the workflow text out")
	}
	onlyA, _ := s.ListRuns(ctx, RunFilter{Repo: "o/a"})
	if len(onlyA) != 3 {
		t.Fatalf("repo filter returned %d, want 3", len(onlyA))
	}
	page, _ := s.ListRuns(ctx, RunFilter{Before: all[1].ID, Limit: 2})
	if len(page) != 2 || page[0].ID != all[2].ID || page[1].ID != all[3].ID {
		t.Fatalf("page = %+v", page)
	}
	if _, err := s.GetRun(ctx, 12345); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing run: %v", err)
	}
}

func newRun(t *testing.T, s *SQLite, name string) Run {
	t.Helper()
	ev, _, _ := s.RecordEvent(context.Background(), Event{DeliveryID: "d-" + name, Kind: "push", Repo: "o/r", Payload: []byte("{}")})
	r, _, err := s.CreateRun(context.Background(), Run{EventID: ev.ID, Repo: "o/r", WorkflowPath: name, Event: "push", SHA: "s", WorkflowText: "x", Inputs: `{"a":"b"}`})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestClaimRunTakesTheOldestQueuedOnceAndFinishEndsIt(t *testing.T) {
	s, ctx := open(t), context.Background()
	if _, ok, err := s.ClaimRun(ctx); ok || err != nil {
		t.Fatalf("nothing queued: ok=%v err=%v", ok, err)
	}
	a, b := newRun(t, s, "a"), newRun(t, s, "b")
	got, ok, err := s.ClaimRun(ctx)
	if err != nil || !ok || got.ID != a.ID || got.Status != RunInProgress || got.WorkflowText != "x" || got.Inputs != `{"a":"b"}` {
		t.Fatalf("first claim = %+v ok=%v err=%v", got, ok, err)
	}
	got, _, _ = s.ClaimRun(ctx)
	if got.ID != b.ID {
		t.Fatalf("second claim got run %d, want %d: a claimed run must not be claimed again", got.ID, b.ID)
	}
	if _, ok, _ := s.ClaimRun(ctx); ok {
		t.Fatal("a third claim found a run")
	}
	if err := s.FinishRun(ctx, a.ID, "success"); err != nil {
		t.Fatal(err)
	}
	done, _ := s.GetRun(ctx, a.ID)
	if done.Status != RunCompleted || done.Conclusion != "success" {
		t.Fatalf("finished run = %+v", done)
	}
	if err := s.FinishRun(ctx, a.ID, "failure"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a run that is not in progress cannot be finished again: %v", err)
	}
}

func TestCancelQueuedOnlyEndsARunThatHasNotStarted(t *testing.T) {
	s, ctx := open(t), context.Background()
	queued, started := newRun(t, s, "q"), newRun(t, s, "s")
	s.db.Exec(`UPDATE runs SET status = ? WHERE id = ?`, RunInProgress, started.ID)
	ok, err := s.CancelQueued(ctx, queued.ID)
	if err != nil || !ok {
		t.Fatalf("queued: ok=%v err=%v", ok, err)
	}
	got, _ := s.GetRun(ctx, queued.ID)
	if got.Status != RunCompleted || got.Conclusion != "cancelled" {
		t.Fatalf("run = %+v", got)
	}
	if ok, _ := s.CancelQueued(ctx, queued.ID); ok {
		t.Fatal("a run was cancelled twice")
	}
	if ok, _ := s.CancelQueued(ctx, started.ID); ok {
		t.Fatal("a run in progress was cancelled as if it had not started")
	}
	if ok, _ := s.CancelQueued(ctx, 999); ok {
		t.Fatal("a missing run was cancelled")
	}
	if _, ok, _ := s.ClaimRun(ctx); ok {
		t.Fatal("a cancelled run was claimed")
	}
}

func TestARerunRemembersWhatItRepeats(t *testing.T) {
	s, ctx := open(t), context.Background()
	first := newRun(t, s, "w")
	ev, _, _ := s.RecordEvent(ctx, Event{DeliveryID: "rerun-1", Kind: "rerun", Repo: "o/r", Payload: []byte("{}")})
	again, created, err := s.CreateRun(ctx, Run{EventID: ev.ID, Repo: "o/r", WorkflowPath: "w", Event: "push", SHA: "s", WorkflowText: "x", RerunOf: first.ID})
	if err != nil || !created || again.RerunOf != first.ID {
		t.Fatalf("rerun = %+v created=%v err=%v", again, created, err)
	}
	if first.RerunOf != 0 {
		t.Fatalf("an original run has RerunOf %d", first.RerunOf)
	}
}

func TestSecretsAreReplacedByNameAndKeepTheirCreationTime(t *testing.T) {
	s, ctx := open(t), context.Background()
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return t0 }
	if err := s.PutSecret(ctx, Secret{Scope: "repo:acme/web", Name: "TOKEN", Kind: KindSecret, KeyID: "1", Value: []byte{1, 2, 3}}); err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return t0.Add(time.Hour) }
	if err := s.PutSecret(ctx, Secret{Scope: "repo:acme/web", Name: "TOKEN", Kind: KindSecret, KeyID: "2", Value: []byte{9}}); err != nil {
		t.Fatal(err)
	}
	s.PutSecret(ctx, Secret{Scope: "repo:acme/web", Name: "MODE", Kind: KindVar, Value: []byte("fast")})
	s.PutSecret(ctx, Secret{Scope: "org:acme", Name: "TOKEN", Kind: KindSecret, KeyID: "1", Value: []byte{7}})
	got, err := s.GetSecret(ctx, "repo:acme/web", "TOKEN")
	if err != nil || got.KeyID != "2" || len(got.Value) != 1 || !got.CreatedAt.Equal(t0) || !got.UpdatedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("secret = %+v, %v", got, err)
	}
	list, _ := s.ListSecrets(ctx, "repo:acme/web")
	if len(list) != 2 || list[0].Name != "MODE" || list[1].Name != "TOKEN" {
		t.Fatalf("list = %+v: only that scope's, by name", list)
	}
	if ok, _ := s.DeleteSecret(ctx, "repo:acme/web", "MODE"); !ok {
		t.Fatal("not deleted")
	}
	if ok, _ := s.DeleteSecret(ctx, "repo:acme/web", "MODE"); ok {
		t.Fatal("deleted twice")
	}
	if _, err := s.GetSecret(ctx, "repo:acme/web", "MODE"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted secret: %v", err)
	}
}

func TestTokensAreFoundByHashUntilRevoked(t *testing.T) {
	s, ctx := open(t), context.Background()
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	made, err := s.CreateToken(ctx, "ci bot", RoleRead, "aaa")
	if err != nil || made.Role != RoleRead || made.Name != "ci bot" || !made.LastUsedAt.IsZero() {
		t.Fatalf("made = %+v, %v", made, err)
	}
	if _, err := s.CreateToken(ctx, "again", RoleAdmin, "aaa"); err == nil {
		t.Fatal("two tokens with one hash")
	}
	found, err := s.LookupToken(ctx, "aaa")
	if err != nil || found.ID != made.ID || found.LastUsedAt.IsZero() {
		t.Fatalf("found = %+v, %v", found, err)
	}
	if _, err := s.LookupToken(ctx, "bbb"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown token: %v", err)
	}
	now = now.Add(10 * time.Second) // within a minute of the last use: not written again
	s.LookupToken(ctx, "aaa")
	list, _ := s.ListTokens(ctx)
	if len(list) != 1 || !list[0].LastUsedAt.Equal(time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("list = %+v", list)
	}
	if ok, _ := s.RevokeToken(ctx, made.ID); !ok {
		t.Fatal("not revoked")
	}
	if _, err := s.LookupToken(ctx, "aaa"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a revoked token still works: %v", err)
	}
	if ok, _ := s.RevokeToken(ctx, made.ID); ok {
		t.Fatal("revoked twice")
	}
	list, _ = s.ListTokens(ctx)
	if len(list) != 1 || list[0].RevokedAt.IsZero() {
		t.Fatalf("a revoked token must stay in the list: %+v", list)
	}
}

func TestOnlyOneOfConcurrentClaimersGetsARun(t *testing.T) {
	s, ctx := open(t), context.Background()
	newRun(t, s, "only")
	results := make(chan bool, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, ok, err := s.ClaimRun(ctx)
			if err != nil {
				t.Error(err)
			}
			results <- ok
		}()
	}
	won := 0
	for i := 0; i < 8; i++ {
		if <-results {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d claimers got the run, want exactly 1", won)
	}
}

func TestInterruptEndsWhatWasInProgress(t *testing.T) {
	s, ctx := open(t), context.Background()
	running, queued := newRun(t, s, "running"), newRun(t, s, "queued")
	s.ClaimRun(ctx) // running
	j, _ := s.StartJob(ctx, running.ID, "build")
	n, err := s.InterruptRuns(ctx, "failure")
	if err != nil || n != 1 {
		t.Fatalf("interrupted %d, %v", n, err)
	}
	got, _ := s.GetRun(ctx, running.ID)
	if got.Status != RunCompleted || got.Conclusion != "failure" {
		t.Fatalf("run = %+v", got)
	}
	jobs, _ := s.ListJobs(ctx, running.ID)
	if len(jobs) != 1 || jobs[0].ID != j.ID || jobs[0].Status != RunCompleted || jobs[0].Conclusion != "failure" || jobs[0].CompletedAt.IsZero() {
		t.Fatalf("jobs = %+v", jobs)
	}
	if q, _ := s.GetRun(ctx, queued.ID); q.Status != RunQueued {
		t.Fatalf("a queued run must stay queued: %+v", q)
	}
}

func TestJobsAreRecordedPerRunAndRestartable(t *testing.T) {
	s, ctx := open(t), context.Background()
	r := newRun(t, s, "w")
	j, err := s.StartJob(ctx, r.ID, "test (1.22)")
	if err != nil || j.Status != RunInProgress || j.StartedAt.IsZero() || !j.CompletedAt.IsZero() {
		t.Fatalf("started job = %+v, %v", j, err)
	}
	if err := s.FinishJob(ctx, j.ID, "failure"); err != nil {
		t.Fatal(err)
	}
	again, _ := s.StartJob(ctx, r.ID, "test (1.22)") // the same instance run again
	if again.ID != j.ID || again.Status != RunInProgress || again.Conclusion != "" || !again.CompletedAt.IsZero() {
		t.Fatalf("restarted job = %+v", again)
	}
	s.StartJob(ctx, r.ID, "lint")
	jobs, _ := s.ListJobs(ctx, r.ID)
	if len(jobs) != 2 || jobs[1].Name != "lint" {
		t.Fatalf("jobs = %+v", jobs)
	}
	if err := s.FinishJob(ctx, 999, "success"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finishing a missing job: %v", err)
	}
}

func TestLogIsReadInOrderFromAPosition(t *testing.T) {
	s, ctx := open(t), context.Background()
	r, other := newRun(t, s, "w"), newRun(t, s, "x")
	var lines []LogLine
	for _, d := range []string{"one", "two", "three"} {
		lines = append(lines, LogLine{Data: d})
	}
	if err := s.AppendLog(ctx, r.ID, lines); err != nil {
		t.Fatal(err)
	}
	s.AppendLog(ctx, other.ID, []LogLine{{Data: "not mine"}})
	s.AppendLog(ctx, r.ID, []LogLine{{Data: "four"}})
	all, _ := s.ReadLog(ctx, r.ID, 0, 0)
	if len(all) != 4 || all[0].Data != "one" || all[3].Data != "four" || all[0].At.IsZero() {
		t.Fatalf("log = %+v", all)
	}
	rest, _ := s.ReadLog(ctx, r.ID, all[1].ID, 0)
	if len(rest) != 2 || rest[0].Data != "three" {
		t.Fatalf("after the second line: %+v", rest)
	}
	limited, _ := s.ReadLog(ctx, r.ID, 0, 2)
	if len(limited) != 2 {
		t.Fatalf("limit ignored: %d lines", len(limited))
	}
	if err := s.AppendLog(ctx, r.ID, nil); err != nil {
		t.Fatalf("appending nothing: %v", err)
	}
}

func TestTimestampsSurviveReopening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 1, 12, 0, 0, 123_000_000, time.UTC)
	s.Now = func() time.Time { return at }
	ev, _, _ := s.RecordEvent(context.Background(), Event{DeliveryID: "d", Kind: "push", Repo: "o/r", Payload: []byte("{}")})
	s.Close()

	s, err = OpenSQLite(path) // migrations must be a no-op the second time
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.GetEvent(context.Background(), ev.ID)
	if err != nil || !got.ReceivedAt.Equal(at) {
		t.Fatalf("received at = %v (%v), want %v", got.ReceivedAt, err, at)
	}
}

func TestADatabaseFromANewerServerIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (9999, 0)`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := OpenSQLite(path); err == nil {
		t.Fatal("opened a database newer than the server")
	}
}
