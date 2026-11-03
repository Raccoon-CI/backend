package storage

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Raccoon-CI/backend/internal/server/secrets"
	"github.com/Raccoon-CI/backend/internal/server/store"
	"github.com/Raccoon-CI/backend/internal/server/vault"
	"github.com/Raccoon-CI/lib/pkg/blob"
)

type env struct {
	svc    *Service
	st     *store.SQLite
	checks []blob.S3Config
	fail   error
	warn   []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	v, _ := vault.New(map[string][]byte{"1": bytes.Repeat([]byte{1}, 32)}, "1")
	e := &env{st: st}
	e.svc = &Service{Store: st, Vault: v, Default: &blob.S3Config{Bucket: "server-bucket"},
		Check: func(_ context.Context, cfg blob.S3Config) ([]string, error) {
			e.checks = append(e.checks, cfg)
			return e.warn, e.fail
		}}
	return e
}

var good = Settings{Endpoint: "https://acct.r2.cloudflarestorage.com", Region: "auto", Bucket: "acme-ci", Prefix: "raccoon", AccessKey: "AKIAEXAMPLE", SecretKey: "very-secret-key-material"}

func TestAnOrganizationsBucketIsCheckedSealedAndUsedForItsRuns(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cfg, ver, err := e.svc.For(ctx, "Acme")
	if err != nil || cfg == nil || cfg.Bucket != "server-bucket" || ver != "default" {
		t.Fatalf("before: %+v %q %v: an organization with none uses the server's", cfg, ver, err)
	}
	e.warn = []string{"the bucket gives its objects to anyone"}
	warnings, err := e.svc.Put(ctx, "Acme", good)
	if err != nil || len(warnings) != 1 {
		t.Fatalf("put: %v, %v", warnings, err)
	}
	if len(e.checks) != 1 || e.checks[0].Bucket != "acme-ci" || e.checks[0].Prefix != "raccoon/" || e.checks[0].SecretKey != good.SecretKey {
		t.Fatalf("the bucket was checked as %+v", e.checks)
	}
	raw, err := e.st.GetSecret(ctx, "org:acme", settingName)
	if err != nil || raw.Kind != store.KindStorage || bytes.Contains(raw.Value, []byte("very-secret-key-material")) || bytes.Contains(raw.Value, []byte("acme-ci")) {
		t.Fatalf("stored = %+v, %v: the settings must be sealed, keys and all", raw, err)
	}
	cfg, ver, err = e.svc.For(ctx, "acme")
	if err != nil || cfg == nil || cfg.Bucket != "acme-ci" || cfg.AccessKey != "AKIAEXAMPLE" || cfg.Prefix != "raccoon/" || ver == "default" {
		t.Fatalf("after: %+v %q %v", cfg, ver, err)
	}
	view, err := e.svc.View(ctx, "ACME")
	if err != nil || !view.Configured || view.Bucket != "acme-ci" || view.Endpoint != good.Endpoint {
		t.Fatalf("view = %+v, %v", view, err)
	}
	// The version changes when the settings do.
	e.warn = nil
	again := good
	again.Bucket = "acme-ci-2"
	e.st.Now = func() time.Time { return time.Now().Add(time.Second) }
	if _, err := e.svc.Put(ctx, "acme", again); err != nil {
		t.Fatal(err)
	}
	if _, ver2, _ := e.svc.For(ctx, "acme"); ver2 == ver {
		t.Fatalf("the version did not change: %q", ver)
	}
	if ok, _ := e.svc.Delete(ctx, "acme"); !ok {
		t.Fatal("not deleted")
	}
	if cfg, ver, _ := e.svc.For(ctx, "acme"); cfg.Bucket != "server-bucket" || ver != "default" {
		t.Fatalf("after delete: %+v %q", cfg, ver)
	}
	if v, _ := e.svc.View(ctx, "acme"); v.Configured {
		t.Fatal("still configured after delete")
	}
}

func TestTheServersOwnBucketCanBeSavedInTheDatabaseAndWinsOverTheStartOnes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// Saved like an organization's: tried first, sealed with keys and all.
	server := Settings{Endpoint: "https://acct.r2.cloudflarestorage.com", Region: "auto", Bucket: "shared-ci", AccessKey: "AKIASERVER", SecretKey: "server-secret-key-material"}
	if _, err := e.svc.Put(ctx, ServerDefault, server); err != nil {
		t.Fatal(err)
	}
	if len(e.checks) != 1 || e.checks[0].Bucket != "shared-ci" {
		t.Fatalf("checked as %+v", e.checks)
	}
	raw, err := e.st.GetSecret(ctx, "server", settingName)
	if err != nil || raw.Kind != store.KindStorage || bytes.Contains(raw.Value, []byte("server-secret-key-material")) {
		t.Fatalf("stored = %+v, %v: must be sealed", raw, err)
	}
	// An organization with none now uses it, not the one the server started with.
	cfg, ver, err := e.svc.For(ctx, "acme")
	if err != nil || cfg == nil || cfg.Bucket != "shared-ci" || cfg.AccessKey != "AKIASERVER" || !strings.HasPrefix(ver, "server-") {
		t.Fatalf("for acme: %+v %q %v", cfg, ver, err)
	}
	// One with its own bucket still uses its own.
	e.svc.Put(ctx, "acme", good)
	if cfg, _, _ := e.svc.For(ctx, "acme"); cfg.Bucket != "acme-ci" {
		t.Fatalf("an organization's own bucket lost to the server's: %+v", cfg)
	}
	if view, err := e.svc.View(ctx, ServerDefault); err != nil || !view.Configured || view.Bucket != "shared-ci" {
		t.Fatalf("view = %+v, %v", view, err)
	}
	// The version moves with the settings.
	e.st.Now = func() time.Time { return time.Now().Add(time.Second) }
	server.Bucket = "shared-ci-2"
	e.svc.Put(ctx, ServerDefault, server)
	if _, ver2, _ := e.svc.For(ctx, "other"); ver2 == ver {
		t.Fatalf("the version did not change: %q", ver2)
	}
	// Deleted, the start-up bucket is the default again.
	if ok, _ := e.svc.Delete(ctx, ServerDefault); !ok {
		t.Fatal("not deleted")
	}
	if cfg, ver, _ := e.svc.For(ctx, "other"); cfg.Bucket != "server-bucket" || ver != "default" {
		t.Fatalf("after delete: %+v %q", cfg, ver)
	}
	// A bucket that does not work is not saved as the server's either.
	e.fail = errors.New("no such bucket")
	if _, err := e.svc.Put(ctx, ServerDefault, server); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("a bad server bucket: %v", err)
	}
	if _, err := e.st.GetSecret(ctx, "server", settingName); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a bucket that failed its check was kept")
	}
}

func TestABucketThatDoesNotWorkIsNotSaved(t *testing.T) {
	e := newEnv(t)
	e.fail = errors.New("writing an object: access denied")
	_, err := e.svc.Put(context.Background(), "acme", good)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "access denied") || strings.Contains(err.Error(), good.SecretKey) {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.st.GetSecret(context.Background(), "org:acme", settingName); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("settings of a bucket that does not work were saved")
	}
}

func TestBadSettingsAreRefusedBeforeAnythingIsTried(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for name, edit := range map[string]func(*Settings){
		"no bucket": func(s *Settings) { s.Bucket = "" }, "bad bucket": func(s *Settings) { s.Bucket = "A_B" },
		"no key": func(s *Settings) { s.AccessKey = "" }, "no secret": func(s *Settings) { s.SecretKey = "" },
		"http endpoint": func(s *Settings) { s.Endpoint = "http://example.com" }, "long prefix": func(s *Settings) { s.Prefix = strings.Repeat("p", 300) },
	} {
		s := good
		edit(&s)
		if _, err := e.svc.Put(ctx, "acme", s); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := e.svc.Put(ctx, "../x", good); !errors.Is(err, ErrInvalid) {
		t.Errorf("a bad organization name: %v", err)
	}
	if len(e.checks) != 0 {
		t.Fatalf("a bucket was tried though the settings were bad: %v", e.checks)
	}
}

// The setting is in the organization's scope with the secrets; a workflow and
// the secrets API must not be able to reach it.
func TestTheSettingsCannotBeReachedAsASecret(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.Put(ctx, "acme", good); err != nil {
		t.Fatal(err)
	}
	ss := &secrets.Service{Store: e.st, Vault: e.svc.Vault}
	for _, name := range []string{"$STORAGE", "STORAGE", "_STORAGE"} {
		if name == "$STORAGE" {
			if err := ss.SetSecret(ctx, "org:acme", name, "x"); !errors.Is(err, secrets.ErrInvalid) {
				t.Errorf("a secret named %s was accepted: %v", name, err)
			}
		}
	}
	run, err := ss.ForRun(ctx, "acme/web", "${{ secrets.STORAGE }} ${{ secrets['$STORAGE'] }}", false)
	if err != nil || len(run.Secrets) != 0 {
		t.Fatalf("a run got %v, %v", run.Secrets, err)
	}
	if ok, _ := ss.Delete(ctx, "org:acme", store.KindSecret, "STORAGE"); ok {
		t.Fatal("the secrets API deleted something")
	}
	if cfg, _, _ := e.svc.For(ctx, "acme"); cfg == nil || cfg.Bucket != "acme-ci" {
		t.Fatal("the settings were touched")
	}
}
