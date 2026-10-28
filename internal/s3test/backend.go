//go:build integration || e2e

// Package s3test starts disposable S3-compatible servers for integration tests.
package s3test

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
)

const (
	// Bucket is created on every backend before Start returns.
	Bucket         = "nx-cloud"
	startupTimeout = 2 * time.Minute
	stopTimeout    = 10 * time.Second
	pollInterval   = 250 * time.Millisecond
)

// Backend is a running S3-compatible server with Bucket available.
type Backend struct {
	Name            string
	Region          string
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	// EnforcesConditionalWrites reports whether the backend atomically rejects
	// PutObject with If-None-Match: * when the key exists.
	EnforcesConditionalWrites bool

	container testcontainers.Container
}

// Starter launches a backend that is removed when the test ends.
type Starter func(tb testing.TB) *Backend

// Starters lists every backend the feature suite runs against.
func Starters() map[string]Starter {
	return map[string]Starter{garageName: StartGarage, versityName: StartVersityGW}
}

// Stop simulates a storage outage by stopping the container.
func (b *Backend) Stop(tb testing.TB) {
	tb.Helper()
	timeout := stopTimeout
	if err := b.container.Stop(context.WithoutCancel(tb.Context()), &timeout); err != nil {
		tb.Fatalf("stop %s: %v", b.Name, err)
	}
}

// Client returns an S3 client for inspecting the backend directly.
func (b *Backend) Client() *s3.Client {
	return s3.New(s3.Options{
		Region:       b.Region,
		BaseEndpoint: aws.String(b.Endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(b.AccessKeyID, b.SecretAccessKey, ""),
	})
}

func (b *Backend) endpoint(tb testing.TB, port string) {
	tb.Helper()
	endpoint, err := b.container.PortEndpoint(tb.Context(), port, "http")
	if err != nil {
		tb.Fatalf("%s endpoint: %v", b.Name, err)
	}
	b.Endpoint = endpoint
}

// waitFor retries op until it succeeds or the startup timeout expires.
func (b *Backend) waitFor(tb testing.TB, what string, op func(*s3.Client) error) {
	tb.Helper()
	client := b.Client()
	deadline := time.Now().Add(startupTimeout)
	for {
		err := op(client)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			tb.Fatalf("%s: %s: %v", b.Name, what, err)
		}
		time.Sleep(pollInterval)
	}
}

func (b *Backend) waitForBucket(tb testing.TB) {
	tb.Helper()
	b.waitFor(tb, "bucket not ready", func(c *s3.Client) error {
		_, err := c.HeadBucket(tb.Context(), &s3.HeadBucketInput{Bucket: aws.String(Bucket)})
		return err //nolint:wrapcheck // reported verbatim by waitFor
	})
}
