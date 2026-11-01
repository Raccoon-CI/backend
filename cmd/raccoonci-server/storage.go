package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"raccoonci/internal/blob"
	"raccoonci/internal/server/config"
	"raccoonci/internal/server/storagecheck"
)

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
