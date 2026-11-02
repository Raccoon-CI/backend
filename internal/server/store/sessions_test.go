package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestASessionNamesItsUserLimitsItsReposAndStopsWorkingWhenItExpires(t *testing.T) {
	s, ctx := open(t), context.Background()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	made, err := s.CreateSession(ctx, "github:alice", "alice", RoleRead, "sess1", []string{"acme/web"}, now.Add(12*time.Hour))
	if err != nil || made.User != "alice" || !made.ExpiresAt.Equal(now.Add(12*time.Hour)) || !slices.Equal(made.Repos, []string{"acme/web"}) {
		t.Fatalf("made = %+v, %v", made, err)
	}
	got, err := s.LookupToken(ctx, "sess1")
	if err != nil || got.User != "alice" || got.Role != RoleRead {
		t.Fatalf("looked up = %+v, %v", got, err)
	}
	// An API token has no user and does not expire.
	api, _ := s.CreateToken(ctx, "bot", RoleRead, "api1", nil)
	if api.User != "" || !api.ExpiresAt.IsZero() {
		t.Fatalf("an API token looks like a session: %+v", api)
	}
	now = now.Add(11*time.Hour + 59*time.Minute)
	if _, err := s.LookupToken(ctx, "sess1"); err != nil {
		t.Fatalf("a minute before it expires: %v", err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := s.LookupToken(ctx, "sess1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after it expired: %v", err)
	}
	if _, err := s.LookupToken(ctx, "api1"); err != nil {
		t.Fatalf("an API token expired: %v", err)
	}
}

func TestExpiredSessionsArePurgedAndOnlyThose(t *testing.T) {
	s, ctx := open(t), context.Background()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	s.CreateSession(ctx, "old", "a", RoleRead, "h1", nil, now.Add(time.Hour))
	s.CreateSession(ctx, "new", "b", RoleRead, "h2", nil, now.Add(48*time.Hour))
	s.CreateToken(ctx, "api", RoleAdmin, "h3", nil)
	n, err := s.PurgeExpiredTokens(ctx, now.Add(2*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("purged %d, %v", n, err)
	}
	all, _ := s.ListTokens(ctx)
	if len(all) != 2 {
		t.Fatalf("left: %+v", all)
	}
}
