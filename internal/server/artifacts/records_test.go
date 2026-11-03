package artifacts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Raccoon-CI/backend/internal/server/store"
	"github.com/Raccoon-CI/lib/pkg/blob"
)

// A server over a database, reachable through a real listener so that the
// links it hands out can be followed.
type dbEnv struct {
	t   *testing.T
	st  *store.SQLite
	dir string
	svc *Service
	srv *httptest.Server
}

func newDBEnv(t *testing.T, dir string, st *store.SQLite, edit func(*Config)) *dbEnv {
	t.Helper()
	e := &dbEnv{t: t, st: st, dir: dir}
	mux := http.NewServeMux()
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	cfg := Config{Dir: dir, PublicURL: e.srv.URL, Records: st}
	if edit != nil {
		edit(&cfg)
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	svc.Mount(mux)
	e.svc = svc
	return e
}

func openDB(t *testing.T) *store.SQLite {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func (e *dbEnv) call(token, method string, body map[string]any) (int, map[string]any) {
	e.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", e.srv.URL+"/twirp/github.actions.results.api.v1.ArtifactService/"+method, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// upload sends a one-block artifact for run as job and finalizes it.
func (e *dbEnv) upload(run int64, job, name, content string, extra map[string]any) {
	e.t.Helper()
	env, err := e.svc.JobEnv(run, job)
	if err != nil {
		e.t.Fatal(err)
	}
	tok := env["ACTIONS_RUNTIME_TOKEN"]
	ids := map[string]any{"workflow_run_backend_id": fmt.Sprint(run), "workflow_job_run_backend_id": job, "name": name}
	for k, v := range extra {
		ids[k] = v
	}
	code, out := e.call(tok, "CreateArtifact", ids)
	if code != 200 {
		e.t.Fatalf("CreateArtifact = %d %v", code, out)
	}
	u := out["signed_upload_url"].(string)
	blockID := base64.StdEncoding.EncodeToString([]byte("0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0" + fmt.Sprintf("%012d", 0)))
	put := func(rawURL, body string) {
		req, _ := http.NewRequest("PUT", rawURL, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			e.t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 201 {
			e.t.Fatalf("PUT %s = %d", rawURL, resp.StatusCode)
		}
	}
	put(u+"&comp=block&blockid="+url.QueryEscape(blockID), content)
	put(u+"&comp=blocklist", `<?xml version="1.0" encoding="utf-8"?><BlockList><Latest>`+blockID+`</Latest></BlockList>`)
	fin := map[string]any{"workflow_run_backend_id": fmt.Sprint(run), "workflow_job_run_backend_id": job, "name": name, "size": fmt.Sprint(len(content)), "hash": "sha256:feed"}
	if code, out := e.call(tok, "FinalizeArtifact", fin); code != 200 {
		e.t.Fatalf("FinalizeArtifact = %d %v", code, out)
	}
}

func TestStartRemovesTheSpoolOfUploadsThatNeverFinished(t *testing.T) {
	dir := t.TempDir()
	orphan := filepath.Join(dir, "spool", "raccoonci-spool-123", "block")
	if err := os.MkdirAll(filepath.Dir(orphan), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("half an upload"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Dir: dir, PublicURL: "https://ci.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := os.Stat(orphan); err == nil {
		t.Fatal("the spool of a dead server is still there")
	}
}

func fetch(t *testing.T, u string) (int, string) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestArtifactsOutliveAServerRestartAndCanBeListedAndDownloaded(t *testing.T) {
	st, dir := openDB(t), t.TempDir()
	e1 := newDBEnv(t, dir, st, nil)
	e1.upload(7, "build-1", "logs", "line one\n", nil)
	e1.upload(7, "build-1", "report", "report body", nil)
	e1.upload(8, "build-1", "other-run", "x", nil)
	e1.svc.Close()
	e1.srv.Close()

	e2 := newDBEnv(t, dir, st, nil) // the next start: the signing key is new, the database and the data are the same
	ctx := context.Background()
	got, err := e2.svc.List(ctx, 7)
	if err != nil || len(got) != 2 || got[0].Name != "logs" || got[1].Name != "report" || got[1].Size != 11 || got[1].Digest != "sha256:feed" || got[0].Job != "build-1" {
		t.Fatalf("List(7) = %+v, %v", got, err)
	}
	if other, _ := e2.svc.List(ctx, 8); len(other) != 1 || other[0].Name != "other-run" {
		t.Fatalf("List(8) = %+v", other)
	}
	u, err := e2.svc.DownloadURL(ctx, 7, got[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if c, body := fetch(t, u); c != 200 || body != "report body" {
		t.Fatalf("download = %d %q", c, body)
	}
	if _, err := e2.svc.DownloadURL(ctx, 8, got[1].ID); err != ErrNotFound {
		t.Fatalf("an artifact of run 7 asked for as run 8's: err = %v", err)
	}
	// A new artifact after the restart does not take an old id.
	e2.upload(7, "build-1", "late", "z", nil)
	all, _ := e2.svc.List(ctx, 7)
	seen := map[int64]bool{}
	for _, a := range all {
		if seen[a.ID] {
			t.Fatalf("id %d used twice: %+v", a.ID, all)
		}
		seen[a.ID] = true
	}
	if len(all) != 3 {
		t.Fatalf("all = %+v", all)
	}
}

func TestSweepDeletesExpiredArtifactsWithTheirData(t *testing.T) {
	st, dir := openDB(t), t.TempDir()
	e := newDBEnv(t, dir, st, func(c *Config) { c.MaxKeep = 24 * time.Hour })
	// asks for a year, gets a day
	e.upload(7, "build-1", "greedy", "old news", map[string]any{"expires_at": time.Now().Add(365 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)})
	ctx := context.Background()
	list, _ := e.svc.List(ctx, 7)
	if len(list) != 1 {
		t.Fatalf("list = %+v", list)
	}
	u, _ := e.svc.DownloadURL(ctx, 7, list[0].ID)

	if n, err := e.svc.Sweep(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("Sweep now = %d, %v: nothing is due", n, err)
	}
	if n, err := e.svc.Sweep(ctx, time.Now().Add(25*time.Hour)); err != nil || n != 1 {
		t.Fatalf("Sweep tomorrow = %d, %v", n, err)
	}
	if list, _ := e.svc.List(ctx, 7); len(list) != 0 {
		t.Fatalf("still listed: %+v", list)
	}
	if recs, _ := st.ListArtifacts(ctx, scopeDisk); len(recs) != 0 {
		t.Fatalf("record left: %+v", recs)
	}
	if c, _ := fetch(t, u); c != 404 {
		t.Fatalf("the data of an expired artifact still downloads: %d", c)
	}
}

func TestSweepForgetsRecordsNoServiceHoldsButKeepsThoseItCouldNotReach(t *testing.T) {
	st := openDB(t)
	ctx := context.Background()
	old := time.Now().Add(-time.Hour)
	for _, a := range []store.Artifact{
		{Scope: "acme@v0", ID: 1, Run: "7", Name: "orphan", Key: "7/1", ExpiresAt: old, CreatedAt: old},
		{Scope: "acme@v1", ID: 1, Run: "9", Name: "unreachable", Key: "9/1", ExpiresAt: old, CreatedAt: old},
	} {
		if err := st.PutArtifact(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	// Run 7 is served from the disk (its organization has no bucket now); run 9's
	// organization cannot be looked up.
	e := newDBEnv(t, t.TempDir(), st, func(c *Config) {
		c.RunOrg = func(_ context.Context, run int64) (string, error) {
			if run == 9 {
				return "", fmt.Errorf("database is busy")
			}
			return "acme", nil
		}
		c.S3For = func(context.Context, string) (*blob.S3Config, string, error) { return nil, "v1", nil }
	})
	n, err := e.svc.Sweep(ctx, time.Now())
	if err == nil || n != 0 {
		t.Fatalf("Sweep = %d, %v: the run it could not reach must be reported", n, err)
	}
	if recs, _ := st.ListArtifacts(ctx, "acme@v0"); len(recs) != 0 {
		t.Fatalf("an orphaned record must be forgotten: %+v", recs)
	}
	if recs, _ := st.ListArtifacts(ctx, "acme@v1"); len(recs) != 1 {
		t.Fatalf("a record whose service could not be reached must stay for the next sweep: %+v", recs)
	}
}
