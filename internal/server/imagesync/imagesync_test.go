package imagesync

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Raccoon-CI/backend/internal/server/store"
	"github.com/Raccoon-CI/lib/pkg/runnerimages"
)

type fakeSource struct {
	versions map[string]string // image -> version; a missing image fails
	alias    string
	calls    int
}

func (f *fakeSource) Latest(_ context.Context, image string) (*runnerimages.Defaults, error) {
	f.calls++
	v, ok := f.versions[image]
	if !ok {
		return nil, errors.New("not deployed")
	}
	return &runnerimages.Defaults{Image: image, Version: v}, nil
}

func (f *fakeSource) LatestAlias(context.Context) (string, error) { return f.alias, nil }

func newSyncer(t *testing.T, src *fakeSource) *Syncer {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Syncer{Store: st, Source: src}
}

func TestWithNothingPolledTheSnapshotIsTheBuiltInOne(t *testing.T) {
	s := newSyncer(t, &fakeSource{})
	got, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := runnerimages.Embedded()
	if len(got.Images) != len(want.Images) || got.Aliases["ubuntu-latest"] != want.Aliases["ubuntu-latest"] {
		t.Fatalf("got %d images, want %d", len(got.Images), len(want.Images))
	}
}

func TestPolledImagesReplaceTheBuiltInOnesAndAFailedImageKeepsItsOldData(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{versions: map[string]string{"ubuntu24": "29990101.1", "ubuntu22": "29990101.2"}, alias: "ubuntu24"}
	s := newSyncer(t, src)
	err := s.Sync(ctx) // ubuntu26 cannot be read
	if err == nil {
		t.Fatal("a failing image must be reported")
	}
	snap, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	emb, _ := runnerimages.Embedded()
	if snap.Images["ubuntu24"].Version != "29990101.1" || snap.Images["ubuntu22"].Version != "29990101.2" {
		t.Fatalf("polled data not used: %v %v", snap.Images["ubuntu24"].Version, snap.Images["ubuntu22"].Version)
	}
	if snap.Images["ubuntu26"] == nil || snap.Images["ubuntu26"].Version != emb.Images["ubuntu26"].Version {
		t.Fatal("an image that could not be polled must keep its built-in data")
	}
	if snap.Aliases["ubuntu-latest"] != "ubuntu24" {
		t.Fatalf("alias = %q", snap.Aliases["ubuntu-latest"])
	}

	// A later poll in which ubuntu24 fails keeps the polled data, not the built-in.
	src.versions = map[string]string{"ubuntu26": "29990102.1"}
	s.Sync(ctx)
	snap, _ = s.Snapshot(ctx)
	if snap.Images["ubuntu24"].Version != "29990101.1" || snap.Images["ubuntu26"].Version != "29990102.1" {
		t.Fatalf("after a partial poll: ubuntu24 %s, ubuntu26 %s", snap.Images["ubuntu24"].Version, snap.Images["ubuntu26"].Version)
	}
}

func TestAnAliasToAnUntrackedImageIsRefused(t *testing.T) {
	ctx := context.Background()
	s := newSyncer(t, &fakeSource{versions: map[string]string{"ubuntu24": "29990101.1"}, alias: "ubuntu99"})
	if err := s.Sync(ctx); err == nil {
		t.Fatal("expected an error")
	}
	snap, _ := s.Snapshot(ctx)
	if snap.Aliases["ubuntu-latest"] == "ubuntu99" {
		t.Fatal("the alias points to an image nobody tracks")
	}
}

func TestADamagedStoredDocumentIsReplacedByTheNextPoll(t *testing.T) {
	ctx := context.Background()
	s := newSyncer(t, &fakeSource{versions: map[string]string{"ubuntu24": "29990101.1", "ubuntu22": "1.1", "ubuntu26": "1.2"}, alias: "ubuntu24"})
	if err := s.Store.PutDocument(ctx, documentName, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if snap, err := s.Snapshot(ctx); err == nil || snap == nil {
		t.Fatalf("a damaged document must be reported and the built-in snapshot still served: %v", err)
	}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot(ctx)
	if err != nil || snap.Images["ubuntu24"].Version != "29990101.1" {
		t.Fatalf("after repair: %v", err)
	}
}
