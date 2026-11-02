package github

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Raccoon-CI/raccoonci/internal/archive"
)

type entry struct {
	name     string
	body     string
	typeflag byte
	link     string
}

func makeTarGz(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Typeflag: tf, Mode: 0o644, Size: int64(len(e.body)), Linkname: e.link}
		if tf == tar.TypeDir {
			hdr.Mode, hdr.Size = 0o755, 0
		}
		if tf == tar.TypeXGlobalHeader {
			hdr = &tar.Header{Typeflag: tf, PAXRecords: map[string]string{"comment": "abc"}}
		}
		if tf == tar.TypeSymlink || tf == tar.TypeLink {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func readTarGz(t *testing.T, b []byte) map[string]string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		switch hdr.Typeflag {
		case tar.TypeSymlink, tar.TypeLink:
			out[hdr.Name] = "-> " + hdr.Linkname
		case tar.TypeDir:
			out[hdr.Name] = "<dir>"
		default:
			out[hdr.Name] = string(data)
		}
	}
}

func TestRepackDropsTheTopDirectoryAndTheGlobalHeader(t *testing.T) {
	src := makeTarGz(t, []entry{
		{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader},
		{name: "acme-web-abc123/", typeflag: tar.TypeDir},
		{name: "acme-web-abc123/main.go", body: "package main\n"},
		{name: "acme-web-abc123/.github/", typeflag: tar.TypeDir},
		{name: "acme-web-abc123/.github/workflows/ci.yml", body: "on: push\n"},
		{name: "acme-web-abc123/link", typeflag: tar.TypeSymlink, link: "main.go"},
		{name: "acme-web-abc123/hard", typeflag: tar.TypeLink, link: "acme-web-abc123/main.go"},
	})
	var out bytes.Buffer
	if err := Repack(&out, bytes.NewReader(src), 1<<20); err != nil {
		t.Fatal(err)
	}
	got := readTarGz(t, out.Bytes())
	want := map[string]string{
		"src/.github/":                 "<dir>",
		"src/main.go":                  "package main\n",
		"src/.github/workflows/ci.yml": "on: push\n",
		"src/link":                     "-> main.go",
		"src/hard":                     "-> src/main.go",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// The agent's extractor is the real consumer: what comes out must be the
// repository's own tree, with the workflow where GitHub has it.
func TestRepackedArchiveUnpacksAsTheRepositoryTree(t *testing.T) {
	src := makeTarGz(t, []entry{
		{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader},
		{name: "acme-web-abc123/", typeflag: tar.TypeDir},
		{name: "acme-web-abc123/main.go", body: "package main\n"},
		{name: "acme-web-abc123/.github/", typeflag: tar.TypeDir},
		{name: "acme-web-abc123/.github/workflows/ci.yml", body: "on: push\n"},
		{name: "acme-web-abc123/link", typeflag: tar.TypeSymlink, link: "main.go"},
		{name: "acme-web-abc123/hard", typeflag: tar.TypeLink, link: "acme-web-abc123/main.go"},
	})
	var out bytes.Buffer
	if err := Repack(&out, bytes.NewReader(src), 1<<20); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := archive.ExtractTarGz(&out, dir, 1<<20); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"main.go": "package main\n", ".github/workflows/ci.yml": "on: push\n", "link": "package main\n", "hard": "package main\n"} {
		got, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
}

func TestRepackRefusesEscapingNamesAndHugeTrees(t *testing.T) {
	escape := makeTarGz(t, []entry{{name: "top/../../etc/passwd", body: "x"}})
	if err := Repack(io.Discard, bytes.NewReader(escape), 1<<20); err == nil || !strings.Contains(err.Error(), "outside the tree") {
		t.Fatalf("escape: %v", err)
	}
	hardEscape := makeTarGz(t, []entry{{name: "top/x", typeflag: tar.TypeLink, link: "../../etc/passwd"}})
	if err := Repack(io.Discard, bytes.NewReader(hardEscape), 1<<20); err == nil {
		t.Fatal("a hard link out of the tree was accepted")
	}
	big := makeTarGz(t, []entry{{name: "top/a", body: strings.Repeat("a", 600)}, {name: "top/b", body: strings.Repeat("b", 600)}})
	if err := Repack(io.Discard, bytes.NewReader(big), 1000); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("size cap: %v", err)
	}
	if err := Repack(io.Discard, strings.NewReader("not gzip"), 1000); err == nil {
		t.Fatal("garbage was accepted")
	}
}

func TestSourceTarFollowsGitHubsRedirectAndRepacks(t *testing.T) {
	archive := makeTarGz(t, []entry{{name: "acme-web-abc/", typeflag: tar.TypeDir}, {name: "acme-web-abc/README.md", body: "hi"}})
	var codeloadAuth string
	codeload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		codeloadAuth = r.Header.Get("Authorization")
		w.Write(archive)
	}))
	defer codeload.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/web/tarball/abc" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("request %s auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		http.Redirect(w, r, codeload.URL+"/signed", http.StatusFound)
	}))
	defer api.Close()
	c := &Client{Base: api.URL, Tokens: StaticToken("tok")}
	var out bytes.Buffer
	if err := c.SourceTar(context.Background(), 0, "acme/web", "abc", &out); err != nil {
		t.Fatal(err)
	}
	if got := readTarGz(t, out.Bytes()); got["src/README.md"] != "hi" || len(got) != 1 {
		t.Fatalf("source = %v", got)
	}
	// 127.0.0.1 and localhost count as the same host to Go's redirect rule, so
	// this only checks the request was made; the credential rule is Go's own.
	_ = codeloadAuth
}

func TestSourceTarAsksForARefWithItsSlashesIntact(t *testing.T) {
	var got string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.EscapedPath()
		w.Write(makeTarGz(t, []entry{{name: "x/f", body: "1"}}))
	}))
	defer api.Close()
	c := &Client{Base: api.URL, Tokens: StaticToken("tok")}
	if err := c.SourceTar(context.Background(), 0, "acme/web", "refs/pull/7/merge", io.Discard); err != nil {
		t.Fatal(err)
	}
	if got != "/repos/acme/web/tarball/refs/pull/7/merge" {
		t.Fatalf("path = %q", got)
	}
	if escapeRef("a b/c") != "a%20b/c" {
		t.Fatalf("escapeRef = %q", escapeRef("a b/c"))
	}
}

func TestSourceTarNotFound(t *testing.T) {
	api := httptest.NewServer(http.NotFoundHandler())
	defer api.Close()
	c := &Client{Base: api.URL, Tokens: StaticToken("tok")}
	if err := c.SourceTar(context.Background(), 0, "acme/web", "abc", io.Discard); err != ErrNotFound {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveRefAndFileAt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/commits/main":
			fmt.Fprint(w, `{"sha":"deadbeef"}`)
		case "/repos/o/r/contents/.github/workflows/ci.yml":
			if r.URL.Query().Get("ref") != "deadbeef" {
				t.Errorf("ref = %q", r.URL.Query().Get("ref"))
			}
			fmt.Fprint(w, `{"encoding":"base64","size":9,"content":"b246IHB1c2gK"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, Tokens: StaticToken("tok")}
	sha, err := c.ResolveRef(context.Background(), 0, "o/r", "main")
	if err != nil || sha != "deadbeef" {
		t.Fatalf("sha = %q, %v", sha, err)
	}
	b, err := c.FileAt(context.Background(), 0, "o/r", ".github/workflows/ci.yml", sha)
	if err != nil || string(b) != "on: push\n" {
		t.Fatalf("file = %q, %v", b, err)
	}
	if _, err := c.ResolveRef(context.Background(), 0, "o/r", "nope"); err != ErrNotFound {
		t.Fatalf("missing ref: %v", err)
	}
}
