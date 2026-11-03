// Package catalogsync keeps the catalog feed (the signed text of
// pkg/catalog.Source) up to date in the server's database: it pulls the feed
// at the start and then at an interval, stores the verified text, and hands out
// the built-in catalog with that text laid over it.
//
// What the catalog says and how a feed is verified is pkg/catalog's; this
// package only schedules the pulling and keeps the result.
package catalogsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Raccoon-CI/backend/internal/server/store"
	"github.com/Raccoon-CI/lib/pkg/catalog"
)

// documentName is the stored feed text.
const documentName = "catalog-feed"

// Syncer pulls Source into Store.
type Syncer struct {
	Store  store.Store
	Source *catalog.Source // nil: nothing is pulled; what is stored is still served
	Log    *slog.Logger
}

func (s *Syncer) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Sync pulls the feed now and stores its verified text when it differs from the
// stored one. A feed that cannot be pulled or does not verify leaves what was
// stored alone, and its reason is returned.
func (s *Syncer) Sync(ctx context.Context) error {
	if s.Source == nil {
		return nil
	}
	_, st := s.Source.Load(ctx, catalog.Default(), true)
	if st.Body == nil {
		if st.Err == nil {
			st.Err = errors.New("no feed text")
		}
		return st.Err
	}
	old, err := s.Store.GetDocument(ctx, documentName)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err == nil && bytes.Equal(old.Data, st.Body) {
		return st.Err
	}
	s.log().Info("catalog feed updated", "origin", string(st.Origin), "bytes", len(st.Body))
	if err := s.Store.PutDocument(ctx, documentName, st.Body); err != nil {
		return err
	}
	return st.Err
}

// Current is the catalog to plan with and the feed text it was made from (nil
// when it is the built-in one). It never has less than the built-in catalog:
// when nothing is stored, or the database cannot be read, or the stored text no
// longer applies, it is the built-in one (with the reason returned alongside).
func (s *Syncer) Current(ctx context.Context) (*catalog.Catalog, []byte, error) {
	base := catalog.Default()
	doc, err := s.Store.GetDocument(ctx, documentName)
	if errors.Is(err, store.ErrNotFound) {
		return base, nil, nil
	}
	if err != nil {
		return base, nil, err
	}
	cat, err := base.Overlay(doc.Data)
	if err != nil {
		return base, nil, fmt.Errorf("the stored catalog feed: %w", err)
	}
	return cat, doc.Data, nil
}

// Run syncs at the start and then every interval until ctx ends.
func (s *Syncer) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	sync := func() {
		c, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if err := s.Sync(c); err != nil && ctx.Err() == nil {
			s.log().Warn("pulling the catalog feed failed (the catalog kept is the last good one)", "err", err)
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
