package caches

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/cache"
)

// DefaultMaxAge is how long an entry may go unused before it is deleted: seven
// days, as on GitHub.
const DefaultMaxAge = 7 * 24 * time.Hour

// orgFile, next to a space's index, says whose bucket the space's objects are
// in; the sweep needs it to reach them, and the directory name is only a hash.
const orgFile = "org"

func (s *Service) maxAge() time.Duration {
	if s.cfg.MaxAge > 0 {
		return s.cfg.MaxAge
	}
	return DefaultMaxAge
}

func (s *Service) log() *slog.Logger {
	if s.cfg.Log != nil {
		return s.cfg.Log
	}
	return slog.New(slog.DiscardHandler)
}

// SweepResult says what a sweep did.
type SweepResult struct {
	Expired int   // entries deleted because nobody used them for MaxAge
	Evicted int   // entries deleted to bring a repository under MaxRepoBytes
	Freed   int64 // bytes of both
	Removed int   // empty spaces removed
}

// spaceIDs lists the spaces that have an index on disk.
func (s *Service) spaceIDs() []string {
	var ids []string
	repos, _ := os.ReadDir(filepath.Join(s.cfg.Dir, "cache"))
	for _, r := range repos {
		spaces, _ := os.ReadDir(filepath.Join(s.cfg.Dir, "cache", r.Name()))
		for _, sp := range spaces {
			id := r.Name() + "/" + sp.Name()
			if !validSpace(id) {
				continue
			}
			if _, err := os.Stat(filepath.Join(s.cfg.Dir, "cache", r.Name(), sp.Name(), "index.json")); err == nil {
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	return ids
}

func (s *Service) spaceDir(id string) string {
	return filepath.Join(s.cfg.Dir, "cache", filepath.FromSlash(id))
}

// orgOf says whose bucket a space is in. ok is false when that is not known: a
// space made before the organization was written down, and not on this disk.
func (s *Service) orgOf(id string) (org string, ok bool) {
	dir := s.spaceDir(id)
	if b, err := os.ReadFile(filepath.Join(dir, orgFile)); err == nil {
		return strings.TrimSpace(string(b)), true
	}
	if _, err := os.Stat(filepath.Join(dir, "objects")); err == nil {
		return "", true // its objects are on this disk
	}
	return "", false
}

// entriesOf lists a space's entries: the live ones if it is open, else the
// index file's (the live last-use times are newer than the file's).
func (s *Service) entriesOf(id string) ([]cache.EntryInfo, error) {
	if sp := s.peek(id); sp != nil {
		return sp.svc.Entries(), nil
	}
	return cache.ReadIndex(filepath.Join(s.spaceDir(id), "index.json"))
}

// Sweep deletes what has gone unused for MaxAge, removes spaces left empty,
// and then brings each repository under MaxRepoBytes by deleting its least
// recently used entries, whichever space they are in. Nothing that is being
// uploaded is touched. The repository cap is therefore checked at sweeps, not at
// every upload (each space still has its own cap at the upload).
func (s *Service) Sweep(ctx context.Context, now time.Time) (SweepResult, error) {
	var res SweepResult
	var firstErr error
	note := func(err error) { firstErr = errors.Join(firstErr, err) }
	cutoff := now.Add(-s.maxAge())

	for _, id := range s.spaceIDs() {
		entries, err := s.entriesOf(id)
		if err != nil {
			note(fmt.Errorf("space %s: %w", id, err))
			continue
		}
		stale := false
		for _, e := range entries {
			stale = stale || e.Used.Before(cutoff)
		}
		empty := len(entries) == 0
		if !stale && !empty {
			continue
		}
		org, ok := s.orgOf(id)
		if !ok {
			continue
		}
		sp, err := s.space(ctx, id, org, false)
		if err != nil {
			note(fmt.Errorf("space %s: %w", id, err))
			continue
		}
		if sp == nil {
			continue
		}
		n, freed, err := sp.svc.Expire(ctx, cutoff)
		res.Expired += n
		res.Freed += freed
		if err != nil {
			note(fmt.Errorf("space %s: %w", id, err))
			continue
		}
		// A space with nothing in it goes, unless it was emptied a moment ago by
		// someone else's eviction and is about to be filled: it must be idle and
		// its index must not be new.
		if len(sp.svc.Entries()) == 0 && sp.svc.Idle() && (n > 0 || s.indexOlderThan(id, cutoff)) {
			if s.removeSpace(id) {
				res.Removed++
			}
		}
	}

	if s.cfg.MaxRepoBytes > 0 {
		if err := s.capRepos(ctx, &res); err != nil {
			note(err)
		}
	}
	return res, firstErr
}

func (s *Service) indexOlderThan(id string, t time.Time) bool {
	fi, err := os.Stat(filepath.Join(s.spaceDir(id), "index.json"))
	return err == nil && fi.ModTime().Before(t)
}

// removeSpace closes a space that has nothing in it and deletes its directory.
func (s *Service) removeSpace(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sp, ok := s.spaces[id]; ok {
		if !sp.svc.Idle() || len(sp.svc.Entries()) != 0 {
			return false
		}
		sp.close()
		delete(s.spaces, id)
	}
	dir := s.spaceDir(id)
	if err := os.RemoveAll(dir); err != nil {
		return false
	}
	os.Remove(filepath.Dir(dir)) // the repository's directory, if that was its last space
	return true
}

type heldEntry struct {
	space string
	cache.EntryInfo
}

// capRepos deletes the least recently used entries of every repository that
// holds more than MaxRepoBytes.
func (s *Service) capRepos(ctx context.Context, res *SweepResult) error {
	byRepo := map[string][]heldEntry{}
	for _, id := range s.spaceIDs() {
		entries, err := s.entriesOf(id)
		if err != nil {
			continue // reported by the first pass
		}
		repo, _, _ := strings.Cut(id, "/")
		for _, e := range entries {
			byRepo[repo] = append(byRepo[repo], heldEntry{id, e})
		}
	}
	var firstErr error
	for _, held := range byRepo {
		var total int64
		for _, e := range held {
			total += e.Size
		}
		if total <= s.cfg.MaxRepoBytes {
			continue
		}
		sort.Slice(held, func(i, j int) bool {
			if !held[i].Used.Equal(held[j].Used) {
				return held[i].Used.Before(held[j].Used)
			}
			return held[i].ID < held[j].ID
		})
		for _, e := range held {
			if total <= s.cfg.MaxRepoBytes {
				break
			}
			org, ok := s.orgOf(e.space)
			if !ok {
				continue
			}
			sp, err := s.space(ctx, e.space, org, false)
			if err != nil || sp == nil {
				firstErr = errors.Join(firstErr, fmt.Errorf("space %s: %v", e.space, err))
				continue
			}
			if err := sp.svc.Evict(ctx, e.ID); err != nil {
				firstErr = errors.Join(firstErr, fmt.Errorf("space %s: %w", e.space, err))
				continue
			}
			total -= e.Size
			res.Evicted++
			res.Freed += e.Size
		}
	}
	return firstErr
}

// Janitor sweeps every interval (and once at the start) until ctx ends.
func (s *Service) Janitor(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	sweep := func() {
		res, err := s.Sweep(ctx, time.Now())
		if err != nil && ctx.Err() == nil {
			s.log().Warn("sweeping the caches failed", "err", err)
		}
		if res.Expired+res.Evicted+res.Removed > 0 {
			s.log().Info("caches swept", "expired", res.Expired, "evicted_for_the_repository_cap", res.Evicted,
				"spaces_removed", res.Removed, "freed_bytes", res.Freed)
		}
	}
	sweep()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}

// writeOrg records whose bucket a space is in, once.
func (s *Service) writeOrg(dir, org string) {
	if org == "" {
		return
	}
	p := filepath.Join(dir, orgFile)
	if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) == org {
		return
	}
	os.MkdirAll(dir, 0o700)
	os.WriteFile(p, []byte(org), 0o600)
}
