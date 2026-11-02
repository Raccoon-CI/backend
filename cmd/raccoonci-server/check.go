package main

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// healthCheck is `raccoonci-server -check URL`: ask a running server whether it
// is ready and exit non-zero if not. A distroless image has no curl, so the
// container's HEALTHCHECK runs the server binary itself.
func healthCheck(url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return nil
}
