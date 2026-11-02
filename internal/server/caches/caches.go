// Package caches serves the cache protocol of actions/cache (v1) to the
// machines that run a server's jobs, with a cache per space (see Space)
// instead of one for everything: a run restores from its own ref's space and
// the default branch's, and saves into its own ref's, never into another's.
//
// Under it sit ordinary cache.Service instances, one per space, made when
// first needed: each has its own index (a file on the server's disk) and its
// own objects, in a bucket under a prefix or in a directory. A request is
// routed by its token: the token names a run, the run names its spaces. v2 is
// not advertised, so jobs use v1.
//
// Sweep (run by Janitor) deletes entries nobody has hit for seven days, as
// GitHub does, removes spaces left empty, and keeps a repository's spaces
// together under a size cap. Each space also has a cap of its own, enforced at
// every upload.
package caches

import (
	"bytes"
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
	"syscall"
	"time"

	"raccoonci/internal/artifact"
	"raccoonci/internal/blob"
	"raccoonci/internal/cache"
)

const (
	apiPrefix = "/_apis/artifactcache/"
	dlPrefix  = "/_cache_dl/"
)

// Config says where caches are kept and who may use which.
type Config struct {
	// Dir holds the index of every space, the spool of chunks, and, without
	// S3, the objects.
	Dir string
	// PublicURL is the https address machines reach the server at.
	PublicURL string
	// Signer is the one that signs the jobs' tokens (artifacts.Service.Signer).
	Signer *artifact.Signer
	// S3For says which bucket an organization's objects go to, each space
	// under its own prefix after the bucket's own; nil, or a nil bucket, keeps
	// them in Dir. version changes when the bucket does (storage.Service.For).
	S3For func(ctx context.Context, org string) (bucket *blob.S3Config, version string, err error)
	// MaxBytes caps the entries of one space (0: no cap); the least recently
	// used go first, as on GitHub.
	MaxBytes int64
	// MaxAge is how long an entry may go unused before the sweep deletes it
	// (default DefaultMaxAge, seven days). MaxRepoBytes caps what all the spaces
	// of one repository hold together (0: no cap); it is enforced by the sweep,
	// so a repository can exceed it for the time between two sweeps.
	MaxAge       time.Duration
	MaxRepoBytes int64
	Log          *slog.Logger
	// Spaces says which spaces a run uses; an error refuses the run any cache.
	Spaces func(ctx context.Context, runID int64) (Space, error)
}

// Service is the cache service for all runs.
type Service struct {
	cfg Config
	key []byte // signs the links to objects kept in Dir

	mu     sync.Mutex
	spaces map[string]*space
}

type space struct {
	svc     *cache.Service
	h       http.Handler
	dl      http.Handler // nil when the objects are in a bucket
	version string       // of the bucket it was made on
	close   func()
}

// New starts the service.
func New(c Config) (*Service, error) {
	if c.Dir == "" || c.PublicURL == "" || c.Signer == nil || c.Spaces == nil {
		return nil, errors.New("caches: Dir, PublicURL, Signer and Spaces are required")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(c.Dir, "cache"), 0o700); err != nil {
		return nil, err
	}
	return &Service{cfg: c, key: key, spaces: map[string]*space{}}, nil
}

// Mount puts the routes on mux: the protocol, and the links to objects kept
// in Dir.
func (s *Service) Mount(mux *http.ServeMux) {
	mux.Handle(apiPrefix, http.HandlerFunc(s.serve))
	mux.Handle(dlPrefix, http.HandlerFunc(s.download))
}

// Env is what a job's steps need besides their token (the artifact service's
// ACTIONS_RUNTIME_TOKEN is the same one).
func (s *Service) Env() map[string]string {
	return map[string]string{
		"ACTIONS_CACHE_URL": strings.TrimRight(s.cfg.PublicURL, "/") + "/",
		// Cleared, so that a value the host has cannot make the client use v2.
		"ACTIONS_CACHE_SERVICE_V2": "",
	}
}

// JobEnver is what execute.Services is.
type JobEnver interface {
	JobEnv(run int64, job string) (map[string]string, error)
}

// Extend adds the cache's environment to what inner gives a job.
func (s *Service) Extend(inner JobEnver) JobEnver { return extended{inner, s.Env()} }

type extended struct {
	inner JobEnver
	env   map[string]string
}

func (e extended) JobEnv(run int64, job string) (map[string]string, error) {
	env, err := e.inner.JobEnv(run, job)
	if err != nil {
		return nil, err
	}
	for k, v := range e.env {
		env[k] = v
	}
	return env, nil
}

// Close stops every space.
func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sp := range s.spaces {
		sp.close()
		delete(s.spaces, id)
	}
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"message":%q}`, msg)
}

// serve routes one request of the protocol by the run its token names.
func (s *Service) serve(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		jsonError(w, http.StatusUnauthorized, "missing bearer token")
		return
	}
	sc, err := s.cfg.Signer.Verify(tok)
	if err != nil {
		jsonError(w, http.StatusUnauthorized, err.Error())
		return
	}
	runID, err := strconv.ParseInt(sc.Run, 10, 64)
	if err != nil {
		jsonError(w, http.StatusForbidden, "the token names no run")
		return
	}
	sp, err := s.cfg.Spaces(r.Context(), runID)
	if err != nil || !validSpace(sp.Own) {
		jsonError(w, http.StatusForbidden, "this run has no cache")
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == apiPrefix+"cache" {
		s.restore(w, r, sp)
		return
	}
	own, err := s.space(r.Context(), sp.Own, sp.Org, true)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "the cache is not available")
		return
	}
	own.h.ServeHTTP(w, r)
}

// capture keeps an answer so that it can be thrown away.
type capture struct {
	h    http.Header
	code int
	body bytes.Buffer
}

func (c *capture) Header() http.Header         { return c.h }
func (c *capture) WriteHeader(code int)        { c.code = code }
func (c *capture) Write(b []byte) (int, error) { return c.body.Write(b) }

// restore looks a key up in the spaces the run may read, in order, and answers
// with the first hit; 204 when none has it, as the protocol says.
func (s *Service) restore(w http.ResponseWriter, r *http.Request, sp Space) {
	for _, id := range sp.Read {
		if !validSpace(id) {
			continue
		}
		// A space nobody has written to is not made for a lookup.
		found, err := s.space(r.Context(), id, sp.Org, id == sp.Own)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "the cache is not available")
			return
		}
		if found == nil {
			continue
		}
		c := &capture{h: http.Header{}, code: http.StatusOK}
		found.h.ServeHTTP(c, r)
		if c.code == http.StatusNoContent {
			continue
		}
		for k, v := range c.h {
			w.Header()[k] = v
		}
		w.WriteHeader(c.code)
		w.Write(c.body.Bytes())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// space returns the service of a space, made if create is set or its index is
// already on disk (nil, nil when it has never been written). The objects go
// to the bucket the organization has (S3For), else to Dir; when that changes
// the space is made again on the new one: its index then names objects that
// are not there, and an entry whose object has vanished is dropped when it is
// looked up.
func (s *Service) space(ctx context.Context, id, org string, create bool) (*space, error) {
	var bucket *blob.S3Config
	version := "disk"
	if s.cfg.S3For != nil && org != "" {
		var err error
		if bucket, version, err = s.cfg.S3For(ctx, org); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sp, ok := s.spaces[id]; ok {
		if sp.version == version {
			return sp, nil
		}
		sp.close()
		delete(s.spaces, id)
	}
	dir := filepath.Join(s.cfg.Dir, "cache", filepath.FromSlash(id))
	if !create {
		if _, err := os.Stat(filepath.Join(dir, "index.json")); err != nil {
			return nil, nil
		}
	}
	sp, err := s.open(id, dir, bucket)
	if err != nil {
		return nil, err
	}
	s.writeOrg(dir, org)
	sp.version = version
	s.spaces[id] = sp
	return sp, nil
}

// peek is the space as it is now, if it is made; the links to objects in Dir
// need no organization.
func (s *Service) peek(id string) *space {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spaces[id]
}

func (s *Service) open(id, dir string, bucket *blob.S3Config) (*space, error) {
	var store blob.Store
	var free func() (int64, error)
	sp := &space{}
	closers := []func(){}
	if bucket != nil {
		c := *bucket
		c.Prefix += "cache/" + id + "/"
		b, err := blob.NewS3(c)
		if err != nil {
			return nil, err
		}
		store = b
	} else {
		d, err := blob.NewDir(filepath.Join(dir, "objects"), s.cfg.PublicURL, s.key)
		if err != nil {
			return nil, err
		}
		d.Prefix = dlPrefix + id + "/"
		store, sp.dl = d, d.Handler()
		closers = append(closers, func() { d.Close() })
		free = freeBytes(dir)
	}
	svc, err := cache.New(cache.Config{
		Store: store, Signer: s.cfg.Signer, BaseURL: s.cfg.PublicURL,
		IndexPath: filepath.Join(dir, "index.json"), SpoolDir: filepath.Join(s.cfg.Dir, "cache-spool"),
		MaxBytes: s.cfg.MaxBytes, Free: free,
	})
	if err != nil {
		for _, c := range closers {
			c()
		}
		return nil, err
	}
	mux := http.NewServeMux()
	svc.Routes(mux)
	sp.svc, sp.h = svc, mux
	sp.close = func() {
		svc.Close()
		for _, c := range closers {
			c()
		}
	}
	return sp, nil
}

// download serves the links to objects kept in Dir: /_cache_dl/<space>/<key>.
func (s *Service) download(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, dlPrefix)
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 3 || !validSpace(parts[0]+"/"+parts[1]) {
		http.NotFound(w, r)
		return
	}
	sp := s.peek(parts[0] + "/" + parts[1])
	if sp == nil || sp.dl == nil {
		http.NotFound(w, r)
		return
	}
	sp.dl.ServeHTTP(w, r)
}

// freeBytes reports the space available on the disk holding dir.
func freeBytes(dir string) func() (int64, error) {
	return func() (int64, error) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(dir, &st); err != nil {
			return 0, err
		}
		return int64(st.Bavail) * int64(st.Bsize), nil
	}
}
