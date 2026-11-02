package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, mode os.FileMode, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSecretFromTheEnvironmentOrAFile(t *testing.T) {
	t.Setenv("RC_TEST_A", " from-env \n")
	if v, err := Secret("RC_TEST_A"); err != nil || v != "from-env" {
		t.Fatalf("env: %q, %v", v, err)
	}
	t.Setenv("RC_TEST_B_FILE", write(t, 0o600, "from-file\n"))
	if v, err := Secret("RC_TEST_B"); err != nil || v != "from-file" {
		t.Fatalf("file: %q, %v", v, err)
	}
	if v, err := Secret("RC_TEST_UNSET"); err != nil || v != "" {
		t.Fatalf("unset: %q, %v", v, err)
	}
	if _, err := Required("RC_TEST_UNSET"); err == nil {
		t.Fatal("Required accepted an unset secret")
	}
}

func TestAFileOthersCanReadIsRefused(t *testing.T) {
	t.Setenv("RC_TEST_C_FILE", write(t, 0o644, "x"))
	_, err := Secret("RC_TEST_C")
	if err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "x\n") {
		t.Fatal("the error must not hold the value")
	}
}

// systemd's credentials are mode 440 files (the service's user is admitted by an
// ACL) in a directory closed to everyone else.
func TestGroupReadableFileIsAcceptedOnlyInTheClosedCredentialsDirectory(t *testing.T) {
	dir := t.TempDir()
	put := func(mode os.FileMode) string {
		p := filepath.Join(dir, "cred")
		os.Remove(p)
		if err := os.WriteFile(p, []byte("v"), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, mode)
		return p
	}
	os.Chmod(dir, 0o700)
	t.Setenv("CREDENTIALS_DIRECTORY", dir)

	// Files of an ordinary user in a directory of the same user are not systemd's.
	t.Setenv("RC_TEST_G_FILE", put(0o440))
	if _, err := Secret("RC_TEST_G"); err == nil {
		t.Error("a group-readable file not owned by root was accepted")
	}
	old := rootOwned
	rootOwned = func(os.FileInfo) bool { return true } // as systemd's are
	defer func() { rootOwned = old }()

	t.Setenv("RC_TEST_G_FILE", put(0o440))
	if v, err := Secret("RC_TEST_G"); err != nil || v != "v" {
		t.Errorf("mode 440 in the closed credentials directory: %q, %v", v, err)
	}
	t.Setenv("RC_TEST_G_FILE", put(0o444))
	if _, err := Secret("RC_TEST_G"); err == nil {
		t.Error("a file others can read was accepted in the credentials directory")
	}
	t.Setenv("RC_TEST_G_FILE", put(0o440))
	os.Chmod(dir, 0o755)
	if _, err := Secret("RC_TEST_G"); err == nil {
		t.Error("accepted although the directory is open to everyone")
	}
	os.Chmod(dir, 0o700)
	// The same mode elsewhere is still refused, even with the variable set.
	other := write(t, 0o440, "v")
	t.Setenv("RC_TEST_G_FILE", other)
	if _, err := Secret("RC_TEST_G"); err == nil {
		t.Error("mode 440 outside the credentials directory was accepted")
	}
	// And without the variable the credentials directory is just a directory.
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	t.Setenv("RC_TEST_G_FILE", put(0o440))
	if _, err := Secret("RC_TEST_G"); err == nil {
		t.Error("accepted with CREDENTIALS_DIRECTORY unset")
	}
}

func TestBothSourcesOrAnEmptyFileAreErrors(t *testing.T) {
	t.Setenv("RC_TEST_D", "a")
	t.Setenv("RC_TEST_D_FILE", write(t, 0o600, "b"))
	if _, err := Secret("RC_TEST_D"); err == nil {
		t.Fatal("two sources for one secret were accepted")
	}
	t.Setenv("RC_TEST_E_FILE", write(t, 0o600, "\n"))
	if _, err := Secret("RC_TEST_E"); err == nil {
		t.Fatal("an empty file was accepted")
	}
	t.Setenv("RC_TEST_F_FILE", filepath.Join(t.TempDir(), "missing"))
	if _, err := Secret("RC_TEST_F"); err == nil {
		t.Fatal("a missing file was accepted")
	}
}
