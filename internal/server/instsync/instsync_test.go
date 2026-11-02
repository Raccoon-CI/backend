package instsync

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Raccoon-CI/raccoonci/internal/server/github"
	"github.com/Raccoon-CI/raccoonci/internal/server/store"
)

type fakeGH struct {
	installations []github.AppInstallation
	repos         map[int64][]string
	listErr       error
	repoErr       map[int64]error
}

func (f *fakeGH) Installations(context.Context) ([]github.AppInstallation, error) {
	return f.installations, f.listErr
}

func (f *fakeGH) InstallationRepos(_ context.Context, id int64) ([]string, error) {
	return f.repos[id], f.repoErr[id]
}

func newStore(t *testing.T) *store.SQLite {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestTheStoreFollowsWhatGitHubLists(t *testing.T) {
	ctx, st := context.Background(), newStore(t)
	gh := &fakeGH{
		installations: []github.AppInstallation{
			{ID: 1, Account: "Acme", AccountType: "Organization", Selection: "all"},
			{ID: 2, Account: "Solo", AccountType: "User", Selection: "selected"},
		},
		repos: map[int64][]string{1: {"Acme/web", "Acme/api"}, 2: {"Solo/tool"}},
	}
	s := &Syncer{Store: st, GH: gh}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	list, _ := st.ListInstallations(ctx)
	if len(list) != 2 || !slices.Equal(list[0].Repos, []string{"acme/api", "acme/web"}) || list[1].Account != "Solo" {
		t.Fatalf("after the first sync: %+v", list)
	}
	// Something was missed: a repository left, an installation was removed,
	// another was suspended.
	gh.installations = []github.AppInstallation{{ID: 1, Account: "Acme", Selection: "all", Suspended: true}}
	gh.repos[1] = []string{"Acme/web"}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	list, _ = st.ListInstallations(ctx)
	if len(list) != 1 || !list[0].Suspended || !slices.Equal(list[0].Repos, []string{"acme/web"}) {
		t.Fatalf("after the second sync: %+v", list)
	}
}

func TestNothingIsForgottenWhenGitHubCannotBeAsked(t *testing.T) {
	ctx, st := context.Background(), newStore(t)
	st.PutInstallation(ctx, store.Installation{ID: 9, Account: "Acme"}, []string{"acme/web"}, true)
	s := &Syncer{Store: st, GH: &fakeGH{listErr: errors.New("github is down")}}
	if err := s.Sync(ctx); err == nil {
		t.Fatal("no error")
	}
	if list, _ := st.ListInstallations(ctx); len(list) != 1 {
		t.Fatalf("an outage forgot installations: %+v", list)
	}
}

func TestAnInstallationWhoseRepositoriesCannotBeReadKeepsItsOldList(t *testing.T) {
	ctx, st := context.Background(), newStore(t)
	st.PutInstallation(ctx, store.Installation{ID: 1, Account: "Acme", Selection: "selected"}, []string{"acme/old"}, true)
	gh := &fakeGH{
		installations: []github.AppInstallation{{ID: 1, Account: "Acme", Selection: "selected"}, {ID: 2, Account: "Other", Selection: "selected"}},
		repos:         map[int64][]string{2: {"other/x"}},
		repoErr:       map[int64]error{1: errors.New("token refused")},
	}
	err := (&Syncer{Store: st, GH: gh}).Sync(ctx)
	if err == nil {
		t.Fatal("the failure was not reported")
	}
	list, _ := st.ListInstallations(ctx)
	if len(list) != 2 || !slices.Equal(list[0].Repos, []string{"acme/old"}) || !slices.Equal(list[1].Repos, []string{"other/x"}) {
		t.Fatalf("list = %+v", list)
	}
}
