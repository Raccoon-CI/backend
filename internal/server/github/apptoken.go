package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RevokeToken ends an installation token (DELETE /installation/token, which
// is authenticated by the token itself). A token that is already gone (401)
// counts as revoked.
func (a *App) RevokeToken(ctx context.Context, token string) error {
	base := a.Base
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, "DELETE", base+"/installation/token", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	h := a.HTTP
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusUnauthorized {
		return nil
	}
	return fmt.Errorf("github: revoking a token: %s", resp.Status)
}

// RunToken makes an installation token for one repository with only the
// permissions given ("contents": "read", ...), for a job to use as its
// GITHUB_TOKEN. It is not cached: every job gets a token of its own, which
// GitHub expires in an hour. GitHub refuses (422) permissions the App does not
// have; that comes back as an error and the caller decides what to do. The
// token is in the result only; no error text holds it.
func (a *App) RunToken(ctx context.Context, installationID int64, repo string, permissions map[string]string) (string, error) {
	if installationID == 0 {
		return "", errors.New("github: the run names no installation to act for")
	}
	_, name, ok := strings.Cut(repo, "/")
	if !ok || name == "" {
		return "", fmt.Errorf("github: %q is not owner/name", repo)
	}
	jwt, err := a.jwt()
	if err != nil {
		return "", err
	}
	base := a.Base
	if base == "" {
		base = "https://api.github.com"
	}
	body, err := json.Marshal(map[string]any{"repositories": []string{name}, "permissions": permissions})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/app/installations/%d/access_tokens", base, installationID), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	h := a.HTTP
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := h.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("github: a token for %s: %s: %s", repo, resp.Status, strings.TrimSpace(string(answer[:min(len(answer), 200)])))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(answer, &out); err != nil || out.Token == "" {
		return "", errors.New("github: the token reply could not be read")
	}
	return out.Token, nil
}
