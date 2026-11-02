package api

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestMetricsNeedAnAdminTokenAndCountRunsAndChecks(t *testing.T) {
	a, h := setup(t)
	a.Checks = []Check{
		{Name: "database", Run: a.Store.Ping},
		{Name: "disk", Run: func(context.Context) error { return errors.New("full") }},
	}
	if rec := call(h, "GET", "/metrics", "", ""); rec.Code != 401 {
		t.Errorf("no token: %d", rec.Code)
	}
	rec := call(h, "GET", "/metrics", token, "")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`raccoonci_build_info{version="test"} 1`,
		`raccoonci_check{check="database"} 1`,
		`raccoonci_check{check="disk"} 0`,
		`raccoonci_oldest_queued_run_age_seconds 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "full") {
		t.Error("the reason a check fails leaked into the metrics")
	}
}

func TestQuoteEscapesWhatThePrometheusFormatNeedsEscaped(t *testing.T) {
	if got := quote("a\"b\\c\nd"); got != `"a\"b\\c\nd"` {
		t.Errorf("quote: %s", got)
	}
}
