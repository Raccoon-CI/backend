package github

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxSource caps what a repository's source may unpack to, so one huge
// repository cannot fill the server's disk or a machine's.
const maxSource = 2 << 30

// topDir is the one directory the repacked archive keeps everything under: the
// agent's extractor (archive.ExtractTarGz) drops the first path component, as
// it does for GitHub's own archive, whose directory name (owner-repo-sha/)
// changes with every commit.
const topDir = "src/"

// SourceTar writes the repository at a commit to w as a .tar.gz in the layout
// a machine's agent unpacks: everything under one top-level directory.
// GitHub's archive has a global header, which is dropped. There is no .git
// directory, so a step that runs git commands on the checkout has no history
// to work with.
func (c *Client) SourceTar(ctx context.Context, installationID int64, repo, sha string, w io.Writer) error {
	tok, err := c.Tokens.Token(ctx, installationID)
	if err != nil {
		return err
	}
	base := c.Base
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/repos/"+repo+"/tarball/"+url.PathEscape(sha), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	h := c.HTTP
	if h == nil {
		// No overall timeout: a big repository takes a while. The context bounds it.
		h = &http.Client{}
	}
	resp, err := h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("github tarball of %s at %s: %s: %s", repo, sha, resp.Status, strings.TrimSpace(string(body)))
	}
	return Repack(w, resp.Body, maxSource)
}

// Repack copies a GitHub source archive (.tar.gz) to w with its top directory
// renamed to topDir and its global header dropped. It fails when what it unpacks to would exceed
// max bytes or an entry names a path outside the tree.
func Repack(w io.Writer, r io.Reader, max int64) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("the archive is not gzip: %w", err)
	}
	defer gz.Close()
	in := tar.NewReader(gz)
	zw := gzip.NewWriter(w)
	out := tar.NewWriter(zw)
	var total int64
	strip := func(name string) (string, bool, error) {
		_, rest, _ := strings.Cut(name, "/")
		rest = strings.TrimPrefix(rest, "./")
		if rest == "" {
			return "", false, nil // the top directory itself
		}
		for _, part := range strings.Split(strings.TrimSuffix(rest, "/"), "/") {
			if part == ".." {
				return "", false, fmt.Errorf("the archive has an entry outside the tree: %q", name)
			}
		}
		return rest, true, nil
	}
	for {
		hdr, err := in.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading the archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name, keep, err := strip(hdr.Name)
		if err != nil {
			return err
		}
		if !keep {
			continue
		}
		hdr.Name = topDir + name
		if hdr.Typeflag == tar.TypeLink {
			link, ok, err := strip(hdr.Linkname)
			if err != nil || !ok {
				return fmt.Errorf("the archive has a hard link to %q", hdr.Linkname)
			}
			hdr.Linkname = topDir + link
		}
		total += hdr.Size
		if total > max {
			return fmt.Errorf("the repository unpacks to more than %d bytes", max)
		}
		if err := out.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			return err
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	return zw.Close()
}

// ResolveRef returns the commit a branch, tag or commit id names.
func (c *Client) ResolveRef(ctx context.Context, installationID int64, repo, ref string) (string, error) {
	var res struct {
		SHA string `json:"sha"`
	}
	if err := c.get(ctx, installationID, "/repos/"+repo+"/commits/"+url.PathEscape(ref), &res); err != nil {
		return "", err
	}
	if res.SHA == "" {
		return "", errors.New("github: the commit has no sha")
	}
	return res.SHA, nil
}

// DefaultBranch returns the name of the repository's default branch.
func (c *Client) DefaultBranch(ctx context.Context, installationID int64, repo string) (string, error) {
	var res struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.get(ctx, installationID, "/repos/"+repo, &res); err != nil {
		return "", err
	}
	return res.DefaultBranch, nil
}

// FileAt returns one file of a repository at a ref.
func (c *Client) FileAt(ctx context.Context, installationID int64, repo, filePath, ref string) ([]byte, error) {
	var f struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Size     int64  `json:"size"`
	}
	if err := c.get(ctx, installationID, "/repos/"+repo+"/contents/"+filePath+"?ref="+url.QueryEscape(ref), &f); err != nil {
		return nil, err
	}
	if f.Encoding != "base64" || f.Size > maxFile {
		return nil, fmt.Errorf("github: %s cannot be read (encoding %q, %d bytes)", filePath, f.Encoding, f.Size)
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
}
