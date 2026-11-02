// Package imagesync keeps what GitHub's hosted runner images preinstall up to
// date in the server's database: it polls the sources the command line's
// `runner-images update` reads (the deployment badge, the release reports),
// and hands out a snapshot made of the one compiled into the binary with the
// polled data laid over it.
//
// What is tracked, and where it is read, is the catalog's; this package only
// schedules the reading and keeps the result.
package imagesync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Raccoon-CI/raccoonci/internal/server/store"
	"github.com/Raccoon-CI/raccoonci/pkg/catalog"
	"github.com/Raccoon-CI/raccoonci/pkg/runnerimages"
)

// documentName is the stored snapshot.
const documentName = "runner-images"

// Source is runnerimages.Fetcher.
type Source interface {
	Latest(ctx context.Context, image string) (*runnerimages.Defaults, error)
	LatestAlias(ctx context.Context) (string, error)
}

// Syncer polls Source into Store.
type Syncer struct {
	Store   store.Store
	Source  Source
	Catalog *catalog.Catalog // nil means catalog.Default()
	Log     *slog.Logger
}

func (s *Syncer) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (s *Syncer) cat() *catalog.Catalog {
	if s.Catalog != nil {
		return s.Catalog
	}
	return catalog.Default()
}

// stored is the JSON of the stored document: the same data as the embedded
// snapshot files, in one piece.
type stored struct {
	Aliases map[string]string                 `json:"aliases"`
	Images  map[string]*runnerimages.Defaults `json:"images"`
}

func (s *Syncer) load(ctx context.Context) (stored, error) {
	st := stored{Aliases: map[string]string{}, Images: map[string]*runnerimages.Defaults{}}
	doc, err := s.Store.GetDocument(ctx, documentName)
	if errors.Is(err, store.ErrNotFound) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(doc.Data, &st); err != nil {
		return stored{}, fmt.Errorf("the stored runner-images snapshot: %w", err)
	}
	if st.Aliases == nil {
		st.Aliases = map[string]string{}
	}
	if st.Images == nil {
		st.Images = map[string]*runnerimages.Defaults{}
	}
	return st, nil
}

// Sync reads every tracked image and the alias, and stores what it got. An
// image that cannot be read keeps the data it had (stored, or embedded); the
// returned error names those, and what could be read is stored anyway.
func (s *Syncer) Sync(ctx context.Context) error {
	cat := s.cat()
	st, err := s.load(ctx)
	if err != nil {
		// A damaged document is replaced by what is read now.
		s.log().Warn("the stored runner-images data is unusable; reading it again", "err", err)
		st = stored{Aliases: map[string]string{}, Images: map[string]*runnerimages.Defaults{}}
	}
	var failed []error
	changed := false
	for _, image := range cat.ImageNames() {
		d, err := s.Source.Latest(ctx, image)
		if err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", image, err))
			continue
		}
		if old := st.Images[image]; old == nil || old.Version != d.Version {
			s.log().Info("runner image updated", "image", image, "version", d.Version)
			changed = true
		}
		st.Images[image] = d
	}
	if latest, err := s.Source.LatestAlias(ctx); err != nil {
		failed = append(failed, fmt.Errorf("%s: %w", cat.ImageAlias, err))
	} else if st.Images[latest] == nil && !s.embeddedHas(latest) {
		failed = append(failed, fmt.Errorf("%s points to %s, which is not tracked", cat.ImageAlias, latest))
	} else if st.Aliases[cat.ImageAlias] != latest {
		st.Aliases[cat.ImageAlias] = latest
		changed = true
	}
	if changed {
		b, err := json.Marshal(st)
		if err != nil {
			return err
		}
		if err := s.Store.PutDocument(ctx, documentName, b); err != nil {
			return err
		}
	}
	return errors.Join(failed...)
}

func (s *Syncer) embeddedHas(image string) bool {
	e, err := runnerimages.Embedded()
	return err == nil && e.Images[image] != nil
}

// Snapshot is the embedded snapshot with the polled data laid over it. It
// never has less than the embedded one, and when the database cannot be read
// it is the embedded one (with the error returned alongside it).
func (s *Syncer) Snapshot(ctx context.Context) (*runnerimages.Snapshot, error) {
	snap, err := runnerimages.Embedded()
	if err != nil {
		return nil, err
	}
	st, err := s.load(ctx)
	if err != nil {
		return snap, err
	}
	for k, v := range st.Images {
		snap.Images[k] = v
	}
	for k, v := range st.Aliases {
		if snap.Images[v] != nil {
			snap.Aliases[k] = v
		}
	}
	return snap, nil
}

// Run syncs at the start and then every interval until ctx ends.
func (s *Syncer) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	sync := func() {
		c, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		if err := s.Sync(c); err != nil && ctx.Err() == nil {
			s.log().Warn("polling the runner images failed (the data kept is the last good one)", "err", err)
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
