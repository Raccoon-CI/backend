package catalogsync

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/Raccoon-CI/backend/internal/server/store"
	"github.com/Raccoon-CI/lib/pkg/catalog"
)

const zig = "toolchains:\n  zig: {title: Zig, commands: [zig]}\n"

func openStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// feedServer serves body (and sig, when set) at /c.yaml; down makes it fail.
func feedServer(t *testing.T, body, sig *string, down *atomic.Bool) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "down", 503)
			return
		}
		switch r.URL.Path {
		case "/c.yaml":
			w.Write([]byte(*body))
		case "/c.yaml.sig":
			w.Write([]byte(*sig))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/c.yaml"
}

func hasZig(c *catalog.Catalog) bool { _, ok := c.Command("zig"); return ok }

func TestNothingStoredIsTheBuiltInCatalog(t *testing.T) {
	s := &Syncer{Store: openStore(t)}
	c, body, err := s.Current(context.Background())
	if err != nil || body != nil || hasZig(c) {
		t.Fatalf("%v %q %v", c, body, err)
	}
	if _, ok := c.Command("npm"); !ok {
		t.Error("not the built-in catalog")
	}
	if err := s.Sync(context.Background()); err != nil {
		t.Errorf("no source is not an error: %v", err)
	}
}

func TestSyncStoresTheVerifiedFeedAndKeepsItWhenTheFeedGoesAway(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	body := zig
	sig := catalog.Sign(priv, []byte(body))
	var down atomic.Bool
	url := feedServer(t, &body, &sig, &down)
	s := &Syncer{Store: openStore(t), Source: &catalog.Source{
		URL: url, CacheDir: t.TempDir(), AllowInsecure: true, PublicKeys: []ed25519.PublicKey{pub}}}
	ctx := context.Background()

	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	c, got, err := s.Current(ctx)
	if err != nil || !hasZig(c) || string(got) != zig {
		t.Fatalf("after sync: zig=%v body=%q err=%v", hasZig(c), got, err)
	}
	if _, ok := c.Command("npm"); !ok {
		t.Error("the feed must overlay the built-in catalog")
	}

	// The feed is down, or serves something that doesn't verify: kept as it was.
	down.Store(true)
	if err := s.Sync(ctx); err != nil {
		t.Logf("sync while down: %v", err) // the cached copy may answer; either way the store keeps zig
	}
	down.Store(false)
	body = zig + "tools:\n  just: {image: just}\n" // new text, old signature
	if err := s.Sync(ctx); err == nil {
		t.Error("an unverifiable feed must be an error")
	}
	if c, _, _ = s.Current(ctx); !hasZig(c) {
		t.Error("the stored feed was lost")
	}
	if _, ok := c.Command("just"); ok {
		t.Error("an unverified feed was applied")
	}

	// A properly signed update replaces it.
	sig = catalog.Sign(priv, []byte(body))
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if c, _, _ = s.Current(ctx); !func() bool { _, ok := c.Command("just"); return ok }() {
		t.Error("update not stored")
	}
}

func TestAnUnusableStoredFeedFallsBackToBuiltIn(t *testing.T) {
	st := openStore(t)
	if err := st.PutDocument(context.Background(), documentName, []byte("toolchains: [broken")); err != nil {
		t.Fatal(err)
	}
	c, body, err := (&Syncer{Store: st}).Current(context.Background())
	if err == nil || body != nil || hasZig(c) {
		t.Fatalf("%v %q %v", c, body, err)
	}
	if _, ok := c.Command("npm"); !ok {
		t.Error("fallback is not the built-in catalog")
	}
}
