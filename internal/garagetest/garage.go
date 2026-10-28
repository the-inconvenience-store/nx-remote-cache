//go:build integration || e2e

// Package garagetest starts a disposable single-node Garage S3 server for tests.
package garagetest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	image = "dxflrs/garage:v2.5.0"
	// Region, Bucket and the keys match the server's test configuration.
	Region          = "garage"
	Bucket          = "nx-cloud"
	AccessKeyID     = "GK11111111111111111111111111"
	SecretAccessKey = "1111111111111111111111111111111111111111111111111111111111111111"
	startupTimeout  = 2 * time.Minute
	pollInterval    = 250 * time.Millisecond
	stopTimeout     = 10 * time.Second
	configFileMode  = 0o644
	config          = `metadata_dir = "/var/lib/garage/meta"
data_dir = "/var/lib/garage/data"
db_engine = "sqlite"
replication_factor = 1
rpc_bind_addr = "[::]:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = "8318c7f67c86be019998d672267f932cd1c86a3b126a84b72208ccac9f64f80d"

[s3_api]
s3_region = "garage"
api_bind_addr = "[::]:3900"
root_domain = ".s3.garage.localhost"
`
)

// Garage is a running Garage container with the Bucket already created.
type Garage struct {
	Endpoint  string
	container testcontainers.Container
}

// Start launches Garage and removes it when the test ends.
func Start(tb testing.TB) *Garage {
	tb.Helper()
	ctx := tb.Context()
	container, err := testcontainers.Run(ctx, image,
		testcontainers.WithExposedPorts("3900/tcp"),
		testcontainers.WithEnv(map[string]string{
			"GARAGE_DEFAULT_ACCESS_KEY": AccessKeyID,
			"GARAGE_DEFAULT_SECRET_KEY": SecretAccessKey,
			"GARAGE_DEFAULT_BUCKET":     Bucket,
		}),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			Reader:            strings.NewReader(config),
			ContainerFilePath: "/etc/garage.toml",
			FileMode:          configFileMode,
		}),
		testcontainers.WithCmd("/garage", "server", "--single-node", "--default-bucket"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("3900/tcp").WithStartupTimeout(startupTimeout)),
	)
	testcontainers.CleanupContainer(tb, container)
	if err != nil {
		tb.Fatalf("start Garage: %v", err)
	}
	endpoint, err := container.PortEndpoint(ctx, "3900/tcp", "http")
	if err != nil {
		tb.Fatalf("Garage endpoint: %v", err)
	}
	g := &Garage{Endpoint: endpoint, container: container}
	g.waitForBucket(tb)
	return g
}

// Stop simulates a storage outage by stopping the container.
func (g *Garage) Stop(tb testing.TB) {
	tb.Helper()
	timeout := stopTimeout
	if err := g.container.Stop(context.WithoutCancel(tb.Context()), &timeout); err != nil {
		tb.Fatalf("stop Garage: %v", err)
	}
}

// Client returns an S3 client for inspecting the bucket directly.
func (g *Garage) Client() *s3.Client {
	return s3.New(s3.Options{
		Region:       Region,
		BaseEndpoint: aws.String(g.Endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(AccessKeyID, SecretAccessKey, ""),
	})
}

func (g *Garage) waitForBucket(tb testing.TB) {
	tb.Helper()
	client := g.Client()
	deadline := time.Now().Add(startupTimeout)
	for {
		_, err := client.HeadBucket(tb.Context(), &s3.HeadBucketInput{Bucket: aws.String(Bucket)})
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			tb.Fatalf("Garage bucket not ready: %v", err)
		}
		time.Sleep(pollInterval)
	}
}
