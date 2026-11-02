// Package github is the server's view of GitHub: reading a repository at a
// commit on behalf of an installation of the GitHub App (or, while
// developing, with a plain token). Nothing here is ever logged with a
// credential in it.
package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TokenSource gives the token to call GitHub with for an installation. A
// delivery of a repository webhook has no installation (0).
type TokenSource interface {
	Token(ctx context.Context, installationID int64) (string, error)
}

// StaticToken is one fixed token, for development with a repository webhook.
type StaticToken string

// Token implements TokenSource.
func (t StaticToken) Token(context.Context, int64) (string, error) { return string(t), nil }

// App authenticates as a GitHub App: it signs a short-lived JWT with the App's
// private key and trades it for an installation access token, which it keeps
// until shortly before it expires.
type App struct {
	ID  int64
	Key *rsa.PrivateKey
	// Base is the API address; default https://api.github.com.
	Base string
	HTTP *http.Client
	// Now is the clock; tests replace it.
	Now func() time.Time

	mu     sync.Mutex
	tokens map[int64]cachedToken
}

type cachedToken struct {
	token   string
	expires time.Time
}

// ParsePrivateKey reads the PEM file GitHub gives for an App (PKCS#1, or PKCS#8).
func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("github: the private key is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("github: the private key is neither PKCS#1 nor PKCS#8")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github: the private key is not an RSA key")
	}
	return rk, nil
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// jwt makes the App's own token, valid for 9 minutes (GitHub allows 10), with
// the issue time a minute back to forgive a slow clock.
func (a *App) jwt() (string, error) {
	now := a.now()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": a.ID})
	signing := b64(header) + "." + b64(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + b64(sig), nil
}

// InstallationOf finds the installation of the App on a repository
// ("owner/name"): what a run started by hand needs, since no delivery has told
// the server. ErrNotFound means the App is not installed there.
func (a *App) InstallationOf(ctx context.Context, repo string) (int64, error) {
	jwt, err := a.jwt()
	if err != nil {
		return 0, err
	}
	base := a.Base
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/repos/%s/installation", base, repo), nil)
	if err != nil {
		return 0, err
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
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return 0, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("github: installation of %s: %s: %s", repo, resp.Status, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.ID == 0 {
		return 0, errors.New("github: the installation reply could not be read")
	}
	return out.ID, nil
}

// Token implements TokenSource.
func (a *App) Token(ctx context.Context, installationID int64) (string, error) {
	if installationID == 0 {
		return "", errors.New("github: the delivery names no installation to act for")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.tokens[installationID]; ok && a.now().Before(c.expires.Add(-time.Minute)) {
		return c.token, nil
	}
	jwt, err := a.jwt()
	if err != nil {
		return "", err
	}
	base := a.Base
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/app/installations/%d/access_tokens", base, installationID), nil)
	if err != nil {
		return "", err
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
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusCreated {
		// The body of a failed request holds a message, never a credential.
		return "", fmt.Errorf("github: installation token for %d: %s: %s", installationID, resp.Status, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	var out struct {
		Token   string    `json:"token"`
		Expires time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Token == "" {
		return "", errors.New("github: the installation token reply could not be read")
	}
	if a.tokens == nil {
		a.tokens = map[int64]cachedToken{}
	}
	a.tokens[installationID] = cachedToken{out.Token, out.Expires}
	return out.Token, nil
}
