package execute

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// internalDeps lists the packages of this module that the non-test files of
// the package in dir import, directly or not (by reading the files, so no
// build is needed).
func internalDeps(t *testing.T, dir string, seen map[string]bool) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			rest, ok := strings.CutPrefix(path, "github.com/Raccoon-CI/raccoonci/")
			if !ok || seen[path] {
				continue
			}
			seen[path] = true
			internalDeps(t, filepath.Join("..", "..", "..", filepath.FromSlash(rest)), seen)
		}
	}
}

// The backend must not depend on the runner side (S1, TODO D.3): none of the
// packages that run steps, scan, provision or download actions, directly or
// not, outside the server's own packages.
func TestServerDoesNotDependOnTheRunnerSide(t *testing.T) {
	seen := map[string]bool{}
	for _, dir := range []string{".", "../api", "../intake", "../schedule", "../imagesync", "../instsync", "../oidc", "../retention"} {
		internalDeps(t, dir, seen)
	}
	for _, bad := range []string{"runner", "engine", "scan", "provision", "actions", "docker", "local", "missing", "nodejs", "runnercmd"} {
		if seen["github.com/Raccoon-CI/raccoonci/internal/"+bad] {
			t.Errorf("the backend depends on internal/%s", bad)
		}
	}
}
