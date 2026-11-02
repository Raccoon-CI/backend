package caches

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (h *harness) spaceDir(id string) string {
	return filepath.Join(h.dir, "cache", filepath.FromSlash(id))
}

func (h *harness) exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestSweepDeletesEntriesNobodyHasUsedForMaxAge(t *testing.T) {
	h := newHarness(t)
	h.svc.cfg.MaxAge = time.Hour
	h.run(1, "acme/web", "refs/heads/main")
	h.save(1, "old", "old data")
	time.Sleep(30 * time.Millisecond)
	mid := time.Now()
	time.Sleep(30 * time.Millisecond)
	h.save(1, "new", "new data")

	res, err := h.svc.Sweep(context.Background(), mid.Add(time.Hour)) // cutoff = mid
	if err != nil || res.Expired != 1 || res.Freed != int64(len("old data")) || res.Removed != 0 {
		t.Fatalf("Sweep = %+v, %v", res, err)
	}
	if got := h.restore(1, "old"); got != "" {
		t.Fatalf("the unused entry is still served: %q", got)
	}
	if got := h.restore(1, "new"); got != "new data" {
		t.Fatalf("the newer entry: %q", got)
	}
}

func TestAHitKeepsAnEntryAlive(t *testing.T) {
	h := newHarness(t)
	h.svc.cfg.MaxAge = time.Hour
	h.run(1, "acme/web", "refs/heads/main")
	h.save(1, "deps", "data")
	time.Sleep(30 * time.Millisecond)
	mid := time.Now()
	time.Sleep(30 * time.Millisecond)
	if got := h.restore(1, "deps"); got != "data" { // used after mid
		t.Fatalf("restore = %q", got)
	}
	res, err := h.svc.Sweep(context.Background(), mid.Add(time.Hour))
	if err != nil || res.Expired != 0 {
		t.Fatalf("Sweep = %+v, %v: the entry was used after the cutoff", res, err)
	}
}

func TestASpaceLeftEmptyIsRemovedAndCanBeMadeAgain(t *testing.T) {
	h := newHarness(t)
	h.run(1, "acme/api", "refs/heads/feature")
	h.run(2, "acme/web", "refs/heads/main")
	h.save(1, "k", "data")
	h.save(2, "w", "web data")
	api := SpaceFor("acme/api", "refs/heads/feature", "main").Own
	if !h.exists(h.spaceDir(api)) {
		t.Fatal("setup: no space")
	}
	res, err := h.svc.Sweep(context.Background(), time.Now().Add(30*24*time.Hour))
	if err != nil || res.Expired != 2 || res.Removed != 2 {
		t.Fatalf("Sweep = %+v, %v", res, err)
	}
	if h.exists(h.spaceDir(api)) || h.exists(filepath.Dir(h.spaceDir(api))) {
		t.Fatal("the space and its repository's directory must go")
	}
	if got := h.restore(1, "k"); got != "" {
		t.Fatalf("restore after removal = %q", got)
	}
	if h.save(1, "k2", "again") != 201 || h.restore(1, "k2") != "again" {
		t.Fatal("a removed space must be made again when it is used")
	}
}

func TestASpaceWithAnUploadInProgressIsNotRemoved(t *testing.T) {
	h := newHarness(t)
	h.run(1, "acme/web", "refs/heads/feature")
	h.save(1, "k", "data")
	b, _ := json.Marshal(map[string]any{"key": "slow", "version": "v1"})
	if code, _ := h.do("POST", "caches", h.token(1), map[string]string{"Content-Type": "application/json"}, bytes.NewReader(b)); code != 201 {
		t.Fatalf("reserve = %d", code)
	}
	id := SpaceFor("acme/web", "refs/heads/feature", "main").Own
	res, err := h.svc.Sweep(context.Background(), time.Now().Add(30*24*time.Hour))
	if err != nil || res.Expired != 1 || res.Removed != 0 {
		t.Fatalf("Sweep = %+v, %v: the entry expires, the space stays while an upload is in progress", res, err)
	}
	if !h.exists(h.spaceDir(id)) {
		t.Fatal("the space was removed under an upload")
	}
}

func TestARepositoryIsKeptUnderItsCapAcrossItsSpaces(t *testing.T) {
	h := newHarness(t)
	h.svc.cfg.MaxRepoBytes = 25
	h.run(1, "acme/web", "refs/heads/main")
	h.run(2, "acme/web", "refs/heads/feature-a")
	h.run(3, "acme/web", "refs/heads/feature-b")
	h.run(4, "acme/api", "refs/heads/main")
	ten := strings.Repeat("x", 10)
	h.save(1, "main-deps", ten)
	time.Sleep(20 * time.Millisecond)
	h.save(2, "a-deps", ten)
	time.Sleep(20 * time.Millisecond)
	h.save(3, "b-deps", ten) // 30 bytes in the repository: over by 5
	h.save(4, "api-deps", strings.Repeat("y", 20))

	res, err := h.svc.Sweep(context.Background(), time.Now())
	if err != nil || res.Evicted != 1 || res.Expired != 0 || res.Freed != 10 {
		t.Fatalf("Sweep = %+v, %v", res, err)
	}
	if got := h.restore(1, "main-deps"); got != "" {
		t.Fatalf("the least recently used entry of the repository should have gone, restored %q", got)
	}
	if h.restore(2, "a-deps") != ten || h.restore(3, "b-deps") != ten {
		t.Fatal("the newer entries must stay")
	}
	if h.restore(4, "api-deps") == "" {
		t.Fatal("another repository was touched")
	}
	// Under the cap: a second sweep does nothing.
	if res, _ := h.svc.Sweep(context.Background(), time.Now()); res.Evicted != 0 {
		t.Fatalf("second sweep = %+v", res)
	}
}

func TestStartRemovesTheChunksOfUploadsThatNeverFinished(t *testing.T) {
	h := newHarness(t)
	orphan := filepath.Join(h.dir, "cache-spool", "raccoonci-cache-spool-9", "chunk")
	if err := os.MkdirAll(filepath.Dir(orphan), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.start() // the next start on the same directory
	if h.exists(orphan) {
		t.Fatal("the spool of a dead server is still there")
	}
}

func TestTheOrganizationIsWrittenDownForTheSweep(t *testing.T) {
	h := newHarness(t)
	h.run(1, "Acme/web", "refs/heads/main")
	h.save(1, "k", "data")
	id := SpaceFor("acme/web", "refs/heads/main", "main").Own
	b, err := os.ReadFile(filepath.Join(h.spaceDir(id), orgFile))
	if err != nil || string(b) != "acme" {
		t.Fatalf("org file = %q, %v", b, err)
	}
}

func TestASpaceWhoseBucketIsUnknownIsLeftAlone(t *testing.T) {
	h := newHarness(t)
	// An index of a space made before the organization was written down, whose
	// objects are not on this disk: the sweep cannot reach them, so it leaves it.
	id := RepoID("old/repo") + "/default"
	dir := h.spaceDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	idx := fmt.Sprintf(`{"next":2,"entries":[{"id":1,"key":"k","version":"v","size":5,"created":%q}]}`, time.Now().Add(-100*24*time.Hour).Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(idx), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := h.svc.Sweep(context.Background(), time.Now())
	if err != nil || res.Expired != 0 || res.Removed != 0 || !h.exists(dir) {
		t.Fatalf("Sweep = %+v, %v", res, err)
	}
}
