package server_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/the-inconvenience-store/nx-remote-cache/internal/server"
)

func unreachableEndpoint(t *testing.T) string {
	t.Helper()
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	return "http://" + addr
}

func failingS3(t *testing.T, status int, code string) string {
	t.Helper()
	s3 := newFakeS3(t)
	s3.override = func(w http.ResponseWriter, r *http.Request) bool {
		_, _ = io.Copy(io.Discard, r.Body)
		writeS3Error(w, status, code, "injected failure")
		return true
	}
	return s3.URL
}

func TestReadOnlyTokenDownloadsButCannotUpload(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	h := newHandler(t, s3.URL, options{readOnly: true})
	put(t, h, "abc", accessToken, "trusted")

	status, body := get(t, h, "abc", readOnlyToken)
	expect(t, status, body, http.StatusOK, "trusted")

	bodyRead := false
	status, body = do(t, h, call{
		method: http.MethodPut, path: "/v1/cache/denied", token: readOnlyToken,
		body: readerFunc(func([]byte) (int, error) { bodyRead = true; return 0, io.EOF }), length: 4,
	})
	expect(t, status, body, http.StatusForbidden, "Read-only credentials cannot upload artifacts")
	if bodyRead {
		t.Fatal("read-only upload body was consumed")
	}
	if _, stored := s3.object("denied"); stored {
		t.Fatal("read-only upload created an object")
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestStorageFailuresDegradeToMissAndNotStored(t *testing.T) {
	t.Parallel()
	endpoints := map[string]string{
		"server error":     failingS3(t, http.StatusInternalServerError, "InternalError"),
		"throttling":       failingS3(t, http.StatusServiceUnavailable, "SlowDown"),
		"permission error": failingS3(t, http.StatusForbidden, "AccessDenied"),
		"credential error": failingS3(t, http.StatusForbidden, "InvalidAccessKeyId"),
		"expired token":    failingS3(t, http.StatusBadRequest, "ExpiredToken"),
		"write conflict":   failingS3(t, http.StatusConflict, "ConditionalRequestConflict"),
		"unreachable":      unreachableEndpoint(t),
	}
	for name, endpoint := range endpoints {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			h := newHandler(t, endpoint, options{logs: &logs})
			status, body := get(t, h, "abc", accessToken)
			expect(t, status, body, http.StatusNotFound, "The record was not found")
			status, body = put(t, h, "abc", accessToken, "data")
			expect(t, status, body, http.StatusForbidden, "Cache storage unavailable; artifact not stored")
			if got := strings.Count(logs.String(), `msg="Storage unavailable"`); got != 2 {
				t.Fatalf("logged %d storage failures, want 2:\n%s", got, logs.String())
			}
		})
	}
}

func TestFailClosedReturns503OnStorageFailure(t *testing.T) {
	t.Parallel()
	h := newHandler(t, failingS3(t, http.StatusInternalServerError, "InternalError"),
		options{failOpen: new(false)})
	status, body := get(t, h, "abc", accessToken)
	expect(t, status, body, http.StatusServiceUnavailable, "Cache storage unavailable")
	status, body = put(t, h, "abc", accessToken, "data")
	expect(t, status, body, http.StatusServiceUnavailable, "Cache storage unavailable")
}

func TestMissingBucketIsAStorageFailureNotAMiss(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	var logs bytes.Buffer
	h, err := server.New(context.Background(), &server.Config{
		AccessToken: accessToken, Bucket: "does-not-exist", Region: "us-east-1",
		Endpoint: s3.URL, AccessKeyID: "test", SecretAccessKey: "test", FailOpen: false,
		Logger: testLogger(&logs),
	})
	if err != nil {
		t.Fatal(err)
	}
	status, body := get(t, h, "abc", accessToken)
	expect(t, status, body, http.StatusServiceUnavailable, "Cache storage unavailable")
	if !strings.Contains(logs.String(), "S3 bucket not found") {
		t.Fatalf("missing bucket not logged:\n%s", logs.String())
	}
}

func TestGenuineMissStays404AndUnloggedWhenFailClosed(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	h := newHandler(t, newFakeS3(t).URL, options{failOpen: new(false), logs: &logs})
	status, body := get(t, h, "missing", accessToken)
	expect(t, status, body, http.StatusNotFound, "The record was not found")
	if strings.Contains(logs.String(), "Storage unavailable") {
		t.Fatalf("genuine miss logged as storage failure:\n%s", logs.String())
	}
}

func TestBadTokenStays401DuringStorageOutage(t *testing.T) {
	t.Parallel()
	h := newHandler(t, failingS3(t, http.StatusInternalServerError, "InternalError"), options{})
	status, body := get(t, h, "abc", "wrong")
	expect(t, status, body, http.StatusUnauthorized, "Missing or invalid authentication token")
	status, body = put(t, h, "abc", "wrong", "data")
	expect(t, status, body, http.StatusUnauthorized, "Missing or invalid authentication token")
}

func TestDownloadTreatsSilentStorageAsMissAfterFiveSeconds(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	s3.override = func(_ http.ResponseWriter, _ *http.Request) bool {
		<-release
		return true
	}
	h := newHandler(t, s3.URL, options{})
	start := time.Now()
	status, body := get(t, h, "abc", accessToken)
	expect(t, status, body, http.StatusNotFound, "The record was not found")
	if elapsed := time.Since(start); elapsed < 4900*time.Millisecond || elapsed > 8*time.Second {
		t.Fatalf("degraded after %s, want about 5s", elapsed)
	}
}

func TestDownloadBodyMayStreamPastHeaderTimeout(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	s3.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet {
			return false
		}
		_, _ = io.WriteString(w, "first,")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// Real clock on purpose: the body must outlive the 5s header deadline.
		time.Sleep(5500 * time.Millisecond)
		_, _ = io.WriteString(w, "last")
		return true
	}
	h := newHandler(t, s3.URL, options{})
	status, body := get(t, h, "abc", accessToken)
	expect(t, status, body, http.StatusOK, "first,last")
}
