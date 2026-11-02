package storagecheck

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Raccoon-CI/raccoonci/pkg/blob"
)

// dirStore is a local store served by a test server, so the links work.
func dirStore(t *testing.T) *blob.Dir {
	t.Helper()
	var mux http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mux.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	d, err := blob.NewDir(t.TempDir(), srv.URL, bytes.Repeat([]byte("s"), 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	m := http.NewServeMux()
	m.Handle(d.Prefix, d.Handler())
	mux = m
	return d
}

func TestAWorkingStorePasses(t *testing.T) {
	warnings, err := Check(context.Background(), dirStore(t), nil)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("warnings %v, err %v", warnings, err)
	}
}

func TestAPubliclyReadableBucketIsAWarningNotAnError(t *testing.T) {
	open := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "anyone can read") }))
	defer open.Close()
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer closed.Close()
	warnings, err := Check(context.Background(), dirStore(t), func(key string) []string { return []string{closed.URL + "/" + key, open.URL + "/" + key} })
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "make it private") {
		t.Fatalf("warnings %v, err %v", warnings, err)
	}
	warnings, err = Check(context.Background(), dirStore(t), func(key string) []string { return []string{closed.URL + "/" + key} })
	if err != nil || len(warnings) != 0 {
		t.Fatalf("a closed bucket: warnings %v, err %v", warnings, err)
	}
}

// failing makes one step of the store fail.
type failing struct {
	blob.Store
	step string
}

var errBoom = errors.New("boom")

func (f failing) BeginUpload(ctx context.Context, key, ct string) (blob.Upload, error) {
	if f.step == "begin" {
		return nil, errBoom
	}
	up, err := f.Store.BeginUpload(ctx, key, ct)
	if err != nil {
		return nil, err
	}
	return failingUpload{up, f.step}, nil
}

func (f failing) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if f.step == "presign" {
		return "", errBoom
	}
	if f.step == "badlink" {
		return "http://127.0.0.1:1/unreachable", nil
	}
	return f.Store.PresignGet(ctx, key, ttl)
}

func (f failing) Delete(ctx context.Context, key string) error {
	if f.step == "delete" {
		return errBoom
	}
	return f.Store.Delete(ctx, key)
}

type failingUpload struct {
	blob.Upload
	step string
}

func (u failingUpload) PutPart(ctx context.Context, n int, r io.Reader, size int64) (blob.Part, error) {
	if u.step == "put" {
		return blob.Part{}, errBoom
	}
	return u.Upload.PutPart(ctx, n, r, size)
}

func TestEachFailingStepIsNamed(t *testing.T) {
	for step, want := range map[string]string{
		"begin": "starting an upload", "put": "writing an object", "presign": "making a download link",
		"badlink": "downloading through the link", "delete": "deleting the object",
	} {
		_, err := Check(context.Background(), failing{dirStore(t), step}, nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want it to say %q", step, err, want)
		}
	}
}

func TestS3Probes(t *testing.T) {
	if got := S3Probes(blob.S3Config{Endpoint: "https://acct.r2.cloudflarestorage.com/", Bucket: "b", Prefix: "p/"})("check/x"); len(got) != 1 || got[0] != "https://acct.r2.cloudflarestorage.com/b/p/check/x" {
		t.Errorf("custom endpoint: %v", got)
	}
	if got := S3Probes(blob.S3Config{Bucket: "b", Region: "eu-west-1"})("check/x"); len(got) != 1 || got[0] != "https://b.s3.eu-west-1.amazonaws.com/check/x" {
		t.Errorf("aws: %v", got)
	}
}
