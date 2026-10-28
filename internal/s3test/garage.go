//go:build integration || e2e

package s3test

import (
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	garageImage  = "dxflrs/garage:v2.5.0"
	garagePort   = "3900/tcp"
	garageConfig = `metadata_dir = "/var/lib/garage/meta"
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
	configFileMode = 0o644
	// garageName names the backend and is also its S3 region.
	garageName = "garage"
)

// StartGarage launches single-node Garage. Garage accepts If-None-Match but
// does not enforce it, so concurrent writers can overwrite each other.
func StartGarage(tb testing.TB) *Backend {
	tb.Helper()
	b := &Backend{
		Name:            garageName,
		Region:          garageName,
		AccessKeyID:     "GK11111111111111111111111111",
		SecretAccessKey: "1111111111111111111111111111111111111111111111111111111111111111",
	}
	container, err := testcontainers.Run(tb.Context(), garageImage,
		testcontainers.WithExposedPorts(garagePort),
		testcontainers.WithEnv(map[string]string{
			"GARAGE_DEFAULT_ACCESS_KEY": b.AccessKeyID,
			"GARAGE_DEFAULT_SECRET_KEY": b.SecretAccessKey,
			"GARAGE_DEFAULT_BUCKET":     Bucket,
		}),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			Reader:            strings.NewReader(garageConfig),
			ContainerFilePath: "/etc/garage.toml",
			FileMode:          configFileMode,
		}),
		testcontainers.WithCmd("/garage", "server", "--single-node", "--default-bucket"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort(garagePort).WithStartupTimeout(startupTimeout)),
	)
	testcontainers.CleanupContainer(tb, container)
	if err != nil {
		tb.Fatalf("start garage: %v", err)
	}
	b.container = container
	b.endpoint(tb, garagePort)
	b.waitForBucket(tb)
	return b
}
