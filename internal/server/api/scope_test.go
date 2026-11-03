package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Raccoon-CI/backend/internal/server/store"
)

func ctx() context.Context { return context.Background() }

// limited makes a read token limited to some repositories, through the API.
func limited(t *testing.T, h http.Handler, repos ...string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": "dash", "role": "read", "repos": repos})
	rec := call(h, "POST", "/v1/tokens", token, string(body))
	var made struct {
		Token string   `json:"token"`
		Repos []string `json:"repos"`
	}
	json.Unmarshal(rec.Body.Bytes(), &made)
	if rec.Code != 201 || made.Token == "" {
		t.Fatalf("making the token: %d %s", rec.Code, rec.Body)
	}
	return made.Token
}

func repoNames(t *testing.T, rec interface{ Bytes() []byte }) []string {
	t.Helper()
	var out struct {
		Runs []struct{ Repo string } `json:"runs"`
	}
	if err := json.Unmarshal(rec.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range out.Runs {
		names = append(names, r.Repo)
	}
	return names
}

func TestALimitedTokenSeesOnlyTheRunsOfItsRepositories(t *testing.T) {
	_, h := setup(t) // runs 1 and 3 are acme/web, 2 is acme/api; event 1 is acme/web
	api := limited(t, h, "Acme/API")

	rec := call(h, "GET", "/v1/runs", api, "")
	if names := repoNames(t, rec.Body); rec.Code != 200 || len(names) != 1 || names[0] != "acme/api" {
		t.Fatalf("list: %d %v", rec.Code, names)
	}
	if rec := call(h, "GET", "/v1/runs?repo=acme/web", api, ""); rec.Code != 200 || len(repoNames(t, rec.Body)) != 0 {
		t.Errorf("asking for another repository: %d %s", rec.Code, rec.Body)
	}
	for _, path := range []string{"/v1/runs/2", "/v1/runs/2/jobs", "/v1/runs/2/log"} {
		if rec := call(h, "GET", path, api, ""); rec.Code != 200 {
			t.Errorf("%s of its own repository: %d %s", path, rec.Code, rec.Body)
		}
	}
	// Another repository's run does not exist as far as the token can tell:
	// the same answer as for a run that is not there.
	missing := call(h, "GET", "/v1/runs/999", api, "")
	for _, path := range []string{"/v1/runs/1", "/v1/runs/1/jobs", "/v1/runs/1/log", "/v1/runs/1/artifacts", "/v1/runs/1/artifacts/1/download", "/v1/runs/3"} {
		rec := call(h, "GET", path, api, "")
		if rec.Code != 404 || rec.Body.String() != missing.Body.String() {
			t.Errorf("%s of another repository: %d %s, want the answer for a missing run (%d %s)", path, rec.Code, rec.Body, missing.Code, missing.Body)
		}
	}
	if rec := call(h, "GET", "/v1/events/1", api, ""); rec.Code != 404 {
		t.Errorf("an event of another repository: %d", rec.Code)
	}
	// It stays a read token: nothing that changes something.
	for _, c := range [][3]string{{"POST", "/v1/runs", `{}`}, {"POST", "/v1/runs/2/cancel", ``}, {"POST", "/v1/runs/2/rerun", ``}, {"GET", "/v1/tokens", ``}, {"GET", "/v1/secrets?scope=org:acme", ``}} {
		if rec := call(h, c[0], c[1], api, c[2]); rec.Code != 403 {
			t.Errorf("%s %s: %d, want 403", c[0], c[1], rec.Code)
		}
	}
}

func TestUsageIsShownPerMonthAndALimitedTokenSeesOnlyItsRepositories(t *testing.T) {
	a, h := setup(t)
	a.Quota = Quota{RepoMinutes: 600, OwnerMinutes: 6000}
	day := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	a.Store.AddUsage(ctx(), "acme/web", day, 120, 1)
	a.Store.AddUsage(ctx(), "acme/web", day.AddDate(0, 0, 1), 30, 1)
	a.Store.AddUsage(ctx(), "acme/api", day, 600, 2)
	a.Store.AddUsage(ctx(), "acme/web", day.AddDate(0, -1, 0), 99999, 1) // October

	rec := call(h, "GET", "/v1/usage?month=2026-11", token, "")
	var out struct {
		Month string `json:"month"`
		Quota Quota  `json:"quota"`
		Repos []struct {
			Repo           string `json:"repo"`
			MachineSeconds int64  `json:"machine_seconds"`
			Runs           int    `json:"runs"`
		} `json:"repos"`
		Days []struct{ Day, Repo string } `json:"days"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 || out.Month != "2026-11" || out.Quota.RepoMinutes != 600 || len(out.Repos) != 2 || len(out.Days) != 3 ||
		out.Repos[0].Repo != "acme/api" || out.Repos[0].MachineSeconds != 600 || out.Repos[1].MachineSeconds != 150 || out.Repos[1].Runs != 2 {
		t.Fatalf("usage = %d %s", rec.Code, rec.Body)
	}
	lim := call(h, "GET", "/v1/usage?month=2026-11", limited(t, h, "acme/web"), "")
	if lim.Code != 200 || strings.Contains(lim.Body.String(), "acme/api") || !strings.Contains(lim.Body.String(), "acme/web") {
		t.Fatalf("a token limited to acme/web: %d %s", lim.Code, lim.Body)
	}
	for _, bad := range []string{"2026-13", "nov", "2026-1-1"} {
		if rec := call(h, "GET", "/v1/usage?month="+bad, token, ""); rec.Code != 400 {
			t.Errorf("month %q: %d", bad, rec.Code)
		}
	}
	if rec := call(h, "GET", "/v1/usage", "", ""); rec.Code != 401 {
		t.Errorf("no token: %d", rec.Code)
	}
}

func TestInstallationsAreListedAndClosedToLimitedTokens(t *testing.T) {
	a, h := setup(t)
	a.Store.PutInstallation(ctx(), store.Installation{ID: 5, Account: "Acme", AccountType: "Organization", Selection: "selected"}, []string{"acme/web"}, true)
	rec := call(h, "GET", "/v1/installations", token, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":5`) || !strings.Contains(rec.Body.String(), `"repos":["acme/web"]`) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "GET", "/v1/installations", limited(t, h, "acme/*"), ""); rec.Code != 403 {
		t.Errorf("a limited token: %d, want 403 (it would show repositories it may not see)", rec.Code)
	}
	if rec := call(h, "GET", "/v1/installations", "", ""); rec.Code != 401 {
		t.Errorf("no token: %d", rec.Code)
	}
}

func TestAnOwnerPatternCoversItsRepositoriesAndNoTokenSeesMoreThanItIsGiven(t *testing.T) {
	_, h := setup(t)
	acme := limited(t, h, "acme/*")
	if names := repoNames(t, call(h, "GET", "/v1/runs", acme, "").Body); len(names) != 3 {
		t.Errorf("acme/*: %v", names)
	}
	other := limited(t, h, "other/*")
	if rec := call(h, "GET", "/v1/runs", other, ""); len(repoNames(t, rec.Body)) != 0 {
		t.Errorf("other/*: %s", rec.Body)
	}
	if rec := call(h, "GET", "/v1/runs/1", other, ""); rec.Code != 404 {
		t.Errorf("other/* reading acme's run: %d", rec.Code)
	}
	// A token with no list, and the bootstrap token, see everything.
	free := call(h, "POST", "/v1/tokens", token, `{"name":"free","role":"read"}`)
	var made struct{ Token string }
	json.Unmarshal(free.Body.Bytes(), &made)
	for _, tok := range []string{made.Token, token} {
		if names := repoNames(t, call(h, "GET", "/v1/runs", tok, "").Body); len(names) != 3 {
			t.Errorf("an unlimited token sees %v", names)
		}
	}
}

func TestOnlyAReadTokenCanBeLimitedAndThePatternsAreChecked(t *testing.T) {
	_, h := setup(t)
	for _, c := range []struct{ body, what string }{
		{`{"name":"a","role":"admin","repos":["acme/web"]}`, "an admin limited to a repository"},
		{`{"name":"a","role":"read","repos":["acme"]}`, "a name without an owner"},
		{`{"name":"a","role":"read","repos":["*/web"]}`, "a wildcard owner"},
		{`{"name":"a","role":"read","repos":["acme/web; drop"]}`, "a pattern with junk"},
	} {
		if rec := call(h, "POST", "/v1/tokens", token, c.body); rec.Code != 400 {
			t.Errorf("%s: %d %s", c.what, rec.Code, rec.Body)
		}
	}
	rec := call(h, "POST", "/v1/tokens", token, `{"name":"a","role":"read","repos":["Acme/Web","acme/web"]}`)
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"repos":["acme/web"]`) {
		t.Errorf("repeats and case: %d %s", rec.Code, rec.Body)
	}
	// The list shows the limit, never the token.
	if list := call(h, "GET", "/v1/tokens", token, ""); !strings.Contains(list.Body.String(), `"repos":["acme/web"]`) {
		t.Errorf("listing: %s", list.Body)
	}
}
