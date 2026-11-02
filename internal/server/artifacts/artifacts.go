// Package artifacts serves the artifact protocol (upload-artifact and
// download-artifact v4+) to the machines that run a server's jobs. One service
// for all runs: a token is signed for one run and one job, and the service
// opens only that run to it, so runs do not see each other's artifacts.
//
// Where the data lives depends on the organization the run belongs to: in the
// bucket the organization has given (S3For), else on the server's disk. Each
// bucket gets an artifact service of its own (made when first needed), and a
// request is routed to it by the run its token or its path names. Finished
// artifacts are remembered in the database (Config.Records), each service under
// a scope of its own, so a restart keeps them and they can be listed and
// downloaded through the API; an artifact is deleted when its time to keep
// (90 days unless the client asks for less, capped by Config.MaxKeep) is up.
// Uploads in progress are not remembered.
package artifacts

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"raccoonci/internal/artifact"
	"raccoonci/internal/blob"
	"raccoonci/internal/server/store"
)

// Records is what the service needs of the database (store.Store).
type Records interface {
	PutArtifact(ctx context.Context, a store.Artifact) error
	ListArtifacts(ctx context.Context, scope string) ([]store.Artifact, error)
	DeleteArtifact(ctx context.Context, scope string, id int64) error
	ExpiredArtifacts(ctx context.Context, now time.Time) ([]store.Artifact, error)
}

// scopeDisk is the scope of the service on this server's disk.
const scopeDisk = "disk"

// recordIndex is an artifact.Index over the database, for one scope.
type recordIndex struct {
	recs  Records
	scope string
}

func (x recordIndex) Load(ctx context.Context) ([]artifact.Record, error) {
	list, err := x.recs.ListArtifacts(ctx, x.scope)
	if err != nil {
		return nil, err
	}
	out := make([]artifact.Record, 0, len(list))
	for _, a := range list {
		out = append(out, artifact.Record{ID: a.ID, Run: a.Run, Job: a.Job, Name: a.Name, Key: a.Key, Mime: a.Mime,
			Size: a.Size, Digest: a.Digest, Created: a.CreatedAt, Expires: a.ExpiresAt})
	}
	return out, nil
}

func (x recordIndex) Save(ctx context.Context, r artifact.Record) error {
	return x.recs.PutArtifact(ctx, store.Artifact{Scope: x.scope, ID: r.ID, Run: r.Run, Job: r.Job, Name: r.Name, Key: r.Key,
		Mime: r.Mime, Size: r.Size, Digest: r.Digest, CreatedAt: r.Created, ExpiresAt: r.Expires})
}

func (x recordIndex) Delete(ctx context.Context, id int64) error {
	return x.recs.DeleteArtifact(ctx, x.scope, id)
}

// tokenTTL is how long a job's token works: longer than a job may run.
const tokenTTL = 7 * time.Hour

// Config says where the service keeps data and where machines reach it.
type Config struct {
	// Dir holds the data kept on this disk and the spool of blocks.
	Dir string
	// PublicURL is the https address machines reach the service at, without
	// a path: every link and environment value is built on it.
	PublicURL string
	// S3For says which bucket an organization's artifacts go to (under the
	// prefix "artifacts/" after the bucket's own); nil, or a nil bucket, keeps
	// them on Dir. version changes when the bucket does (storage.Service.For).
	S3For func(ctx context.Context, org string) (bucket *blob.S3Config, version string, err error)
	// RunOrg says the organization a run belongs to; needed with S3For.
	RunOrg func(ctx context.Context, runID int64) (string, error)
	// NewStore makes the store of a bucket; default blob.NewS3 (tests replace it).
	NewStore func(blob.S3Config) (blob.Store, error)
	// Records, if set, remembers finished artifacts (see the package comment).
	Records Records
	// Keep is how long an artifact is kept when the client does not say
	// (default 90 days); MaxKeep, if set, caps what a client may ask for.
	Keep    time.Duration
	MaxKeep time.Duration
	// Free, with MinFree, refuses new artifacts while the disk of Dir has less
	// than MinFree bytes available (the spool of every service lives there).
	Free    func() (int64, error)
	MinFree int64
	Log     *slog.Logger
}

// Service is the artifact service.
type Service struct {
	cfg    Config
	signer *artifact.Signer
	disk   *instance // the one on this disk

	mu    sync.Mutex
	other map[string]*instance // by organization and bucket version
}

// instance is one artifact service on one store.
type instance struct {
	art *artifact.Service
	dir *blob.Dir // nil when the data is in a bucket
	h   http.Handler
}

// New starts the service. Its signing key is new each time, so links and
// tokens from before a restart stop working (the runs they belong to do too).
func New(c Config) (*Service, error) {
	if c.Dir == "" || c.PublicURL == "" {
		return nil, errors.New("artifacts: Dir and PublicURL are required")
	}
	if c.NewStore == nil {
		c.NewStore = func(cfg blob.S3Config) (blob.Store, error) { return blob.NewS3(cfg) }
	}
	// Blocks of uploads that a stopped server never finished; no upload can be
	// in progress yet.
	os.RemoveAll(filepath.Join(c.Dir, "spool"))
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	signer, err := artifact.NewSigner(key)
	if err != nil {
		return nil, err
	}
	dir, err := blob.NewDir(filepath.Join(c.Dir, "objects"), c.PublicURL, key)
	if err != nil {
		return nil, err
	}
	s := &Service{cfg: c, signer: signer, other: map[string]*instance{}}
	if s.disk, err = s.newInstance(dir, dir, scopeDisk); err != nil {
		dir.Close()
		return nil, err
	}
	return s, nil
}

func (s *Service) newInstance(st blob.Store, dir *blob.Dir, scope string) (*instance, error) {
	cfg := artifact.Config{Store: st, Signer: s.signer, BaseURL: s.cfg.PublicURL,
		SpoolDir: filepath.Join(s.cfg.Dir, "spool"), Keep: s.cfg.Keep, MaxKeep: s.cfg.MaxKeep,
		Free: s.cfg.Free, MinFree: s.cfg.MinFree}
	if s.cfg.Records != nil {
		cfg.Index = recordIndex{recs: s.cfg.Records, scope: scope}
	}
	art, err := artifact.New(cfg)
	if err != nil {
		return nil, err
	}
	return &instance{art: art, dir: dir, h: art.Handler()}, nil
}

// Signer is what signs the tokens and links of this service; the cache service
// uses the same one, so one token serves both, as on GitHub.
func (s *Service) Signer() *artifact.Signer { return s.signer }

// Mount puts the service's routes on mux: the Twirp API, the blob upload and
// the download links of the data on this disk. Every one is guarded by a
// token or a signed link.
func (s *Service) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/twirp/", s.twirp)
	mux.HandleFunc("/_blob/", s.blob)
	mux.Handle(blob.DownloadPrefix, s.disk.dir.Handler())
}

func twirpError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"code":%q,"msg":%q}`, code, msg)
}

// instanceFor finds the service a run's artifacts are in.
func (s *Service) instanceFor(ctx context.Context, runID int64) (*instance, error) {
	if s.cfg.S3For == nil || s.cfg.RunOrg == nil {
		return s.disk, nil
	}
	org, err := s.cfg.RunOrg(ctx, runID)
	if err != nil {
		return nil, err
	}
	bucket, version, err := s.cfg.S3For(ctx, org)
	if err != nil {
		return nil, err
	}
	if bucket == nil {
		return s.disk, nil
	}
	key := org + "@" + version
	s.mu.Lock()
	defer s.mu.Unlock()
	if in, ok := s.other[key]; ok {
		return in, nil
	}
	cfg := *bucket
	cfg.Prefix += "artifacts/"
	st, err := s.cfg.NewStore(cfg)
	if err != nil {
		return nil, err
	}
	in, err := s.newInstance(st, nil, key)
	if err != nil {
		return nil, err
	}
	s.other[key] = in
	return in, nil
}

// twirp routes a call of the Twirp API by the run its token is for. A token
// that does not verify goes to the service on this disk, which refuses it as it
// refuses any.
func (s *Service) twirp(w http.ResponseWriter, r *http.Request) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	sc, err := s.signer.Verify(tok)
	if err != nil {
		s.disk.h.ServeHTTP(w, r)
		return
	}
	s.serveRun(w, r, sc.Run)
}

// blob routes an upload by the run in its path, /_blob/<run>/<id>.
func (s *Service) blob(w http.ResponseWriter, r *http.Request) {
	run, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/_blob/"), "/")
	s.serveRun(w, r, run)
}

func (s *Service) serveRun(w http.ResponseWriter, r *http.Request, run string) {
	id, err := strconv.ParseInt(run, 10, 64)
	if err != nil {
		twirpError(w, http.StatusForbidden, "permission_denied", "the request names no run")
		return
	}
	in, err := s.instanceFor(r.Context(), id)
	if err != nil {
		twirpError(w, http.StatusInternalServerError, "internal", "the artifact store is not available for this run")
		return
	}
	in.h.ServeHTTP(w, r)
}

// JobEnv is what a step needs to reach the service as one job of one run:
// ACTIONS_RESULTS_URL and a token that opens only that run and creates
// artifacts only as that job.
func (s *Service) JobEnv(run int64, job string) (map[string]string, error) {
	return s.disk.art.Env(artifact.Scope{Run: strconv.FormatInt(run, 10), Job: job}, tokenTTL)
}

func (s *Service) log() *slog.Logger {
	if s.cfg.Log != nil {
		return s.cfg.Log
	}
	return slog.New(slog.DiscardHandler)
}

// ErrNotFound is returned for an artifact that is not there.
var ErrNotFound = artifact.ErrNoArtifact

// List returns the finished artifacts of a run.
func (s *Service) List(ctx context.Context, runID int64) ([]artifact.Info, error) {
	in, err := s.instanceFor(ctx, runID)
	if err != nil {
		return nil, err
	}
	return in.art.Artifacts(strconv.FormatInt(runID, 10)), nil
}

// DownloadURL presigns a link to one finished artifact of a run, for an hour.
func (s *Service) DownloadURL(ctx context.Context, runID, artifactID int64) (string, error) {
	in, err := s.instanceFor(ctx, runID)
	if err != nil {
		return "", err
	}
	return in.art.DownloadURL(ctx, strconv.FormatInt(runID, 10), artifactID, time.Hour)
}

// Sweep deletes the artifacts whose time is up, data and record, and returns
// how many. A record that no service holds any more (its bucket was replaced)
// is only forgotten; one whose service could not be reached is left for the
// next sweep.
func (s *Service) Sweep(ctx context.Context, now time.Time) (int, error) {
	if s.cfg.Records == nil {
		return 0, nil
	}
	due, err := s.cfg.Records.ExpiredArtifacts(ctx, now)
	if err != nil {
		return 0, err
	}
	n := 0
	unreachable := map[string]bool{}
	seen := map[string]bool{}
	var firstErr error
	for _, a := range due {
		if seen[a.Run] {
			continue
		}
		seen[a.Run] = true
		id, perr := strconv.ParseInt(a.Run, 10, 64)
		if perr != nil {
			continue
		}
		in, ierr := s.instanceFor(ctx, id)
		if ierr != nil {
			unreachable[a.Run] = true
			firstErr = errors.Join(firstErr, fmt.Errorf("run %s: %w", a.Run, ierr))
			continue
		}
		c, eerr := in.art.Expire(ctx, now)
		n += c
		if eerr != nil {
			unreachable[a.Run] = true
			firstErr = errors.Join(firstErr, fmt.Errorf("run %s: %w", a.Run, eerr))
		}
	}
	left, err := s.cfg.Records.ExpiredArtifacts(ctx, now)
	if err != nil {
		return n, errors.Join(firstErr, err)
	}
	for _, a := range left {
		if unreachable[a.Run] {
			continue
		}
		if err := s.cfg.Records.DeleteArtifact(ctx, a.Scope, a.ID); err != nil {
			firstErr = errors.Join(firstErr, err)
		}
	}
	return n, firstErr
}

// Janitor sweeps every interval (and once at the start) until ctx ends.
func (s *Service) Janitor(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	sweep := func() {
		n, err := s.Sweep(ctx, time.Now())
		if err != nil && ctx.Err() == nil {
			s.log().Warn("deleting expired artifacts failed", "deleted", n, "err", err)
		} else if n > 0 {
			s.log().Info("expired artifacts deleted", "artifacts", n)
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

// Close stops the service's background work.
func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, in := range s.other {
		in.art.Close()
		delete(s.other, k)
	}
	s.disk.art.Close()
	s.disk.dir.Close()
}
