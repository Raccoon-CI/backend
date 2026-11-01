// Package artifacts serves the artifact protocol (upload-artifact and
// download-artifact v4+) to the machines that run a server's jobs. One service
// for all runs: a token is signed for one run and one job, and the service
// opens only that run to it, so runs do not see each other's artifacts.
//
// Where the data lives depends on the organization the run belongs to: in the
// bucket the organization has given (S3For), else on the server's disk. Each
// bucket gets an artifact service of its own (made when first needed), and a
// request is routed to it by the run its token or its path names. The metadata
// is in memory, so a restart forgets what was uploaded (TODO C.1: persistence,
// retention); the data stays in the store.
package artifacts

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"raccoonci/internal/artifact"
	"raccoonci/internal/blob"
)

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
	if s.disk, err = s.newInstance(dir, dir); err != nil {
		dir.Close()
		return nil, err
	}
	return s, nil
}

func (s *Service) newInstance(store blob.Store, dir *blob.Dir) (*instance, error) {
	art, err := artifact.New(artifact.Config{Store: store, Signer: s.signer, BaseURL: s.cfg.PublicURL,
		SpoolDir: filepath.Join(s.cfg.Dir, "spool")})
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
	store, err := s.cfg.NewStore(cfg)
	if err != nil {
		return nil, err
	}
	in, err := s.newInstance(store, nil)
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
