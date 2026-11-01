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
