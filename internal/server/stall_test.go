package server_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/the-inconvenience-store/nx-remote-cache/internal/server"
)

const testStallTimeout = 300 * time.Millisecond

// stallServer serves a handler with a short stall timeout over a real
// connection, so the server can expire client deadlines. Close waits for the
// handler, so its log lines and metrics are complete afterwards.
func stallServer(t *testing.T, endpoint string, logs *bytes.Buffer) (*httptest.Server, *prometheus.Registry) {
	t.Helper()
	registry := prometheus.NewRegistry()
	srv := httptest.NewServer(newHandler(t, endpoint, options{logs: logs, configure: func(c *server.Config) {
		c.Metrics = registry
		c.StallTimeout = testStallTimeout
	}}))
	t.Cleanup(srv.Close)
	return srv, registry
}

func expectStalls(t *testing.T, registry *prometheus.Registry, samples ...string) {
	t.Helper()
	want := `
# HELP nx_cache_stalled_transfers_total Transfers stopped because the server received no bytes for the stall timeout, ` +
		`by HTTP method and the side it was waiting on.
# TYPE nx_cache_stalled_transfers_total counter
` + strings.Join(samples, "\n") + "\n"
	if err := testutil.GatherAndCompare(
		registry,
		strings.NewReader(want),
		"nx_cache_stalled_transfers_total",
	); err != nil {
		t.Fatal(err)
	}
}

func expectStallLog(t *testing.T, logs *bytes.Buffer, method, side string) {
	t.Helper()
	want := `msg="Transfer stalled" method=` + method
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, want) && strings.Contains(line, "side="+side) &&
			strings.Contains(line, "timeout="+testStallTimeout.String()) {
			return
		}
	}
	t.Fatalf("no %s stall log for side %s:\n%s", method, side, logs.String())
}

// fetch downloads key from srv. An error means the download failed or was
// truncated.
func fetch(t *testing.T, srv *httptest.Server, key string) (int, string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/cache/"+key, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

// upload sends body as key to srv with the declared length. An error means
// the upload failed before a response arrived.
func upload(t *testing.T, srv *httptest.Server, key string, body io.Reader, length int64) (int, string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+"/v1/cache/"+key, body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = length
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data), err
}

func TestDownloadStopsWhenStorageStallsMidBody(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	s3.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet {
			return false
		}
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "partial")
		_ = http.NewResponseController(w).Flush()
		<-release
		return true
	}
	var logs bytes.Buffer
	srv, registry := stallServer(t, s3.URL, &logs)

	start := time.Now()
	status, body, err := fetch(t, srv, "abc")
	if elapsed := time.Since(start); err == nil || elapsed < testStallTimeout || elapsed > 5*time.Second {
		t.Fatalf("got %d %q, %v after %s; want a failed download stopped after %s",
			status, body, err, elapsed, testStallTimeout)
	}
	srv.Close()
	expectStalls(t, registry, `nx_cache_stalled_transfers_total{method="GET",side="storage"} 1`)
	expectStallLog(t, &logs, "GET", "storage")
	expectRequestLog(t, &logs, map[string]string{"method": "GET", "status": "200", "bytes": "7", "outcome": "error"})
}

// slowChunks is an artifact that arrives in small pieces with gaps shorter than
// the stall timeout but a total time well over it.
func slowChunks() []string {
	return strings.Split("slow,but,steady,bytes,keep,the,transfer,alive,past,the,timeout", ",")
}

const slowGap = testStallTimeout / 3

func expectNoStalls(t *testing.T, registry *prometheus.Registry) {
	t.Helper()
	if n, err := testutil.GatherAndCount(registry, "nx_cache_stalled_transfers_total"); err != nil || n != 0 {
		t.Fatalf("counted %d stalled transfers, %v; want none", n, err)
	}
}

func TestSlowDownloadThatKeepsSendingCompletes(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	s3.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet {
			return false
		}
		for _, chunk := range slowChunks() {
			_, _ = io.WriteString(w, chunk)
			_ = http.NewResponseController(w).Flush()
			time.Sleep(slowGap)
		}
		return true
	}
	srv, registry := stallServer(t, s3.URL, nil)

	start := time.Now()
	status, body, err := fetch(t, srv, "abc")
	if err != nil || status != http.StatusOK || body != strings.Join(slowChunks(), "") {
		t.Fatalf("got %d %q, %v; want the whole artifact", status, body, err)
	}
	if elapsed := time.Since(start); elapsed < 3*testStallTimeout {
		t.Fatalf("download took %s; the test needs it to outlast the stall timeout several times", elapsed)
	}
	expectNoStalls(t, registry)
}

func TestUploadStopsWhenStorageStopsTakingTheBody(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	s3.override = func(_ http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPut {
			return false
		}
		_, _ = r.Body.Read(make([]byte, 1024))
		<-release
		return true
	}
	var logs bytes.Buffer
	srv, registry := stallServer(t, s3.URL, &logs)

	// Far larger than the socket buffers, so the server blocks sending it.
	const size = 256 << 20
	body := io.LimitReader(readerFunc(func(p []byte) (int, error) { return len(p), nil }), size)
	start := time.Now()
	status, resp, err := upload(t, srv, "abc", body, size)
	if elapsed := time.Since(start); elapsed < testStallTimeout || elapsed > 5*time.Second {
		t.Fatalf("upload ended after %s, want about %s", elapsed, testStallTimeout)
	}
	if err != nil {
		t.Fatal(err)
	}
	expect(t, status, resp, http.StatusForbidden, "Cache storage unavailable; artifact not stored")
	srv.Close()
	expectStalls(t, registry, `nx_cache_stalled_transfers_total{method="PUT",side="storage"} 1`)
	expectStallLog(t, &logs, "PUT", "storage")
	expectRequestLog(t, &logs, map[string]string{"method": "PUT", "status": "403", "outcome": "degraded"})
}

func TestSlowUploadThatKeepsSendingCompletes(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	srv, registry := stallServer(t, s3.URL, nil)

	chunks := slowChunks()
	want := strings.Join(chunks, "")
	body := readerFunc(func(p []byte) (int, error) {
		if len(chunks) == 0 {
			return 0, io.EOF
		}
		time.Sleep(slowGap)
		n := copy(p, chunks[0])
		chunks = chunks[1:]
		return n, nil
	})
	start := time.Now()
	status, resp, err := upload(t, srv, "abc", body, int64(len(want)))
	if err != nil {
		t.Fatal(err)
	}
	expect(t, status, resp, http.StatusOK, "Successfully uploaded")
	if elapsed := time.Since(start); elapsed < 3*testStallTimeout {
		t.Fatalf("upload took %s; the test needs it to outlast the stall timeout several times", elapsed)
	}
	if stored, _ := s3.object("abc"); string(stored) != want {
		t.Fatalf("stored %q, want %q", stored, want)
	}
	expectNoStalls(t, registry)
}

// A client that stops sending stalls the upload too: storage receives no bytes.
func TestUploadStopsWhenClientStopsSending(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	var logs bytes.Buffer
	srv, registry := stallServer(t, s3.URL, &logs)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	sent := false
	body := readerFunc(func(p []byte) (int, error) {
		if sent {
			<-release
			return 0, errClientGone
		}
		sent = true
		return copy(p, "first"), nil
	})
	// The server answers once it stops the upload; the client may or may not
	// read that answer while its body is stuck.
	if status, resp, err := upload(t, srv, "abc", body, 100); err == nil {
		expect(t, status, resp, http.StatusInternalServerError, "Internal server error")
	}
	srv.Close() // returns only once the handler has released the stuck body read
	expectStalls(t, registry, `nx_cache_stalled_transfers_total{method="PUT",side="client"} 1`)
	expectStallLog(t, &logs, "PUT", "client")
	expectRequestLog(t, &logs, map[string]string{"method": "PUT", "status": "500", "outcome": "error"})
	if _, stored := s3.object("abc"); stored {
		t.Fatal("stalled upload was stored")
	}
}

// A client that stops reading stalls the download too: the server stops
// receiving from storage once the connection's buffers are full.
func TestDownloadStopsWhenClientStopsReading(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	s3.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet {
			return false
		}
		chunk := make([]byte, 32<<10)
		for {
			if _, err := w.Write(chunk); err != nil {
				return true
			}
		}
	}
	var logs bytes.Buffer
	srv, registry := stallServer(t, s3.URL, &logs)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/cache/abc", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	srv.Close() // returns only once the handler has released the stuck write
	expectStalls(t, registry, `nx_cache_stalled_transfers_total{method="GET",side="client"} 1`)
	expectStallLog(t, &logs, "GET", "client")
	expectRequestLog(t, &logs, map[string]string{"method": "GET", "status": "200", "outcome": "error"})
}
