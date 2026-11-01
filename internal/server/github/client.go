package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// Client reads repositories through the REST API.
type Client struct {
	Base   string // default https://api.github.com
	Tokens TokenSource
	HTTP   *http.Client
}

// ErrNotFound is returned for a 404.
var ErrNotFound = errors.New("github: not found")

// File is a file of a repository at a commit.
type File struct {
	Path    string
	Content []byte
}

// maxFile caps one workflow file; a bigger one is not a workflow.
const maxFile = 1 << 20

func (c *Client) get(ctx context.Context, installationID int64, p string, out any) error {
	return c.do(ctx, installationID, "GET", p, nil, out)
}

// do calls the API as the installation; body (if any) goes as JSON and a JSON
// answer is read into out (if any).
func (c *Client) do(ctx context.Context, installationID int64, method, p string, body, out any) error {
	tok, err := c.Tokens.Token(ctx, installationID)
	if err != nil {
		return err
	}
	base := c.Base
	if base == "" {
		base = "https://api.github.com"
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+p, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	h := c.HTTP
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode >= 300:
		return fmt.Errorf("github %s %s: %s: %s", method, p, resp.Status, strings.TrimSpace(string(answer[:min(len(answer), 200)])))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(answer, out)
}

// WorkflowFiles returns the workflow files (.github/workflows/*.yml and
// *.yaml, not in subdirectories, as on GitHub) of repo at ref, in name order.
// A repository without the directory has none.
func (c *Client) WorkflowFiles(ctx context.Context, installationID int64, repo, ref string) ([]File, error) {
	q := "?ref=" + url.QueryEscape(ref)
	var dir []struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Type string `json:"type"`
		Size int64  `json:"size"`
	}
	if err := c.get(ctx, installationID, "/repos/"+repo+"/contents/.github/workflows"+q, &dir); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var files []File
	for _, e := range dir {
		ext := path.Ext(e.Name)
		if e.Type != "file" || (ext != ".yml" && ext != ".yaml") {
			continue
		}
		if e.Size > maxFile {
			return nil, fmt.Errorf("github: %s is %d bytes, more than the %d a workflow may have", e.Path, e.Size, maxFile)
		}
		var f struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}
		if err := c.get(ctx, installationID, "/repos/"+repo+"/contents/"+e.Path+q, &f); err != nil {
			return nil, fmt.Errorf("reading %s: %w", e.Path, err)
		}
		if f.Encoding != "base64" {
			return nil, fmt.Errorf("github: %s has encoding %q", e.Path, f.Encoding)
		}
		b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", e.Path, err)
		}
		files = append(files, File{Path: e.Path, Content: b})
	}
	return files, nil
}

// ChangedFiles lists the files that differ between two commits. It returns
// nil, meaning unknown, when GitHub cut the list short (300 files at most) and
// when either commit cannot be compared (a new branch has no earlier commit).
func (c *Client) ChangedFiles(ctx context.Context, installationID int64, repo, base, head string) ([]string, error) {
	var res struct {
		Files []struct {
			Filename string `json:"filename"`
		} `json:"files"`
	}
	err := c.get(ctx, installationID, "/repos/"+repo+"/compare/"+url.PathEscape(base)+"..."+url.PathEscape(head), &res)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(res.Files) >= 300 {
		return nil, nil
	}
	files := make([]string, 0, len(res.Files))
	for _, f := range res.Files {
		files = append(files, f.Filename)
	}
	return files, nil
}
