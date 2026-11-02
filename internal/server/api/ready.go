package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// Check is one thing /readyz looks at: it returns nil when that is fine.
type Check struct {
	Name string
	Run  func(ctx context.Context) error
}

// ready answers whether the server can do its work now, which /healthz (the
// process is up) does not say: a proxy or an orchestrator that should stop
// sending it requests, or restart it, asks this. It needs no token and tells
// little: which check failed, not why (the reason is logged).
func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	results := map[string]string{}
	ok := true
	for _, c := range a.Checks {
		if err := c.Run(ctx); err != nil {
			ok = false
			results[c.Name] = "failing"
			a.log().Warn("not ready", "check", c.Name, "err", err)
			continue
		}
		results[c.Name] = "ok"
	}
	status, code := "ready", http.StatusOK
	if !ok {
		status, code = "not ready", http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"status": status, "version": a.Version, "checks": results})
}

func (a *API) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.New(slog.DiscardHandler)
}
