package execute

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Raccoon-CI/raccoonci/pkg/dispatch"
)

// script writes an executable shell script standing in for raccoonci.
func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "raccoonci")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func sourceWith(files map[string]string) func(io.Writer) error {
	return func(w io.Writer) error {
		zw := gzip.NewWriter(w)
		tw := tar.NewWriter(zw)
		for name, body := range files {
			tw.WriteHeader(&tar.Header{Name: "src/" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
			tw.Write([]byte(body))
		}
		tw.Close()
		return zw.Close()
	}
}

func TestLocalRunsTheCommandInTheUnpackedSourceWithTheInputAndEnvironment(t *testing.T) {
	bin := script(t, `echo "args: $*"; echo "dir has: $(ls)"; echo "stdin: $(cat)"; echo "env: $MARK $SECRET1"; echo "err" >&2; exit 3`)
	var log bytes.Buffer
	res, err := Local{Binary: bin, Dir: t.TempDir()}.Run(context.Background(), "rc-r1-1", dispatch.Job{
		Command: "run-instance", Input: []byte("the-instance"),
		Env: map[string]string{"MARK": "m"}, Secrets: map[string]string{"SECRET1": "s1"},
		Source: sourceWith(map[string]string{"README.md": "hi"}), Log: &log,
	})
	if err != nil || res.ExitCode != 3 || res.Error != "" {
		t.Fatalf("res = %+v, %v", res, err)
	}
	out := log.String()
	for _, want := range []string{"args: run-instance -secret SECRET1", "dir has: README.md", "stdin: the-instance", "env: m s1", "err"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestLocalRemovesItsDirectoryAndReportsWhatCannotStart(t *testing.T) {
	dir := t.TempDir()
	if _, err := (Local{Binary: script(t, "exit 0"), Dir: dir}).Run(context.Background(), "x", dispatch.Job{Source: sourceWith(nil)}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left %v", entries)
	}
	res, err := Local{Binary: filepath.Join(dir, "missing"), Dir: dir}.Run(context.Background(), "x", dispatch.Job{Source: sourceWith(nil)})
	if err != nil || !strings.Contains(res.Error, "starting") {
		t.Fatalf("a binary that cannot start: %+v, %v", res, err)
	}
	bad := func(io.Writer) error { return io.ErrUnexpectedEOF }
	res, err = Local{Binary: script(t, "exit 0"), Dir: dir}.Run(context.Background(), "x", dispatch.Job{Source: bad})
	if err != nil || !strings.Contains(res.Error, "unpacking") {
		t.Fatalf("a source that cannot be read: %+v, %v", res, err)
	}
}

func TestLocalInterruptsAJobWhenCancelled(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "interrupted")
	bin := script(t, `trap 'echo got-int > `+marker+`; exit 0' INT; echo ready; while true; do sleep 0.05; done`)
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	log := &signalWriter{ready: ready}
	done := make(chan error, 1)
	go func() {
		_, err := Local{Binary: bin, Dir: t.TempDir()}.Run(ctx, "x", dispatch.Job{Source: sourceWith(nil), Log: log})
		done <- err
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("the job never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the job was killed instead of interrupted: its trap did not run")
	}
}

// signalWriter closes ready when it first sees output.
type signalWriter struct {
	ready chan struct{}
	once  bool
}

func (w *signalWriter) Write(p []byte) (int, error) {
	if !w.once {
		w.once = true
		close(w.ready)
	}
	return len(p), nil
}
