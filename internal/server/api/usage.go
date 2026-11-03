package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/Raccoon-CI/backend/internal/server/store"
)

// Quota is the monthly allowances of machine time, in minutes; 0 is none.
type Quota struct {
	RepoMinutes  int64 `json:"repo_minutes"`
	OwnerMinutes int64 `json:"owner_minutes"`
}

type usageRow struct {
	Day            string `json:"day,omitempty"`
	Repo           string `json:"repo"`
	MachineSeconds int64  `json:"machine_seconds"`
	Runs           int    `json:"runs"`
}

// getUsage shows the machine time used in a UTC month (?month=YYYY-MM, the
// current one by default): per day and repository, and per repository over the
// month. A token limited to repositories sees only theirs.
func (a *API) getUsage(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if v := r.URL.Query().Get("month"); v != "" {
		t, err := time.Parse("2006-01", v)
		if err != nil {
			fail(w, http.StatusBadRequest, "month must be like 2026-11")
			return
		}
		month = t
	}
	last := month.AddDate(0, 1, -1)
	rows, err := a.Store.ListUsage(r.Context(), month, last)
	if err != nil {
		fail(w, http.StatusInternalServerError, "could not read the usage")
		return
	}
	scope := scopeOf(r.Context())
	days := []usageRow{}
	totals := map[string]*usageRow{}
	for _, u := range rows {
		if !store.RepoAllowed(scope, u.Repo) {
			continue
		}
		days = append(days, usageRow{Day: u.Day, Repo: u.Repo, MachineSeconds: u.MachineSeconds, Runs: u.Runs})
		t := totals[u.Repo]
		if t == nil {
			t = &usageRow{Repo: u.Repo}
			totals[u.Repo] = t
		}
		t.MachineSeconds += u.MachineSeconds
		t.Runs += u.Runs
	}
	byRepo := make([]usageRow, 0, len(totals))
	for _, t := range totals {
		byRepo = append(byRepo, *t)
	}
	sort.Slice(byRepo, func(i, j int) bool { return byRepo[i].Repo < byRepo[j].Repo })
	writeJSON(w, http.StatusOK, map[string]any{"month": month.Format("2006-01"), "quota": a.Quota, "repos": byRepo, "days": days})
}
