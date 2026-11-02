package github

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRealCheckRunAnnotations sends a check run with annotations (one for a file,
// one without a place, as the server does) to real GitHub and reads them back.
// It leaves one completed neutral check run named "raccoonci annotation probe"
// on RACCOONCI_TEST_SHA, which GitHub does not let anyone delete. Same
// variables as TestRealAppTokens.
func TestRealCheckRunAnnotations(t *testing.T) {
	id, keyFile, repo, sha := os.Getenv("RACCOONCI_TEST_APP_ID"), os.Getenv("RACCOONCI_TEST_APP_KEY_FILE"),
		os.Getenv("RACCOONCI_TEST_REPO"), os.Getenv("RACCOONCI_TEST_SHA")
	if id == "" || keyFile == "" || repo == "" || sha == "" {
		t.Skip("set RACCOONCI_TEST_APP_ID, _APP_KEY_FILE, _REPO and _SHA to run against real GitHub")
	}
	var appID int64
	for _, c := range id {
		appID = appID*10 + int64(c-'0')
	}
	pem, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ParsePrivateKey(pem)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{ID: appID, Key: key}
	ctx := context.Background()
	inst, err := app.InstallationOf(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{Tokens: app}
	now := time.Now()
	checkID, err := c.CreateCheckRun(ctx, inst, repo, sha, CheckRun{Name: "raccoonci annotation probe", Status: CheckInProgress, StartedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	err = c.UpdateCheckRun(ctx, inst, repo, checkID, CheckRun{
		Name: "raccoonci annotation probe", Status: CheckCompleted, Conclusion: "neutral", CompletedAt: now,
		Title: "Annotation probe", Summary: "## Probe\n\nA check run with two annotations, made by the real-GitHub test.",
		Annotations: []Annotation{
			{Path: "README.md", StartLine: 1, EndLine: 2, Level: LevelWarning, Title: "A file", Message: "with a place"},
			{Path: ".github", StartLine: 1, EndLine: 1, Level: LevelNotice, Message: "without one"},
		},
	})
	if err != nil {
		t.Fatalf("GitHub refused the annotations: %v", err)
	}
	var got []struct {
		Path, Message string
		Level         string `json:"annotation_level"`
		Title         string
	}
	if err := c.get(ctx, inst, "/repos/"+repo+"/check-runs/"+strconv.FormatInt(checkID, 10)+"/annotations", &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Path != "README.md" || got[0].Level != "warning" || got[0].Title != "A file" || got[1].Path != ".github" || got[1].Message != "without one" {
		t.Fatalf("annotations read back: %+v", got)
	}
}

// TestRealAppTokens checks, against real GitHub, what a job's GITHUB_TOKEN is:
// scoped to one repository, limited to the permissions asked for, and dead
// after RevokeToken. It needs a registered App:
//
//	RACCOONCI_TEST_APP_ID, RACCOONCI_TEST_APP_KEY_FILE   the App
//	RACCOONCI_TEST_REPO       owner/name the App is installed on (the token's repository)
//	RACCOONCI_TEST_OTHER_REPO owner/name of another private repository of the same installation
//	RACCOONCI_TEST_SHA        a commit of RACCOONCI_TEST_REPO (a status named "raccoonci token probe" is written on it)
//
// Nothing secret is printed.
func TestRealAppTokens(t *testing.T) {
	id, keyFile, repo, other, sha := os.Getenv("RACCOONCI_TEST_APP_ID"), os.Getenv("RACCOONCI_TEST_APP_KEY_FILE"),
		os.Getenv("RACCOONCI_TEST_REPO"), os.Getenv("RACCOONCI_TEST_OTHER_REPO"), os.Getenv("RACCOONCI_TEST_SHA")
	if id == "" || keyFile == "" || repo == "" || other == "" || sha == "" {
		t.Skip("set RACCOONCI_TEST_APP_ID, _APP_KEY_FILE, _REPO, _OTHER_REPO and _SHA to run against real GitHub")
	}
	var appID int64
	for _, c := range id {
		appID = appID*10 + int64(c-'0')
	}
	pem, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ParsePrivateKey(pem)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{ID: appID, Key: key}
	ctx := context.Background()
	inst, err := app.InstallationOf(ctx, repo)
	if err != nil {
		t.Fatalf("installation of %s: %v", repo, err)
	}

	call := func(token, method, path, body string) int {
		req, _ := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	status := `{"state":"success","context":"raccoonci token probe"}`

	ro, err := app.RunToken(ctx, inst, repo, map[string]string{"contents": "read", "metadata": "read"})
	if err != nil {
		t.Fatal(err)
	}
	if c := call(ro, "GET", "/repos/"+repo, ""); c != 200 {
		t.Errorf("read the run's repository: %d, want 200", c)
	}
	if c := call(ro, "GET", "/repos/"+other, ""); c != 404 {
		t.Errorf("read another private repository of the installation: %d, want 404 (the token must be scoped to one repository)", c)
	}
	if c := call(ro, "POST", "/repos/"+repo+"/statuses/"+sha, status); c != 403 {
		t.Errorf("write a status with a read-only token: %d, want 403", c)
	}

	rw, err := app.RunToken(ctx, inst, repo, map[string]string{"statuses": "write", "metadata": "read"})
	if err != nil {
		t.Fatal(err)
	}
	if c := call(rw, "POST", "/repos/"+repo+"/statuses/"+sha, status); c != 201 {
		t.Errorf("write a status with statuses: write: %d, want 201", c)
	}
	if c := call(rw, "GET", "/repos/"+repo+"/contents/README.md", ""); c != 403 && c != 404 {
		t.Errorf("read contents with statuses only: %d, want 403 or 404", c)
	}

	// A permission the App does not have is refused, which is what the
	// executor's fallback depends on.
	if _, err := app.RunToken(ctx, inst, repo, map[string]string{"packages": "write"}); err == nil {
		t.Error("a permission the App lacks was granted; the fallback path would never run")
	}

	if err := app.RevokeToken(ctx, ro); err != nil {
		t.Fatal(err)
	}
	// GitHub takes a moment to make a revocation count: poll before judging.
	var c int
	for i := 0; i < 20; i++ {
		if c = call(ro, "GET", "/repos/"+repo, ""); c == 401 {
			t.Logf("the revoked token stopped working after %d checks", i+1)
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if c != 401 {
		t.Errorf("a revoked token still answers %d after 10 s, want 401", c)
	}
	if err := app.RevokeToken(ctx, ro); err != nil {
		t.Errorf("revoking a token twice must not fail: %v", err)
	}
	app.RevokeToken(ctx, rw)
}
