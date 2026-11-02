package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestReadyzSaysWhichCheckFailsAndNeedsNoToken(t *testing.T) {
	a, _ := setup(t)
	failing := false
	a.Checks = []Check{
		{Name: "database", Run: func(ctx context.Context) error { return a.Store.Ping(ctx) }},
		{Name: "disk", Run: func(context.Context) error {
			if failing {
				return errors.New("only 12 MiB free on /var/lib/raccoonci-server")
			}
			return nil
		}},
	}
	h := a.Handler()
	rec := call(h, "GET", "/readyz", "", "")
	var out struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 || out.Status != "ready" || out.Checks["database"] != "ok" || out.Checks["disk"] != "ok" {
		t.Fatalf("ready: %d %s", rec.Code, rec.Body)
	}
	failing = true
	rec = call(h, "GET", "/readyz", "", "")
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 503 || out.Status != "not ready" || out.Checks["disk"] != "failing" || out.Checks["database"] != "ok" {
		t.Fatalf("not ready: %d %s", rec.Code, rec.Body)
	}
	// The answer is open to anyone, so it says which check failed and not why.
	if body := rec.Body.String(); strings.Contains(body, "MiB") || strings.Contains(body, "/var/lib") {
		t.Errorf("the reason leaked into an unauthenticated answer: %s", body)
	}
}

func TestReadyzWithNoChecksOnlySaysTheProcessIsUpAndTheLivenessRouteIsUnchanged(t *testing.T) {
	_, h := setup(t)
	if rec := call(h, "GET", "/readyz", "", ""); rec.Code != 200 {
		t.Errorf("readyz with no checks: %d", rec.Code)
	}
	if rec := call(h, "GET", "/healthz", "", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("healthz: %d %s", rec.Code, rec.Body)
	}
}
