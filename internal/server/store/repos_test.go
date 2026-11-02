package store

import (
	"context"
	"slices"
	"testing"
)

func TestNormalizeRepoPatterns(t *testing.T) {
	got, ok := NormalizeRepoPatterns([]string{"Acme/Web", " acme/web ", "Other/*"})
	if !ok || !slices.Equal(got, []string{"acme/web", "other/*"}) {
		t.Fatalf("got %v %v", got, ok)
	}
	for _, bad := range []string{"", "acme", "acme/", "/web", "*/web", "acme/we*b", "a/b/c", "acme/web;drop", "ac me/web", "*"} {
		if _, ok := NormalizeRepoPatterns([]string{bad}); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestRepoAllowed(t *testing.T) {
	for _, c := range []struct {
		patterns []string
		repo     string
		want     bool
	}{
		{nil, "any/thing", true},
		{[]string{"acme/web"}, "Acme/Web", true},
		{[]string{"acme/web"}, "acme/api", false},
		{[]string{"acme/*"}, "acme/api", true},
		{[]string{"acme/*"}, "acme-evil/api", false},
		{[]string{"acme/*"}, "other/acme", false},
		{[]string{"acme/web", "other/*"}, "other/x", true},
	} {
		if got := RepoAllowed(c.patterns, c.repo); got != c.want {
			t.Errorf("%v %s: %v, want %v", c.patterns, c.repo, got, c.want)
		}
	}
}

func TestListRunsAllowOnlyReturnsTheRunsOfTheCoveredRepositories(t *testing.T) {
	s, ctx := open(t), context.Background()
	mk := func(i int, repo string) {
		ev, _, err := s.RecordEvent(ctx, Event{DeliveryID: repo + string(rune('a'+i)), Kind: "push", Repo: repo, Payload: []byte("{}")})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.CreateRun(ctx, Run{EventID: ev.ID, Repo: repo, WorkflowPath: "w", Event: "push", SHA: "s", WorkflowText: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	mk(0, "Acme/web")
	mk(1, "acme/api")
	mk(2, "acme_x/api") // an underscore is not a wildcard
	mk(3, "acmeX/api")
	mk(4, "other/web")
	repos := func(f RunFilter) []string {
		runs, err := s.ListRuns(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range runs {
			out = append(out, r.Repo)
		}
		slices.Sort(out)
		return out
	}
	if got := repos(RunFilter{Allow: []string{"acme/*"}}); !slices.Equal(got, []string{"Acme/web", "acme/api"}) {
		t.Errorf("acme/*: %v", got)
	}
	if got := repos(RunFilter{Allow: []string{"acme_x/*"}}); !slices.Equal(got, []string{"acme_x/api"}) {
		t.Errorf("acme_x/*: %v (_ must not match any character)", got)
	}
	if got := repos(RunFilter{Allow: []string{"acme/web", "other/*"}}); !slices.Equal(got, []string{"Acme/web", "other/web"}) {
		t.Errorf("web and other: %v", got)
	}
	if got := repos(RunFilter{Allow: []string{"acme/*"}, Repo: "other/web"}); len(got) != 0 {
		t.Errorf("a repo outside the list was returned: %v", got)
	}
	if got := repos(RunFilter{}); len(got) != 5 {
		t.Errorf("no list: %v", got)
	}
}

func TestATokensRepositoriesAreKept(t *testing.T) {
	s, ctx := open(t), context.Background()
	made, err := s.CreateToken(ctx, "dash", RoleRead, "hhh", []string{"acme/*", "other/web"})
	if err != nil || !slices.Equal(made.Repos, []string{"acme/*", "other/web"}) {
		t.Fatalf("made = %+v, %v", made, err)
	}
	got, err := s.LookupToken(ctx, "hhh")
	if err != nil || !slices.Equal(got.Repos, made.Repos) {
		t.Fatalf("looked up = %+v, %v", got, err)
	}
	all, _ := s.ListTokens(ctx)
	if len(all) != 1 || !slices.Equal(all[0].Repos, made.Repos) {
		t.Fatalf("listed = %+v", all)
	}
	free, _ := s.CreateToken(ctx, "free", RoleRead, "fff", nil)
	if len(free.Repos) != 0 {
		t.Fatalf("an unlimited token has repos %v", free.Repos)
	}
}
