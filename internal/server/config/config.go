// Package config reads the server's secrets. A secret comes from the
// environment variable NAME, or from the file named by NAME_FILE (which must
// not be readable by others); it never comes from a command-line flag, and
// nothing here logs a value.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Secret returns the value of the secret NAME. It is empty, with no error,
// when neither NAME nor NAME_FILE is set.
func Secret(name string) (string, error) {
	return secret(name, os.Getenv, os.Stat, os.ReadFile)
}

func secret(name string, getenv func(string) string, stat func(string) (os.FileInfo, error), read func(string) ([]byte, error)) (string, error) {
	direct, file := getenv(name), getenv(name+"_FILE")
	switch {
	case direct != "" && file != "":
		return "", fmt.Errorf("both %s and %s_FILE are set; use one", name, name)
	case direct != "":
		return strings.TrimSpace(direct), nil
	case file == "":
		return "", nil
	}
	info, err := stat(file)
	if err != nil {
		return "", fmt.Errorf("%s_FILE: %w", name, err)
	}
	if bad := info.Mode().Perm() & 0o077; bad != 0 && !inCredentialsDir(file, bad, getenv, stat) {
		return "", fmt.Errorf("%s_FILE: %s is readable by others (mode %o); make it 600", name, file, info.Mode().Perm())
	}
	b, err := read(file)
	if err != nil {
		return "", fmt.Errorf("%s_FILE: %w", name, err)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", errors.New(name + "_FILE: " + file + " is empty")
	}
	return v, nil
}

// OthersCanRead says whether a secret file is readable by users it should not
// be (see inCredentialsDir for the one case where group bits are fine).
func OthersCanRead(file string, info os.FileInfo) bool {
	bad := info.Mode().Perm() & 0o077
	return bad != 0 && !inCredentialsDir(file, bad, os.Getenv, os.Stat)
}

// inCredentialsDir says whether a group-readable file is one of systemd's
// credentials. systemd (257 at least) hands them over as root-owned files of
// mode 440 and admits the service's user with an ACL, whose mask shows as the
// group bits, so the mode alone cannot tell that from a file other users can
// read. What does tell it: the file sits directly in $CREDENTIALS_DIRECTORY
// (also mask-shown as 550), and both belong to root:root, so the group bits
// stand for root's group and the ACL's entry. Bits for "other" are never
// accepted.
func inCredentialsDir(file string, bad os.FileMode, getenv func(string) string, stat func(string) (os.FileInfo, error)) bool {
	dir := getenv("CREDENTIALS_DIRECTORY")
	if dir == "" || bad&0o007 != 0 || filepath.Dir(filepath.Clean(file)) != filepath.Clean(dir) {
		return false
	}
	d, err := stat(dir)
	if err != nil || !d.IsDir() || d.Mode().Perm()&0o007 != 0 {
		return false
	}
	f, err := stat(file)
	return err == nil && rootOwned(d) && rootOwned(f)
}

// rootOwned is a variable so tests, which cannot make root's files, can stand in.
var rootOwned = ownedByRoot

// ownedByRoot: with root as owner and group, the group bits (an ACL's mask)
// admit only root and whoever the ACL names.
func ownedByRoot(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0 && st.Gid == 0
}

// Required is Secret, but an unset secret is an error.
func Required(name string) (string, error) {
	v, err := Secret(name)
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", fmt.Errorf("%s (or %s_FILE) is not set", name, name)
	}
	return v, nil
}
