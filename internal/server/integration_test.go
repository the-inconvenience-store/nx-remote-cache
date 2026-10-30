//go:build integration

package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/the-inconvenience-store/nx-remote-cache/internal/s3test"
	"github.com/the-inconvenience-store/nx-remote-cache/internal/server"
)

const atomicRaceRounds = 20

// eachBackend runs test against a fresh container of every backend.
func eachBackend(t *testing.T, test func(t *testing.T, b *s3test.Backend)) {
	t.Helper()
	for name, start := range s3test.Starters() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			test(t, start(t))
		})
	}
}

type backendServer struct {
	bucket string
	secret string
	logs   *bytes.Buffer
}

func newBackendHandler(t *testing.T, b *s3test.Backend, opts backendServer) http.Handler {
	t.Helper()
	if opts.bucket == "" {
		opts.bucket = s3test.Bucket
	}
	if opts.secret == "" {
		opts.secret = b.SecretAccessKey
	}
	if opts.logs == nil {
		opts.logs = &bytes.Buffer{}
	}
	h, err := server.New(context.Background(), &server.Config{
		AccessTokens: []string{accessToken}, ReadOnlyTokens: []string{readOnlyToken},
		Bucket: opts.bucket, Region: b.Region, Endpoint: b.Endpoint,
		AccessKeyID: b.AccessKeyID, SecretAccessKey: opts.secret,
		FailOpen: true, Logger: testLogger(opts.logs), StallTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func backendServerURL(t *testing.T, b *s3test.Backend, opts backendServer) string {
	t.Helper()
	srv := httptest.NewServer(newBackendHandler(t, b, opts))
	t.Cleanup(srv.Close)
	return srv.URL
}

// send performs c against a real HTTP server, exercising net/http streaming.
func send(t *testing.T, baseURL string, c call) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), c.method, baseURL+c.path, c.body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = c.length
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(data)
}

func putCall(key, token, body string) call {
	return call{
		method: http.MethodPut, path: "/v1/cache/" + key, token: token,
		body: strings.NewReader(body), length: int64(len(body)),
	}
}

func getCall(path, token string) call {
	return call{method: http.MethodGet, path: path, token: token}
}

// zeroes streams n bytes without holding them in memory.
type zeroes struct{ left int64 }

func (z *zeroes) Read(p []byte) (int, error) {
	if z.left == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), z.left)
	clear(p[:n])
	z.left -= n
	return int(n), nil
}

func TestBackendRoundTripsLargeStreamedArtifact(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, b *s3test.Backend) { //nolint:thelper // subtest body; keep failure lines
		url := backendServerURL(t, b, backendServer{})
		const size = 64 << 20

		upload := call{
			method: http.MethodPut, path: "/v1/cache/large", token: accessToken,
			body: &zeroes{left: size}, length: size,
		}
		if status, body := send(t, url, upload); status != http.StatusOK {
			t.Fatalf("upload: %d %s", status, body)
		}
		status, body := send(t, url, getCall("/v1/cache/large", readOnlyToken))
		if status != http.StatusOK || sha256.Sum256([]byte(body)) != sha256.Sum256(make([]byte, size)) {
			t.Fatalf("download: status %d, %d bytes, digest mismatch", status, len(body))
		}

		status, body = send(t, url, putCall("large", accessToken, "x"))
		expect(t, status, body, http.StatusConflict, "Cannot override an existing record")
		status, body = send(t, url, putCall("denied", readOnlyToken, "x"))
		expect(t, status, body, http.StatusForbidden, "Read-only credentials cannot upload artifacts")
		status, body = send(t, url, getCall("/v1/cache/denied", accessToken))
		expect(t, status, body, http.StatusNotFound, "The record was not found")
		status, body = send(t, url, getCall("/ready", ""))
		expect(t, status, body, http.StatusOK, "OK")
	})
}

func TestBackendUploadRecordsWriterMetadata(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, b *s3test.Backend) { //nolint:thelper // subtest body
		const key = "metadata"
		status, body := send(t, backendServerURL(t, b, backendServer{}), putCall(key, accessToken, "artifact"))
		expect(t, status, body, http.StatusOK, "Successfully uploaded")

		head, err := b.Client().HeadObject(t.Context(), &s3.HeadObjectInput{
			Bucket: aws.String(s3test.Bucket), Key: aws.String(key),
		})
		if err != nil {
			t.Fatalf("HeadObject: %v", err)
		}
		if got, want := head.Metadata["nx-writer"], "4c5dc9b77089"; got != want {
			t.Errorf("nx-writer metadata = %q, want %q", got, want)
		}
		uploadedAt := head.Metadata["nx-uploaded-at"]
		if uploadedAt == "" {
			t.Fatal("nx-uploaded-at metadata is missing")
		}
		if _, err := time.Parse(time.RFC3339Nano, uploadedAt); err != nil {
			t.Errorf("nx-uploaded-at metadata %q is not RFC3339 UTC: %v", uploadedAt, err)
		}
	})
}

func TestBackendOutageDegradesGracefully(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, b *s3test.Backend) { //nolint:thelper // subtest body; keep failure lines
		var logs bytes.Buffer
		url := backendServerURL(t, b, backendServer{logs: &logs})
		b.Stop(t)

		status, body := send(t, url, getCall("/v1/cache/abc", accessToken))
		expect(t, status, body, http.StatusNotFound, "The record was not found")
		status, body = send(t, url, putCall("abc", accessToken, "data"))
		expect(t, status, body, http.StatusForbidden, "Cache storage unavailable; artifact not stored")
		status, body = send(t, url, getCall("/ready", ""))
		expect(t, status, body, http.StatusServiceUnavailable, "Storage unavailable")
		if got := strings.Count(logs.String(), `msg="Storage unavailable"`); got != 2 {
			t.Fatalf("logged %d degraded responses, want 2", got)
		}
	})
}

func TestBackendRejectingCredentialsDegradesGracefully(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, b *s3test.Backend) { //nolint:thelper // subtest body; keep failure lines
		var logs bytes.Buffer
		url := backendServerURL(t, b, backendServer{secret: "wrong-" + b.SecretAccessKey, logs: &logs})

		status, body := send(t, url, getCall("/v1/cache/abc", accessToken))
		expect(t, status, body, http.StatusNotFound, "The record was not found")
		status, body = send(t, url, putCall("abc", accessToken, "data"))
		expect(t, status, body, http.StatusForbidden, "Cache storage unavailable; artifact not stored")
		status, body = send(t, url, getCall("/ready", ""))
		expect(t, status, body, http.StatusServiceUnavailable, "Storage unavailable")
		if got := strings.Count(logs.String(), `msg="Storage unavailable"`); got != 2 {
			t.Fatalf("logged %d degraded responses, want 2:\n%s", got, logs.String())
		}
	})
}

func TestBackendMissingBucketIsUnreadyAndLogged(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, b *s3test.Backend) { //nolint:thelper // subtest body; keep failure lines
		var logs bytes.Buffer
		url := backendServerURL(t, b, backendServer{bucket: "does-not-exist", logs: &logs})

		status, body := send(t, url, getCall("/ready", ""))
		expect(t, status, body, http.StatusServiceUnavailable, "Storage unavailable")
		status, body = send(t, url, getCall("/v1/cache/abc", accessToken))
		expect(t, status, body, http.StatusNotFound, "The record was not found")
		if !strings.Contains(logs.String(), "S3 bucket not found") {
			t.Fatalf("missing bucket not logged as storage failure:\n%s", logs.String())
		}
	})
}

func TestBackendAbortedUploadStoresNothing(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, b *s3test.Backend) { //nolint:thelper // subtest body; keep failure lines
		url := backendServerURL(t, b, backendServer{})
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write(bytes.Repeat([]byte("x"), 1<<20))
			_ = pw.CloseWithError(errClientGone)
		}()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, url+"/v1/cache/aborted", pr)
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = 64 << 20
		req.Header.Set("Authorization", "Bearer "+accessToken)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}

		status, body := send(t, url, getCall("/v1/cache/aborted", accessToken))
		expect(t, status, body, http.StatusNotFound, "The record was not found")
		status, body = send(t, url, putCall("after", accessToken, "ok"))
		expect(t, status, body, http.StatusOK, "Successfully uploaded")
	})
}

// TestConditionalWriteBackendKeepsRacingUploadsAtomic proves end to end that
// If-None-Match closes the race the HEAD duplicate check leaves open.
func TestConditionalWriteBackendKeepsRacingUploadsAtomic(t *testing.T) {
	t.Parallel()
	b := s3test.StartVersityGW(t)
	if !b.EnforcesConditionalWrites {
		t.Fatal("versitygw must enforce conditional writes for this test")
	}
	var logs bytes.Buffer
	h := newBackendHandler(t, b, backendServer{logs: &logs})
	for round := range atomicRaceRounds {
		assertAtomicUpload(t, h, "race-"+strconv.Itoa(round))
	}
	if strings.Contains(logs.String(), "does not support conditional PUT") {
		t.Fatalf("server stopped sending conditional PUTs:\n%s", logs.String())
	}
}

// Escaped branch names put %, + and UTF-8 escapes into keys; every backend
// must store and isolate them.
func TestRepositoryTiersOnRealBackends(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, b *s3test.Backend) { //nolint:thelper // subtest body; keep failure lines
		issuer := newFakeIssuer(t)
		h := newHandler(t, b.Endpoint, options{configure: func(c *server.Config) {
			withExchange(issuer)(c)
			c.Bucket, c.Region = s3test.Bucket, b.Region
			c.AccessKeyID, c.SecretAccessKey = b.AccessKeyID, b.SecretAccessKey
		}})
		branch := issue(t, h, issuer, push("feature/x y+ü", true))
		pr := issue(t, h, issuer, pullRequest(1))
		trusted := issue(t, h, issuer, push("main", true))

		status, body := put(t, h, "b", branch, "branch artifact")
		expect(t, status, body, http.StatusOK, "Successfully uploaded")
		status, body = get(t, h, "b", branch)
		expect(t, status, body, http.StatusOK, "branch artifact")
		for _, other := range []string{pr, trusted} {
			status, body = get(t, h, "b", other)
			expect(t, status, body, http.StatusNotFound, "The record was not found")
		}

		status, body = put(t, h, "t", trusted, "trusted artifact")
		expect(t, status, body, http.StatusOK, "Successfully uploaded")
		status, body = get(t, h, "t", pr)
		expect(t, status, body, http.StatusOK, "trusted artifact")
	})
}
