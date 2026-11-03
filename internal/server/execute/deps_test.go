package execute

import (
	"os/exec"
	"strings"
	"testing"
)

// The backend must not depend on the runner or the cli (S1, TODO D.3). They are
// separate modules now, so the check is on `go list -deps` of every package of
// this module, tests included: no package of raccoonci-runner or cli
// may be among them.
func TestBackendDoesNotDependOnRunnerOrCLI(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-test", "-f", "{{.ImportPath}}", "./...")
	cmd.Dir = "../../.."
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("go list: %v", err)
	}
	for _, pkg := range strings.Fields(string(out)) {
		for _, bad := range []string{"github.com/Raccoon-CI/runner", "github.com/Raccoon-CI/cli"} {
			if pkg == bad || strings.HasPrefix(pkg, bad+"/") {
				t.Errorf("the backend depends on %s", pkg)
			}
		}
	}
}
