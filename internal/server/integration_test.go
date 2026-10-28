//go:build integration

package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/the-inconvenience-store/nx-remote-cache/internal/garagetest"
	"github.com/the-inconvenience-store/nx-remote-cache/internal/server"
)

func garageServer(t *testing.T, g *garagetest.Garage, bucket string, logs *bytes.Buffer) *httptest.Server {
	t.Helper()
	if logs == nil {
		logs = &bytes.Buffer{}
	}
	h, err := server.New(context.Background(), &server.Config{
		AccessToken: accessToken, ReadOnlyToken: readOnlyToken,
		Bucket: bucket, Region: garagetest.Region, Endpoint: g.Endpoint,
		AccessKeyID: garagetest.AccessKeyID, SecretAccessKey: garagetest.SecretAccessKey,
		FailOpen: true, Logger: testLogger(logs),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
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

func TestGarageRoundTripsLargeStreamedArtifact(t *testing.T) {
	t.Parallel()
	g := garagetest.Start(t)
	srv := garageServer(t, g, garagetest.Bucket, nil)
	const size = 64 << 20

	upload := call{
		method: http.MethodPut,
		path:   "/v1/cache/large",
		token:  accessToken,
		body:   &zeroes{left: size},
		length: size,
	}
	if status, body := send(t, srv.URL, upload); status != http.StatusOK {
		t.Fatalf("upload: %d %s", status, body)
	}
	status, body := send(t, srv.URL, getCall("/v1/cache/large", readOnlyToken))
	if status != http.StatusOK || sha256.Sum256([]byte(body)) != sha256.Sum256(make([]byte, size)) {
		t.Fatalf("download: status %d, %d bytes, digest mismatch", status, len(body))
	}

	status, body = send(t, srv.URL, putCall("large", accessToken, "x"))
	expect(t, status, body, http.StatusConflict, "Cannot override an existing record")
	status, body = send(t, srv.URL, putCall("denied", readOnlyToken, "x"))
	expect(t, status, body, http.StatusForbidden, "Read-only credentials cannot upload artifacts")
	status, body = send(t, srv.URL, getCall("/v1/cache/denied", accessToken))
	expect(t, status, body, http.StatusNotFound, "The record was not found")
	status, body = send(t, srv.URL, getCall("/ready", ""))
	expect(t, status, body, http.StatusOK, "OK")
}

func TestGarageOutageDegradesGracefully(t *testing.T) {
	t.Parallel()
	g := garagetest.Start(t)
	var logs bytes.Buffer
	srv := garageServer(t, g, garagetest.Bucket, &logs)
	g.Stop(t)

	status, body := send(t, srv.URL, getCall("/v1/cache/abc", accessToken))
	expect(t, status, body, http.StatusNotFound, "The record was not found")
	status, body = send(t, srv.URL, putCall("abc", accessToken, "data"))
	expect(t, status, body, http.StatusForbidden, "Cache storage unavailable; artifact not stored")
	status, body = send(t, srv.URL, getCall("/ready", ""))
	expect(t, status, body, http.StatusServiceUnavailable, "Storage unavailable")
	if got := strings.Count(logs.String(), `msg="Storage unavailable"`); got != 2 {
		t.Fatalf("logged %d degraded responses, want 2", got)
	}
}

func TestGarageMissingBucketIsUnreadyAndLogged(t *testing.T) {
	t.Parallel()
	g := garagetest.Start(t)
	var logs bytes.Buffer
	srv := garageServer(t, g, "does-not-exist", &logs)

	status, body := send(t, srv.URL, getCall("/ready", ""))
	expect(t, status, body, http.StatusServiceUnavailable, "Storage unavailable")
	status, body = send(t, srv.URL, getCall("/v1/cache/abc", accessToken))
	expect(t, status, body, http.StatusNotFound, "The record was not found")
	if !strings.Contains(logs.String(), "S3 bucket not found") {
		t.Fatalf("missing bucket not logged as storage failure:\n%s", logs.String())
	}
}

func TestGarageAbortedUploadStoresNothing(t *testing.T) {
	t.Parallel()
	g := garagetest.Start(t)
	srv := garageServer(t, g, garagetest.Bucket, nil)
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(bytes.Repeat([]byte("x"), 1<<20))
		_ = pw.CloseWithError(errClientGone)
	}()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+"/v1/cache/aborted", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = 64 << 20
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}

	status, body := send(t, srv.URL, getCall("/v1/cache/aborted", accessToken))
	expect(t, status, body, http.StatusNotFound, "The record was not found")
	status, body = send(t, srv.URL, putCall("after", accessToken, "ok"))
	expect(t, status, body, http.StatusOK, "Successfully uploaded")
}
