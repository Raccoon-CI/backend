// Package artifacts serves the artifact protocol (upload-artifact and
// download-artifact v4) to the machines that run a server's jobs. One service
// for all runs: a token is signed for one run and one job, and the service
// opens only that run to it, so runs do not see each other's artifacts.
//
// Data lives in a local directory for now (blob.Dir); the metadata is in
// memory, so a restart forgets what was uploaded (TODO C.1: persistence, an
// S3-compatible store of the user's choice, retention). The cache protocols
// are not served here yet: a cache must be kept apart per repository and per
// branch, which the shared CLI service does not do, so jobs run without one,
// as actions/cache does when the service is missing.
package artifacts

import (
	"crypto/rand"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"raccoonci/internal/artifact"
	"raccoonci/internal/blob"
)

// tokenTTL is how long a job's token works: longer than a job may run.
const tokenTTL = 7 * time.Hour

// Config says where the service keeps data and where machines reach it.
type Config struct {
	// Dir holds the uploaded data and the spool of blocks.
	Dir string
	// PublicURL is the https address machines reach the service at, without
	// a path: every link and environment value is built on it.
	PublicURL string
	// S3, if set, keeps the data in that bucket under the prefix
	// "artifacts/" (after its own Prefix); downloads are presigned links to
	// the bucket. Nil keeps it in Dir.
	S3 *blob.S3Config
}

// Service is the artifact service.
type Service struct {
	art    *artifact.Service
	dir    *blob.Dir // nil when the data is in a bucket
	signer *artifact.Signer
}

// New starts the service. Its signing key is new each time, so links and
// tokens from before a restart stop working (the runs they belong to do too).
func New(c Config) (*Service, error) {
	if c.Dir == "" || c.PublicURL == "" {
		return nil, errors.New("artifacts: Dir and PublicURL are required")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	signer, err := artifact.NewSigner(key)
	if err != nil {
		return nil, err
	}
	var store blob.Store
	var dir *blob.Dir
	if c.S3 != nil {
		s3 := *c.S3
		s3.Prefix += "artifacts/"
		if store, err = blob.NewS3(s3); err != nil {
			return nil, err
		}
	} else {
		if dir, err = blob.NewDir(filepath.Join(c.Dir, "objects"), c.PublicURL, key); err != nil {
			return nil, err
		}
		store = dir
	}
	art, err := artifact.New(artifact.Config{Store: store, Signer: signer, BaseURL: c.PublicURL, SpoolDir: filepath.Join(c.Dir, "spool")})
	if err != nil {
		if dir != nil {
			dir.Close()
		}
		return nil, err
	}
	return &Service{art: art, dir: dir, signer: signer}, nil
}

// Signer is what signs the tokens and links of this service; the cache service
// uses the same one, so one token serves both, as on GitHub.
func (s *Service) Signer() *artifact.Signer { return s.signer }

// Mount puts the service's routes on mux: the Twirp API, the blob upload and
// the download links. Every one is guarded by a token or a signed link.
func (s *Service) Mount(mux *http.ServeMux) {
	h := s.art.Handler()
	mux.Handle("/twirp/", h)
	mux.Handle("/_blob/", h)
	if s.dir != nil {
		mux.Handle(blob.DownloadPrefix, s.dir.Handler())
	}
}

// JobEnv is what a step needs to reach the service as one job of one run:
// ACTIONS_RESULTS_URL and a token that opens only that run and creates
// artifacts only as that job.
func (s *Service) JobEnv(run int64, job string) (map[string]string, error) {
	return s.art.Env(artifact.Scope{Run: strconv.FormatInt(run, 10), Job: job}, tokenTTL)
}

// Close stops the service's background work.
func (s *Service) Close() {
	s.art.Close()
	if s.dir != nil {
		s.dir.Close()
	}
}
