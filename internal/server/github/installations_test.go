package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestTheAppListsItsInstallationsWithTheAppJWTAndFollowsPages(t *testing.T) {
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || r.URL.Path != "/app/installations" {
			t.Errorf("request %s auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		pages = append(pages, r.URL.Query().Get("page"))
		if r.URL.Query().Get("page") == "1" {
			var parts []string
			for i := 1; i <= 100; i++ {
				parts = append(parts, fmt.Sprintf(`{"id":%d,"account":{"login":"a%d","type":"User"},"repository_selection":"all","suspended_at":null}`, i, i))
			}
			fmt.Fprintf(w, "[%s]", strings.Join(parts, ","))
			return
		}
		fmt.Fprint(w, `[{"id":101,"account":{"login":"Org","type":"Organization"},"repository_selection":"selected","suspended_at":"2026-01-01T00:00:00Z"}]`)
	}))
	defer srv.Close()
	app := &App{ID: 1, Key: newKey(t), Base: srv.URL}
	got, err := app.Installations(context.Background())
	if err != nil || len(got) != 101 || !slices.Equal(pages, []string{"1", "2"}) {
		t.Fatalf("%d installations over pages %v, %v", len(got), pages, err)
	}
	last := got[100]
	if last.ID != 101 || last.Account != "Org" || last.AccountType != "Organization" || last.Selection != "selected" || !last.Suspended || got[0].Suspended {
		t.Fatalf("last = %+v first = %+v", last, got[0])
	}
}

func TestInstallationReposAreReadWithTheInstallationsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/installation/repositories" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("request %s auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"total_count":2,"repositories":[{"full_name":"Acme/web"},{"full_name":"Acme/api"}]}`)
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, Tokens: StaticToken("tok")}
	got, err := c.InstallationRepos(context.Background(), 5)
	if err != nil || !slices.Equal(got, []string{"Acme/web", "Acme/api"}) {
		t.Fatalf("%v, %v", got, err)
	}
}
