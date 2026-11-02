// Package storage keeps where each organization's artifacts and caches live: a
// bucket the organization provides (R2, S3, any S3-compatible store), with its
// settings and access key sealed in the database, tried before they are saved.
// An organization without one uses the server's own (a bucket given at start,
// or a directory on its disk).
//
// The settings are stored in the organization's scope under a name no secret
// can have and a kind of their own, so a workflow can neither name them nor
// list them, and the secrets API cannot overwrite or delete them.
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/Raccoon-CI/raccoonci/internal/server/secrets"
	"github.com/Raccoon-CI/raccoonci/internal/server/storagecheck"
	"github.com/Raccoon-CI/raccoonci/internal/server/store"
	"github.com/Raccoon-CI/raccoonci/internal/server/vault"
	"github.com/Raccoon-CI/raccoonci/pkg/blob"
)

// settingName is the name the settings are kept under: the `$` cannot be in a
// secret's name.
const settingName = "$STORAGE"

// ErrInvalid is wrapped by what is wrong with the settings given.
var ErrInvalid = errors.New("invalid")

// Settings are a bucket's settings, as given and as sealed.
type Settings struct {
	Endpoint  string `json:"endpoint,omitempty"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix,omitempty"`
	PathStyle bool   `json:"path_style,omitempty"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// S3 is the settings as the blob package takes them.
func (s Settings) S3() blob.S3Config {
	prefix := s.Prefix
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return blob.S3Config{Endpoint: s.Endpoint, Region: s.Region, Bucket: s.Bucket, Prefix: prefix,
		PathStyle: s.PathStyle, AccessKey: s.AccessKey, SecretKey: s.SecretKey}
}

var (
	bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	orgName    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

func (s Settings) validate() error {
	switch {
	case !bucketName.MatchString(s.Bucket):
		return invalid("the bucket's name is 3 to 63 lower-case letters, digits, dots and dashes")
	case s.AccessKey == "" || s.SecretKey == "":
		return invalid("the access key and the secret key are both needed")
	case len(s.AccessKey) > 512 || len(s.SecretKey) > 512 || len(s.Prefix) > 256 || len(s.Region) > 64:
		return invalid("a setting is too long")
	}
	if s.Endpoint != "" && !strings.HasPrefix(s.Endpoint, "https://") {
		return invalid("the endpoint must be https")
	}
	return nil
}

// View is what is said of an organization's settings: everything but the
// keys, which are never read back.
type View struct {
	Configured bool
	Endpoint   string
	Region     string
	Bucket     string
	Prefix     string
	PathStyle  bool
}

// Service keeps the settings.
type Service struct {
	Store store.Store
	Vault *vault.Vault
	// Default is the server's own bucket, for an organization with none; nil
	// means its own disk.
	Default *blob.S3Config
	// Check tries a bucket (default: storagecheck against the real one).
	Check func(ctx context.Context, cfg blob.S3Config) (warnings []string, err error)
}

// ServerDefault is the name that stands for the server's own bucket in Put,
// View and Delete: the bucket organizations without one of their own use. It
// is kept the same way an organization's is (sealed under the master key,
// tried before it is saved), so it does not have to be given as flags and
// environment variables at start; those still work, and a bucket saved here
// wins over them.
const ServerDefault = "*"

// serverScope is the scope the server's own settings are kept under: no
// organization or repository scope looks like it.
const serverScope = "server"

func scope(org string) (string, error) {
	if org == ServerDefault {
		return serverScope, nil
	}
	org = strings.ToLower(org)
	if !orgName.MatchString(org) {
		return "", invalid("an organization's name is letters, digits, dots, dashes and underscores")
	}
	return secrets.Org(org), nil
}

func (s *Service) check(ctx context.Context, cfg blob.S3Config) ([]string, error) {
	if s.Check != nil {
		return s.Check(ctx, cfg)
	}
	b, err := blob.NewS3(cfg)
	if err != nil {
		return nil, err
	}
	return storagecheck.Check(ctx, b, storagecheck.S3Probes(cfg))
}

// Put tries the bucket and, only if it works, seals and saves its settings.
// The warnings are what the check found that does not stop it from being used
// (a bucket anyone can read).
func (s *Service) Put(ctx context.Context, org string, st Settings) (warnings []string, err error) {
	sc, err := scope(org)
	if err != nil {
		return nil, err
	}
	if err := st.validate(); err != nil {
		return nil, err
	}
	warnings, err = s.check(ctx, st.S3())
	if err != nil {
		return nil, invalid("the bucket does not work: %v", err)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	keyID, sealed, err := s.Vault.Seal(sc, settingName, raw)
	if err != nil {
		return nil, err
	}
	err = s.Store.PutSecret(ctx, store.Secret{Scope: sc, Name: settingName, Kind: store.KindStorage, KeyID: keyID, Value: sealed})
	return warnings, err
}

func (s *Service) load(ctx context.Context, sc string) (Settings, string, bool, error) {
	sec, err := s.Store.GetSecret(ctx, sc, settingName)
	if errors.Is(err, store.ErrNotFound) {
		return Settings{}, "", false, nil
	}
	if err != nil {
		return Settings{}, "", false, err
	}
	if sec.Kind != store.KindStorage {
		return Settings{}, "", false, errors.New("storage: the setting has the wrong kind")
	}
	pt, err := s.Vault.Open(sc, settingName, sec.KeyID, sec.Value)
	if err != nil {
		return Settings{}, "", false, fmt.Errorf("storage: the settings of %s cannot be opened: %w", sc, err)
	}
	var st Settings
	if err := json.Unmarshal(pt, &st); err != nil {
		return Settings{}, "", false, errors.New("storage: the settings are damaged")
	}
	return st, strconv.FormatInt(sec.UpdatedAt.UnixMilli(), 10), true, nil
}

// View describes an organization's settings without its keys.
func (s *Service) View(ctx context.Context, org string) (View, error) {
	sc, err := scope(org)
	if err != nil {
		return View{}, err
	}
	st, _, ok, err := s.load(ctx, sc)
	if err != nil || !ok {
		return View{}, err
	}
	return View{Configured: true, Endpoint: st.Endpoint, Region: st.Region, Bucket: st.Bucket, Prefix: st.Prefix, PathStyle: st.PathStyle}, nil
}

// Delete removes an organization's settings; its objects stay where they are.
func (s *Service) Delete(ctx context.Context, org string) (bool, error) {
	sc, err := scope(org)
	if err != nil {
		return false, err
	}
	return s.Store.DeleteSecret(ctx, sc, settingName)
}

// For says which bucket an organization's objects go to: its own if it has
// one, else the server's default (nil: the server's disk). version changes when
// the settings do, so what was made from them can be made again.
func (s *Service) For(ctx context.Context, org string) (cfg *blob.S3Config, version string, err error) {
	sc, err := scope(org)
	if err != nil {
		return nil, "", err
	}
	st, version, ok, err := s.load(ctx, sc)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		// No bucket of its own: the one saved for the server, else the one the
		// server was started with, else the disk.
		st, version, ok, err = s.load(ctx, serverScope)
		if err != nil {
			return nil, "", err
		}
		if !ok {
			return s.Default, "default", nil
		}
		c := st.S3()
		return &c, "server-" + version, nil
	}
	c := st.S3()
	return &c, version, nil
}
