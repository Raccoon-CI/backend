package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Raccoon-CI/backend/internal/server/config"
	"github.com/Raccoon-CI/backend/internal/server/storagecheck"
	"github.com/Raccoon-CI/backend/internal/server/vault"
	"github.com/Raccoon-CI/lib/pkg/blob"
)

// openVault makes the vault the secrets are sealed with, from the master key
// (and the one from before a rotation). nil, with no error, when no master key
// is given.
func openVault(id, oldID string) (*vault.Vault, error) {
	mk, err := config.Secret("RACCOONCI_MASTER_KEY")
	if err != nil {
		return nil, err
	}
	old, err := config.Secret("RACCOONCI_MASTER_KEY_OLD")
	if err != nil {
		return nil, err
	}
	if mk == "" {
		if old != "" {
			return nil, errors.New("RACCOONCI_MASTER_KEY_OLD is set without RACCOONCI_MASTER_KEY")
		}
		return nil, nil
	}
	key, err := vault.ParseKey(mk)
	if err != nil {
		return nil, fmt.Errorf("RACCOONCI_MASTER_KEY: %w", err)
	}
	keys := map[string][]byte{id: key}
	if old != "" {
		if oldID == "" || oldID == id {
			return nil, errors.New("RACCOONCI_MASTER_KEY_OLD needs -master-key-old-id, different from -master-key-id")
		}
		oldKey, err := vault.ParseKey(old)
		if err != nil {
			return nil, fmt.Errorf("RACCOONCI_MASTER_KEY_OLD: %w", err)
		}
		keys[oldID] = oldKey
	}
	return vault.New(keys, id)
}

// storageConfig reads the bucket's settings; its key and secret come from the
// environment or files like every secret.
func storageConfig(bucket, endpoint, region, prefix string, pathStyle bool) (*blob.S3Config, error) {
	key, err := config.Required("RACCOONCI_STORAGE_S3_KEY")
	if err != nil {
		return nil, err
	}
	secret, err := config.Required("RACCOONCI_STORAGE_S3_SECRET")
	if err != nil {
		return nil, err
	}
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &blob.S3Config{Endpoint: endpoint, Region: region, Bucket: bucket, AccessKey: key, SecretKey: secret, PathStyle: pathStyle, Prefix: prefix}, nil
}

// checkStorage tries the bucket and refuses to go on with one that does not
// work; one that anyone can read is a warning in the log.
func checkStorage(ctx context.Context, cfg *blob.S3Config, log *slog.Logger) error {
	store, err := blob.NewS3(*cfg)
	if err != nil {
		return err
	}
	warnings, err := storagecheck.Check(ctx, store, storagecheck.S3Probes(*cfg))
	if err != nil {
		return fmt.Errorf("the bucket %q does not work: %w", cfg.Bucket, err)
	}
	for _, w := range warnings {
		log.Warn(w, "bucket", cfg.Bucket)
	}
	log.Info("the bucket works", "bucket", cfg.Bucket, "endpoint", cfg.Endpoint)
	return nil
}
