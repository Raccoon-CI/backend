package intake

import (
	"context"
	"testing"
)

const created = `{"action":"created","installation":{"id":77,"account":{"login":"Acme","type":"Organization"},"repository_selection":"selected","suspended_at":null},"repositories":[{"full_name":"acme/web"},{"full_name":"acme/api"}]}`

func TestInstallationEventsAreKeptSoTheServerKnowsWhereTheAppIs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if rec := e.deliver("installation", "i1", []byte(created)); rec.Code != 202 {
		t.Fatalf("created: %d %s", rec.Code, rec.Body)
	}
	if id, err := e.st.InstallationOfRepo(ctx, "acme/web"); err != nil || id != 77 {
		t.Fatalf("after created: %d %v", id, err)
	}
	// Repositories added and removed.
	added := `{"action":"added","installation":{"id":77,"account":{"login":"Acme","type":"Organization"},"repository_selection":"selected"},"repositories_added":[{"full_name":"acme/new"}],"repositories_removed":[{"full_name":"acme/api"}]}`
	if rec := e.deliver("installation_repositories", "i2", []byte(added)); rec.Code != 202 {
		t.Fatalf("added: %d %s", rec.Code, rec.Body)
	}
	if id, _ := e.st.InstallationOfRepo(ctx, "acme/new"); id != 77 {
		t.Error("an added repo is not known")
	}
	if id, _ := e.st.InstallationOfRepo(ctx, "acme/api"); id != 0 {
		t.Error("a removed repo is still known")
	}
	// Suspended, then not.
	e.deliver("installation", "i3", []byte(`{"action":"suspend","installation":{"id":77,"account":{"login":"Acme"},"suspended_at":"2026-11-02T00:00:00Z"}}`))
	if id, _ := e.st.InstallationOfRepo(ctx, "acme/web"); id != 0 {
		t.Error("a suspended installation still counts")
	}
	e.deliver("installation", "i4", []byte(`{"action":"unsuspend","installation":{"id":77,"account":{"login":"Acme"}}}`))
	if id, _ := e.st.InstallationOfRepo(ctx, "acme/web"); id != 77 {
		t.Error("an unsuspended installation does not count")
	}
	// A redelivery changes nothing.
	e.deliver("installation", "i1", []byte(created))
	list, _ := e.st.ListInstallations(ctx)
	if len(list) != 1 || len(list[0].Repos) != 2 { // the redelivered created lists web and api again
		t.Fatalf("after a redelivery: %+v", list)
	}
	// Deleted.
	e.deliver("installation", "i5", []byte(`{"action":"deleted","installation":{"id":77,"account":{"login":"Acme"}}}`))
	if list, _ := e.st.ListInstallations(ctx); len(list) != 0 {
		t.Fatalf("after deleted: %+v", list)
	}
}

func TestInstallationEventsNeedASignatureAndAnInstallation(t *testing.T) {
	e := newEnv(t)
	if rec := e.deliverSigned("installation", "x", []byte(created), "sha256=00"); rec.Code != 401 {
		t.Errorf("a bad signature: %d", rec.Code)
	}
	if list, _ := e.st.ListInstallations(context.Background()); len(list) != 0 {
		t.Error("an unsigned delivery was kept")
	}
	if rec := e.deliver("installation", "y", []byte(`{"action":"created"}`)); rec.Code != 400 {
		t.Errorf("no installation: %d", rec.Code)
	}
}

func TestAHandStartedRunFindsTheInstallationFromWhatTheAppToldUs(t *testing.T) {
	e := newEnv(t, wf("ci.yml", "workflow_dispatch"))
	asked := 0
	e.h.Installation = func(context.Context, string) (int64, error) { asked++; return 999, nil }
	e.deliver("installation", "i1", []byte(created))
	got, err := e.h.installationFor(context.Background(), "acme/web")
	if err != nil || got != 77 || asked != 0 {
		t.Fatalf("installationFor = %d, %v (GitHub asked %d times)", got, err, asked)
	}
	// A repository the table does not know falls back to asking GitHub.
	got, err = e.h.installationFor(context.Background(), "acme/unknown")
	if err != nil || got != 999 || asked != 1 {
		t.Fatalf("fallback = %d, %v (asked %d)", got, err, asked)
	}
}
