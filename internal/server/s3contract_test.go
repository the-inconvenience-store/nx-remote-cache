//go:build s3contract

package server_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/the-inconvenience-store/nx-remote-cache/internal/server"
)

// TestS3ContractConcurrentUploadsCreateOneImmutableArtifact checks a real
// backend's conditional-write support. It forces both uploads past the HEAD
// duplicate check before either body is sent, so only If-None-Match can stop
// the second writer. Configure AWS_REGION, AWS_ACCESS_KEY_ID,
// AWS_SECRET_ACCESS_KEY, S3_BUCKET_NAME and optionally S3_ENDPOINT_URL; use a
// disposable bucket. Backends without conditional writes fail this test.
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
		AccessToken: accessToken, Bucket: bucket, Region: region,
		Endpoint:    os.Getenv("S3_ENDPOINT_URL"),
		AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		FailOpen: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	key := "s3contract-" + time.Now().UTC().Format("20060102T150405.000000000")
	payloads := []string{"first artifact", "second artifact"}
	statuses := racePastDuplicateCheck(t, h, key, payloads)

	sorted := append([]int(nil), statuses...)
	sort.Ints(sorted)
	if sorted[0] != http.StatusOK || sorted[1] != http.StatusConflict {
		t.Fatalf("statuses %v, want one 200 and one 409", statuses)
	}
	winner := payloads[0]
	if statuses[1] == http.StatusOK {
		winner = payloads[1]
	}
	status, body := get(t, h, key, accessToken)
	expect(t, status, body, http.StatusOK, winner)

	status, _ = put(t, h, key, accessToken, "replacement")
	if status != http.StatusConflict {
		t.Fatalf("later overwrite: %d, want 409", status)
	}
	if status, body = get(t, h, key, accessToken); !bytes.Equal([]byte(body), []byte(winner)) {
		t.Fatalf("artifact changed after overwrite attempt: %d %q", status, body)
	}
}

// racePastDuplicateCheck uploads every payload to key concurrently and holds
// each body until all requests have passed HEAD and started their PUT.
func racePastDuplicateCheck(t *testing.T, h http.Handler, key string, payloads []string) []int {
	t.Helper()
	var arrived sync.WaitGroup
	arrived.Add(len(payloads))
	release := make(chan struct{})
	go func() { arrived.Wait(); close(release) }()

	statuses := make([]int, len(payloads))
	var uploads sync.WaitGroup
	for i, payload := range payloads {
		uploads.Go(func() {
			first := true
			body := readerFunc(func(p []byte) (int, error) {
				if !first {
					return 0, io.EOF
				}
				first = false
				arrived.Done()
				select {
				case <-release:
				case <-time.After(30 * time.Second):
					return 0, errClientGone
				}
				return copy(p, payload), nil
			})
			statuses[i], _ = do(t, h, call{
				method: http.MethodPut, path: "/v1/cache/" + key, token: accessToken,
				body: body, length: int64(len(payload)),
			})
		})
	}
	uploads.Wait()
	return statuses
}
