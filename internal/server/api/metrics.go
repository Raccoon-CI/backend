package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// metrics answers in Prometheus' text format, for a monitor to scrape with the
// bearer token (admin: the counts say what the server is working on). It says
// the same things /readyz does (one gauge per check) plus how many runs there
// are in each state and how long the oldest queued one has waited, which is the
// number to alert on when runs are not being taken.
func (a *API) metrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var b strings.Builder

	b.WriteString("# HELP raccoonci_build_info The server's version.\n# TYPE raccoonci_build_info gauge\n")
	fmt.Fprintf(&b, "raccoonci_build_info{version=%s} 1\n", quote(a.Version))

	b.WriteString("# HELP raccoonci_check Whether a readiness check passes (1) or fails (0); see /readyz.\n# TYPE raccoonci_check gauge\n")
	for _, c := range a.Checks {
		v := 1
		if err := c.Run(ctx); err != nil {
			v = 0
			a.log().Warn("metrics: check failing", "check", c.Name, "err", err)
		}
		fmt.Fprintf(&b, "raccoonci_check{check=%s} %d\n", quote(c.Name), v)
	}

	stats, err := a.Store.RunStats(ctx)
	if err != nil {
		a.log().Error("metrics: counting runs", "err", err)
		http.Error(w, "counting runs failed", http.StatusInternalServerError)
		return
	}
	b.WriteString("# HELP raccoonci_runs Runs the database holds, by status and conclusion (empty until completed); the log retention does not remove runs.\n# TYPE raccoonci_runs gauge\n")
	var statuses []string
	for s := range stats.Counts {
		statuses = append(statuses, s)
	}
	sort.Strings(statuses)
	for _, s := range statuses {
		var conclusions []string
		for c := range stats.Counts[s] {
			conclusions = append(conclusions, c)
		}
		sort.Strings(conclusions)
		for _, c := range conclusions {
			fmt.Fprintf(&b, "raccoonci_runs{status=%s,conclusion=%s} %d\n", quote(s), quote(c), stats.Counts[s][c])
		}
	}
	b.WriteString("# HELP raccoonci_oldest_queued_run_age_seconds How long the oldest queued run has waited (0 if none is queued).\n# TYPE raccoonci_oldest_queued_run_age_seconds gauge\n")
	age := 0.0
	if !stats.OldestQueued.IsZero() {
		age = time.Since(stats.OldestQueued).Seconds()
	}
	fmt.Fprintf(&b, "raccoonci_oldest_queued_run_age_seconds %.0f\n", age)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write([]byte(b.String()))
}

// quote writes a label value as the text format wants it: in double quotes,
// with backslash, quote and newline escaped.
func quote(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
	return `"` + s + `"`
}
