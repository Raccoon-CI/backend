// Package storagecheck tries an object store before the server relies on it:
// it writes a small object, reads it back through a presigned link, deletes
// it, and looks for the mistake of a bucket that anyone can read. A user who
// brings their own bucket finds out here that a key is wrong or the bucket is
// open, not when a job cannot save its cache.
package storagecheck

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"raccoonci/internal/blob"
)

// Check runs the checks on store. publicProbes are plain addresses of the
// check object, tried without any credential: one that answers 200 means the
// bucket is readable by anyone, which is returned as a warning (the bucket
// still works). An error means the store cannot be used.
func Check(ctx context.Context, store blob.Store, publicProbes func(key string) []string) (warnings []string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var id [8]byte
	rand.Read(id[:])
	key := "check/" + hex.EncodeToString(id[:])
	payload := []byte("raccoonci storage check " + hex.EncodeToString(id[:]))

	up, err := store.BeginUpload(ctx, key, "text/plain")
	if err != nil {
		return nil, fmt.Errorf("starting an upload: %w", err)
	}
	part, err := up.PutPart(ctx, 1, bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		up.Abort(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("writing an object: %w", err)
	}
	if err := up.Complete(ctx, []blob.Part{part}); err != nil {
		up.Abort(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("finishing an upload: %w", err)
	}
	defer store.Delete(context.WithoutCancel(ctx), key)

	info, err := store.Stat(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("reading the object's size: %w", err)
	}
	if info.Size != int64(len(payload)) {
		return nil, fmt.Errorf("the object has %d bytes, %d were written", info.Size, len(payload))
	}
	link, err := store.PresignGet(ctx, key, time.Minute)
	if err != nil {
		return nil, fmt.Errorf("making a download link: %w", err)
	}
	body, status, err := get(ctx, link)
	if err != nil {
		return nil, fmt.Errorf("downloading through the link: %w", err)
	}
	if status != http.StatusOK || !bytes.Equal(body, payload) {
		return nil, fmt.Errorf("the download link answered %d with the wrong content", status)
	}
	if publicProbes != nil {
		for _, u := range publicProbes(key) {
			if _, status, err := get(ctx, u); err == nil && status == http.StatusOK {
				warnings = append(warnings, "the bucket gives its objects to anyone who asks without credentials ("+host(u)+"): make it private")
				break
			}
		}
	}
	if err := store.Delete(ctx, key); err != nil {
		return warnings, fmt.Errorf("deleting the object: %w", err)
	}
	if _, err := store.Stat(ctx, key); !errors.Is(err, blob.ErrNotFound) {
		return warnings, fmt.Errorf("the object is still there after it was deleted (%v)", err)
	}
	return warnings, nil
}

func get(ctx context.Context, u string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b, resp.StatusCode, err
}

func host(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	host, _, _ := strings.Cut(u, "/")
	return host
}

// S3Probes gives the plain addresses an S3 bucket's objects would have if the
// bucket were public: path style on a custom endpoint, or AWS's own address.
func S3Probes(c blob.S3Config) func(key string) []string {
	return func(key string) []string {
		k := c.Prefix + key
		if c.Endpoint != "" {
			return []string{strings.TrimRight(c.Endpoint, "/") + "/" + c.Bucket + "/" + k}
		}
		region := c.Region
		if region == "" {
			region = "us-east-1"
		}
		return []string{"https://" + c.Bucket + ".s3." + region + ".amazonaws.com/" + k}
	}
}
