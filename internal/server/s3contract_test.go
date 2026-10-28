//go:build s3contract

package server_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/the-inconvenience-store/nx-remote-cache/internal/server"
)

// TestS3ContractConcurrentUploadsCreateOneImmutableArtifact checks a real
// backend's conditional-write support. Configure AWS_REGION,
// AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, S3_BUCKET_NAME and optionally
// S3_ENDPOINT_URL; use a disposable bucket. Backends without conditional
// writes fail this test.
func TestS3ContractConcurrentUploadsCreateOneImmutableArtifact(t *testing.T) {
	t.Parallel()
	bucket := os.Getenv("S3_BUCKET_NAME")
	if bucket == "" {
		t.Fatal("configure S3_BUCKET_NAME, AWS_REGION, AWS credentials and optionally S3_ENDPOINT_URL")
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}
	h, err := server.New(context.Background(), &server.Config{
		AccessTokens: []string{accessToken}, Bucket: bucket, Region: region,
		Endpoint:    os.Getenv("S3_ENDPOINT_URL"),
		AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		FailOpen: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertAtomicUpload(t, h, "s3contract-"+strconv.FormatInt(time.Now().UnixNano(), 10))
}
