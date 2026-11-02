package store

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestInstallationsAreKeptAndFoundByRepository(t *testing.T) {
	s, ctx := open(t), context.Background()
	if _, err := s.InstallationOfRepo(ctx, "acme/web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("before anything: %v", err)
	}
	// A "selected" installation covers the listed repositories only.
	if err := s.PutInstallation(ctx, Installation{ID: 10, Account: "Acme", AccountType: "Organization", Selection: "selected"}, []string{"Acme/Web", "acme/api"}, true); err != nil {
		t.Fatal(err)
	}
	if id, err := s.InstallationOfRepo(ctx, "ACME/web"); err != nil || id != 10 {
		t.Errorf("a listed repo (any case): %d %v", id, err)
	}
	if _, err := s.InstallationOfRepo(ctx, "acme/other"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unlisted repo of a selected installation: %v", err)
	}
	// An "all" installation covers whatever its account owns, listed or not.
	if err := s.PutInstallation(ctx, Installation{ID: 20, Account: "Other", Selection: "all"}, nil, false); err != nil {
		t.Fatal(err)
	}
	if id, err := s.InstallationOfRepo(ctx, "other/anything"); err != nil || id != 20 {
		t.Errorf("an all installation: %d %v", id, err)
	}
	if _, err := s.InstallationOfRepo(ctx, "another/x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another owner: %v", err)
	}
	// Repositories come and go.
	if err := s.ChangeInstallationRepos(ctx, 10, "", []string{"acme/new"}, []string{"acme/api"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InstallationOfRepo(ctx, "acme/api"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a removed repo: %v", err)
	}
	if id, err := s.InstallationOfRepo(ctx, "acme/new"); err != nil || id != 10 {
		t.Errorf("an added repo: %d %v", id, err)
	}
	if err := s.ChangeInstallationRepos(ctx, 999, "", nil, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown installation: %v", err)
	}
	// A suspended installation does not count, and counts again afterwards.
	if err := s.SetInstallationSuspended(ctx, 10, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InstallationOfRepo(ctx, "acme/web"); !errors.Is(err, ErrNotFound) {
		t.Errorf("suspended: %v", err)
	}
	s.SetInstallationSuspended(ctx, 10, false)
	if id, _ := s.InstallationOfRepo(ctx, "acme/web"); id != 10 {
		t.Errorf("unsuspended: %d", id)
	}
	list, err := s.ListInstallations(ctx)
	if err != nil || len(list) != 2 || list[0].ID != 10 || !slices.Equal(list[0].Repos, []string{"acme/new", "acme/web"}) || list[1].Selection != "all" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	// Deleting takes its repositories with it.
	if err := s.DeleteInstallation(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InstallationOfRepo(ctx, "acme/web"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted: %v", err)
	}
	if err := s.DeleteInstallation(ctx, 10); err != nil {
		t.Errorf("deleting twice: %v", err)
	}
	// Putting again replaces the list when asked and keeps it otherwise.
	s.PutInstallation(ctx, Installation{ID: 30, Account: "z"}, []string{"z/a", "z/b"}, true)
	s.PutInstallation(ctx, Installation{ID: 30, Account: "z"}, nil, false)
	s.PutInstallation(ctx, Installation{ID: 30, Account: "z"}, []string{"z/c"}, true)
	list, _ = s.ListInstallations(ctx)
	for _, in := range list {
		if in.ID == 30 && !slices.Equal(in.Repos, []string{"z/c"}) {
			t.Errorf("replaced list = %v", in.Repos)
		}
	}
}
