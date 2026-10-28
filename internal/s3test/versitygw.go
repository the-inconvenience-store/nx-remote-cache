//go:build integration || e2e

package s3test

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	versityImage = "versity/versitygw:v1.10.0"
	versityPort  = "7070/tcp"
	versityName  = "versitygw"
)

// StartVersityGW launches the Versity S3 gateway over a POSIX directory. It
// enforces If-None-Match atomically, so racing writers get exactly one winner.
func StartVersityGW(tb testing.TB) *Backend {
	tb.Helper()
	b := &Backend{
		Name:                      versityName,
		Region:                    "us-east-1",
		AccessKeyID:               "nxremotecacheroot",
		SecretAccessKey:           "nx-remote-cache-versitygw-secret",
		EnforcesConditionalWrites: true,
	}
	container, err := testcontainers.Run(tb.Context(), versityImage,
		testcontainers.WithExposedPorts(versityPort),
		testcontainers.WithEnv(map[string]string{
			"ROOT_ACCESS_KEY": b.AccessKeyID,
			"ROOT_SECRET_KEY": b.SecretAccessKey,
		}),
		testcontainers.WithCmd("--port", ":7070", "posix", "/tmp"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort(versityPort).WithStartupTimeout(startupTimeout)),
	)
	testcontainers.CleanupContainer(tb, container)
	if err != nil {
		tb.Fatalf("start versitygw: %v", err)
	}
	b.container = container
	b.endpoint(tb, versityPort)
	b.waitFor(tb, "create bucket", func(c *s3.Client) error {
		_, err := c.CreateBucket(tb.Context(), &s3.CreateBucketInput{Bucket: aws.String(Bucket)})
		if _, owned := errors.AsType[*types.BucketAlreadyOwnedByYou](err); owned {
			return nil
		}
		return err //nolint:wrapcheck // reported verbatim by waitFor
	})
	b.waitForBucket(tb)
	return b
}
