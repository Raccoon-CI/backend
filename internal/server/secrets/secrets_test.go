package secrets

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Raccoon-CI/backend/internal/server/store"
	"github.com/Raccoon-CI/backend/internal/server/vault"
)

func newService(t *testing.T) (*Service, *store.SQLite) {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	v, err := vault.New(map[string][]byte{"1": bytes.Repeat([]byte{1}, 32)}, "1")
	if err != nil {
		t.Fatal(err)
	}
	return &Service{Store: st, Vault: v}, st
}

const wfText = `on: push
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: deploy
        env:
          TOKEN: ${{ secrets.DEPLOY_TOKEN }}
          OTHER: ${{ secrets['org_key'] }}
          LOWER: ${{ secrets.lower_case }}
          NOTSET: ${{ secrets.NOT_SET }}
`

func TestReferencedFindsLiteralNamesOnly(t *testing.T) {
	got := Referenced(wfText + "      - run: echo ${{ secrets[matrix.name] }} ${{ toJSON(secrets) }} ${{ secrets.DEPLOY_TOKEN }} ${{ SECRETS.Upper }}\n      - run: echo mysecrets.fake\n")
	want := []string{"DEPLOY_TOKEN", "LOWER_CASE", "NOT_SET", "ORG_KEY", "UPPER"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestStoredSecretsAreSealedAndNeverListedWithTheirValues(t *testing.T) {
	s, st := newService(t)
	ctx := context.Background()
	if err := s.SetSecret(ctx, Repo("Acme/Web"), "deploy_token", "hunter2"); err != nil {
		t.Fatal(err)
	}
	raw, err := st.GetSecret(ctx, "repo:acme/web", "DEPLOY_TOKEN") // the name was put in capitals
	if err != nil || bytes.Contains(raw.Value, []byte("hunter2")) || raw.KeyID != "1" {
		t.Fatalf("stored = %+v, %v: the value must be sealed", raw, err)
	}
	s.SetVar(ctx, Repo("acme/web"), "mode", "fast")
	secrets, _ := s.List(ctx, "repo:acme/web", store.KindSecret)
	if len(secrets) != 1 || secrets[0].Name != "DEPLOY_TOKEN" || secrets[0].Value != "" {
		t.Fatalf("secrets = %+v", secrets)
	}
	vars, _ := s.List(ctx, "repo:acme/web", store.KindVar)
	if len(vars) != 1 || vars[0].Name != "MODE" || vars[0].Value != "fast" {
		t.Fatalf("vars = %+v", vars)
	}
	// A variable is not deleted as a secret, nor the other way round.
	if ok, _ := s.Delete(ctx, "repo:acme/web", store.KindVar, "DEPLOY_TOKEN"); ok {
		t.Fatal("a secret was deleted as a variable")
	}
	if ok, _ := s.Delete(ctx, "repo:acme/web", store.KindSecret, "deploy_token"); !ok {
		t.Fatal("the secret was not deleted")
	}
}

func TestARunGetsTheSecretsItNamesRepositoryOverOrganizationAndNoneFromAFork(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	s.SetSecret(ctx, Org("acme"), "DEPLOY_TOKEN", "from the org")
	s.SetSecret(ctx, Repo("acme/web"), "DEPLOY_TOKEN", "from the repo")
	s.SetSecret(ctx, Org("acme"), "ORG_KEY", "org only")
	s.SetSecret(ctx, Repo("acme/web"), "NOT_REFERENCED", "never sent")
	s.SetSecret(ctx, Repo("acme/api"), "LOWER_CASE", "another repository's")
	s.SetVar(ctx, Org("acme"), "REGION", "eu")
	s.SetVar(ctx, Repo("acme/web"), "REGION", "us")

	run, err := s.ForRun(ctx, "Acme/Web", wfText, false)
	if err != nil {
		t.Fatal(err)
	}
	if run.Secrets["DEPLOY_TOKEN"] != "from the repo" || run.Secrets["ORG_KEY"] != "org only" || len(run.Secrets) != 2 {
		t.Fatalf("secrets = %v: the repository's wins, only named ones, nothing of another repository", run.Secrets)
	}
	if run.Vars["REGION"] != "us" || len(run.Vars) != 1 {
		t.Fatalf("vars = %v", run.Vars)
	}
	fork, err := s.ForRun(ctx, "acme/web", wfText, true)
	if err != nil || len(fork.Secrets) != 0 {
		t.Fatalf("a fork got secrets: %v, %v", fork.Secrets, err)
	}
	if fork.Vars["REGION"] != "us" {
		t.Fatalf("a fork should still get variables: %v", fork.Vars)
	}
}

func TestASecretThatCannotBeOpenedIsLeftOutWithAWarningThatHasNoValue(t *testing.T) {
	s, st := newService(t)
	ctx := context.Background()
	s.SetSecret(ctx, Repo("acme/web"), "DEPLOY_TOKEN", "hunter2")
	s.SetSecret(ctx, Repo("acme/web"), "ORG_KEY", "fine")
	// The row was sealed for another name: copied by hand.
	a, _ := st.GetSecret(ctx, "repo:acme/web", "DEPLOY_TOKEN")
	st.PutSecret(ctx, store.Secret{Scope: "repo:acme/web", Name: "LOWER_CASE", Kind: store.KindSecret, KeyID: a.KeyID, Value: a.Value})
	run, err := s.ForRun(ctx, "acme/web", wfText, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := run.Secrets["LOWER_CASE"]; ok || run.Secrets["DEPLOY_TOKEN"] != "hunter2" {
		t.Fatalf("secrets = %v", run.Secrets)
	}
	if len(run.Warnings) != 1 || !strings.Contains(run.Warnings[0], "LOWER_CASE") || strings.Contains(run.Warnings[0], "hunter2") {
		t.Fatalf("warnings = %v", run.Warnings)
	}
}

func TestWhatTheServerKeepsForItselfIsNeverGivenToARun(t *testing.T) {
	s, st := newService(t)
	ctx := context.Background()
	keyID, sealed, _ := s.Vault.Seal("org:acme", "DEPLOY_TOKEN", []byte("bucket keys"))
	st.PutSecret(ctx, store.Secret{Scope: "org:acme", Name: "DEPLOY_TOKEN", Kind: store.KindStorage, KeyID: keyID, Value: sealed})
	run, err := s.ForRun(ctx, "acme/web", wfText, false)
	if err != nil || len(run.Secrets) != 0 || len(run.Vars) != 0 {
		t.Fatalf("a storage setting reached a run: %v %v, %v", run.Secrets, run.Vars, err)
	}
	for _, kind := range []string{store.KindSecret, store.KindVar} {
		if list, _ := s.List(ctx, "org:acme", kind); len(list) != 0 {
			t.Fatalf("a storage setting listed as %s: %+v", kind, list)
		}
	}
}

func TestBadScopesNamesAndValuesAreRefused(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	for _, c := range []struct{ scope, name, value string }{
		{"repo:acme/web", "1BAD", "v"}, {"repo:acme/web", "has space", "v"}, {"repo:acme/web", "GITHUB_TOKEN", "v"},
		{"repo:acme/web", "github_x", "v"}, {"repo:acme/web", strings.Repeat("A", 101), "v"}, {"repo:acme/web", "OK", ""},
		{"repo:acme/web", "OK", strings.Repeat("x", MaxValue+1)},
		{"acme/web", "OK", "v"}, {"repo:Acme/Web", "OK", "v"}, {"org:", "OK", "v"}, {"repo:a/b/c", "OK", "v"}, {"org:../x", "OK", "v"},
	} {
		if err := s.SetSecret(ctx, c.scope, c.name, c.value); !errors.Is(err, ErrInvalid) {
			t.Errorf("SetSecret(%q, %q, %d bytes): %v", c.scope, c.name, len(c.value), err)
		}
	}
	if err := s.SetVar(ctx, "repo:acme/web", "OK", ""); err != nil {
		t.Errorf("an empty variable is fine: %v", err)
	}
	if _, err := s.List(ctx, "bad", store.KindSecret); !errors.Is(err, ErrInvalid) {
		t.Errorf("listing a bad scope: %v", err)
	}
}
