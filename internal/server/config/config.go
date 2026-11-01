// Package config reads the server's secrets. A secret comes from the
// environment variable NAME, or from the file named by NAME_FILE (which must
// not be readable by others); it never comes from a command-line flag, and
// nothing here logs a value.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
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
	if info.Mode().Perm()&0o077 != 0 {
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
