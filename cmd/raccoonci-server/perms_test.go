package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Raccoon-CI/raccoonci/internal/server/store"
)

func modeOf(t *testing.T, p string) os.FileMode {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestANewDatabaseAndItsWalFilesAreMadeReadableByTheServerAlone(t *testing.T) {
	old := syscall.Umask(0o022) // the usual umask, which would make them 644
	defer syscall.Umask(old)
	db := filepath.Join(t.TempDir(), "server.db")
	if err := privateDatabase(db, slog.Default()); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Write something, so that the -wal and -shm files exist.
	if _, _, err := st.RecordEvent(t.Context(), store.Event{DeliveryID: "d", Kind: "push", Repo: "o/r", Payload: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{db, db + "-wal", db + "-shm"} {
		if m := modeOf(t, p); m&0o077 != 0 {
			t.Errorf("%s has mode %o; others can read it", filepath.Base(p), m)
		}
	}
}

func TestADatabaseAnOlderStartLeftOpenIsTightened(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "server.db")
	for _, p := range []string{db, db + "-wal", db + "-shm"} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, 0o644) // whatever the umask was
	}
	var logged bytes.Buffer
	if err := privateDatabase(db, slog.New(slog.NewTextHandler(&logged, nil))); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{db, db + "-wal", db + "-shm"} {
		if m := modeOf(t, p); m != 0o600 {
			t.Errorf("%s has mode %o, want 600", filepath.Base(p), m)
		}
	}
	if !strings.Contains(logged.String(), "readable by others") {
		t.Errorf("the change was not logged: %q", logged.String())
	}
	// A database that is already private is left alone and nothing is logged.
	logged.Reset()
	privateDatabase(db, slog.New(slog.NewTextHandler(&logged, nil)))
	if logged.Len() != 0 {
		t.Errorf("a private database was reported: %q", logged.String())
	}
}

func TestAnOpenDataDirectoryIsWarnedAboutAndNotChanged(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0o755)
	var logged bytes.Buffer
	warnOpenDirectory(dir, slog.New(slog.NewTextHandler(&logged, nil)))
	if !strings.Contains(logged.String(), "make it mode 700") {
		t.Errorf("no warning for a 755 directory: %q", logged.String())
	}
	if modeOf(t, dir) != 0o755 {
		t.Error("the operator's directory was changed")
	}
	os.Chmod(dir, 0o700)
	logged.Reset()
	warnOpenDirectory(dir, slog.New(slog.NewTextHandler(&logged, nil)))
	if logged.Len() != 0 {
		t.Errorf("a private directory was reported: %q", logged.String())
	}
}
