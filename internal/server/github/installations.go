package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AppInstallation is one installation of the App as GitHub lists them.
type AppInstallation struct {
	ID          int64
	Account     string
	AccountType string
	Selection   string // all | selected
	Suspended   bool
}

// Installations lists every installation of the App (GET /app/installations,
// authenticated as the App itself), following the pages.
func (a *App) Installations(ctx context.Context) ([]AppInstallation, error) {
	base := a.Base
	if base == "" {
		base = "https://api.github.com"
	}
	var out []AppInstallation
	for page := 1; page <= 50; page++ {
		jwt, err := a.jwt()
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/app/installations?per_page=100&page=%d", base, page), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+jwt)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		h := a.HTTP
		if h == nil {
			h = &http.Client{Timeout: 30 * time.Second}
		}
		resp, err := h.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("github: installations: %s: %s", resp.Status, strings.TrimSpace(string(body[:min(len(body), 200)])))
		}
		var list []struct {
			ID      int64 `json:"id"`
			Account struct {
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"account"`
			Selection   string `json:"repository_selection"`
			SuspendedAt any    `json:"suspended_at"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, errors.New("github: the installations reply could not be read")
		}
		for _, in := range list {
			out = append(out, AppInstallation{ID: in.ID, Account: in.Account.Login, AccountType: in.Account.Type,
				Selection: in.Selection, Suspended: in.SuspendedAt != nil})
		}
		if len(list) < 100 {
			break
		}
	}
	return out, nil
}

// InstallationRepos lists the repositories an installation can reach
// (GET /installation/repositories with its token), as owner/name.
func (c *Client) InstallationRepos(ctx context.Context, installationID int64) ([]string, error) {
	var out []string
	for page := 1; page <= 100; page++ {
		var res struct {
			Repositories []struct {
				FullName string `json:"full_name"`
			} `json:"repositories"`
		}
		if err := c.get(ctx, installationID, fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page), &res); err != nil {
			return nil, err
		}
		for _, r := range res.Repositories {
			out = append(out, r.FullName)
		}
		if len(res.Repositories) < 100 {
			break
		}
	}
	return out, nil
}
