package execute

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"raccoonci/internal/server/secrets"
	"raccoonci/internal/server/store"
	"raccoonci/internal/server/vault"
)

const echoSecret = `on: push
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: echo "token=[$TOKEN] region=[${{ vars.REGION }}]"
        env:
          TOKEN: ${{ secrets.DEPLOY_TOKEN }}
`

// tarSource is a Source that serves a repository holding just a workflow.
type tarSource struct{ files map[string]string }

func (s tarSource) SourceTar(_ context.Context, _ int64, _, _ string, w io.Writer) error {
	zw := gzip.NewWriter(w)
	tw := tar.NewWriter(zw)
	for name, body := range s.files {
		if err := tw.WriteHeader(&tar.Header{Name: "src/" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	return zw.Close()
}

const echoGithubToken = `on: push
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: |
          test "$A" = "$B" && echo "same=yes"
          test -n "$A" && echo "length=${#A}"
          echo "printed=[$A]"
        env:
          A: ${{ github.token }}
          B: ${{ secrets.GITHUB_TOKEN }}
`

// With the real runner, the GITHUB_TOKEN the server makes for a job is what
// `github.token` and `secrets.GITHUB_TOKEN` give a step, and it is masked.
func TestRealRunnerGivesAJobItsGithubTokenAndMasksIt(t *testing.T) {
	bin := os.Getenv("RACCOONCI_TEST_BIN")
	if bin == "" {
		t.Skip("set RACCOONCI_TEST_BIN to a raccoonci binary to run this")
	}
	e := newEnv(t)
	e.ex.Machine = Local{Binary: bin, Dir: t.TempDir()}
	e.ex.Source = tarSource{files: map[string]string{".github/workflows/ci.yml": echoGithubToken}}
	e.ex.Install = "none"
	e.ex.Tokens = &fakeMinter{}
	run := e.queue(t, "t", echoGithubToken, nil)
	e.start(t)
	if done := e.wait(t, run.ID); done.Conclusion != "success" {
		t.Fatalf("%q; log: %+v", done.Conclusion, e.log(t, run.ID))
	}
	var out []string
	for _, ev := range e.log(t, run.ID) {
		if strings.Contains(ev.Text, "ghs_job") {
			t.Fatalf("the token is in the log: %+v", ev)
		}
		out = append(out, ev.Text)
	}
	got := strings.Join(out, "\n")
	for _, want := range []string{"same=yes", "length=", "printed=[***]"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in the log:\n%s", want, got)
		}
	}
}

// This runs the real `raccoonci run-instance`: set RACCOONCI_TEST_BIN to a
// built raccoonci (make build; dist/raccoonci). It shows that a secret reaches
// the step, is masked in everything the step prints, and that a fork's run
// gets an empty one.
func TestRealRunnerMasksTheSecretsItIsGiven(t *testing.T) {
	bin := os.Getenv("RACCOONCI_TEST_BIN")
	if bin == "" {
		t.Skip("set RACCOONCI_TEST_BIN to a raccoonci binary to run this")
	}
	e := newEnv(t)
	e.ex.Machine = Local{Binary: bin, Dir: t.TempDir()}
	e.ex.Source = tarSource{files: map[string]string{".github/workflows/ci.yml": echoSecret}}
	e.ex.Install = "none"
	v, _ := vault.New(map[string][]byte{"1": bytes.Repeat([]byte{1}, 32)}, "1")
	svc := &secrets.Service{Store: e.st, Vault: v}
	ctx := context.Background()
	svc.SetSecret(ctx, secrets.Repo("acme/web"), "DEPLOY_TOKEN", "hunter2-not-for-logs")
	svc.SetVar(ctx, secrets.Org("acme"), "REGION", "eu")
	e.ex.Secrets = svc

	normal := e.queue(t, "n", echoSecret, nil)
	fork := e.queue(t, "f", echoSecret, func(r *store.Run) { r.Fork = true; r.WorkflowPath = ".github/workflows/ci.yml" })
	e.start(t)
	for _, run := range []store.Run{normal, fork} {
		if done := e.wait(t, run.ID); done.Conclusion != "success" {
			t.Fatalf("run %d: %q; log: %+v", run.ID, done.Conclusion, e.log(t, run.ID))
		}
	}
	var normalOut, forkOut string
	for _, ev := range e.log(t, normal.ID) {
		if strings.Contains(ev.Text, "hunter2") {
			t.Fatalf("the secret is in the log: %+v", ev)
		}
		if strings.HasPrefix(ev.Text, "token=") {
			normalOut = ev.Text
		}
	}
	for _, ev := range e.log(t, fork.ID) {
		if strings.HasPrefix(ev.Text, "token=") {
			forkOut = ev.Text
		}
	}
	if normalOut != "token=[***] region=[eu]" {
		t.Errorf("normal run printed %q, want the secret masked and the variable shown", normalOut)
	}
	if forkOut != "token=[] region=[eu]" {
		t.Errorf("fork run printed %q, want an empty secret and the variable", forkOut)
	}
}
