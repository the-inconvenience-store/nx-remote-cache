package server

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// NewS3Client exposes the storage client so tests can inspect request addresses.
//
//nolint:gochecknoglobals // test-only export
var NewS3Client = func(ctx context.Context, cfg *Config) (*s3.Client, error) {
	return newS3Client(ctx, cfg, newMetrics(nil).timeS3Calls)
}
