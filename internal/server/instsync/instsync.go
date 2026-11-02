// Package instsync keeps the table of the App's installations in step with
// GitHub: the installation events keep it current, but an installation made
// before the server could receive events (or while it was down) is only known
// by asking. It asks at the start and then every interval.
package instsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/server/github"
	"github.com/Raccoon-CI/raccoonci/internal/server/store"
)

// GitHub is what is asked of GitHub (github.App and github.Client).
type GitHub interface {
	Installations(ctx context.Context) ([]github.AppInstallation, error)
	InstallationRepos(ctx context.Context, installationID int64) ([]string, error)
}

// Syncer reads the installations into the store.
type Syncer struct {
	Store store.Store
	GH    GitHub
	Log   *slog.Logger
}

func (s *Syncer) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Sync makes the store agree with GitHub: installations are added or updated
// with their repositories, and those GitHub no longer lists are removed. An
// installation whose repositories cannot be read keeps what it had; the error
// says which. It never removes anything when GitHub could not be asked.
func (s *Syncer) Sync(ctx context.Context) error {
	listed, err := s.GH.Installations(ctx)
	if err != nil {
		return err
	}
	have := map[int64]bool{}
	var failed []error
	for _, in := range listed {
		have[in.ID] = true
		rec := store.Installation{ID: in.ID, Account: in.Account, AccountType: in.AccountType, Selection: in.Selection, Suspended: in.Suspended}
		repos, rerr := s.GH.InstallationRepos(ctx, in.ID)
		if rerr != nil {
			// Keep the repositories we have, still update the rest. A suspended
			// installation cannot make a token, which is no news.
			if !in.Suspended {
				failed = append(failed, fmt.Errorf("installation %d (%s): %w", in.ID, in.Account, rerr))
			}
			if err := s.Store.PutInstallation(ctx, rec, nil, false); err != nil {
				return err
			}
			continue
		}
		if err := s.Store.PutInstallation(ctx, rec, repos, true); err != nil {
			return err
		}
	}
	known, err := s.Store.ListInstallations(ctx)
	if err != nil {
		return err
	}
	for _, in := range known {
		if !have[in.ID] {
			if err := s.Store.DeleteInstallation(ctx, in.ID); err != nil {
				return err
			}
			s.log().Info("an installation GitHub no longer lists was forgotten", "installation", in.ID, "account", strings.ToLower(in.Account))
		}
	}
	return errors.Join(failed...)
}

// Run syncs now and then every interval (default an hour) until ctx ends.
func (s *Syncer) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	sync := func() {
		c, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if err := s.Sync(c); err != nil && ctx.Err() == nil {
			s.log().Warn("reading the App's installations failed", "err", err)
		}
	}
	sync()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sync()
		}
	}
}
