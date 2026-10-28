package server_test

import (
	"net/http"
	"testing"

	"github.com/the-inconvenience-store/nx-remote-cache/internal/server"
)

func TestHealthNeedsNoAuthOrStorage(t *testing.T) {
	t.Parallel()
	h := newHandler(t, "http://127.0.0.1:1", options{})
	status, body := do(t, h, call{method: http.MethodGet, path: "/health"})
	expect(t, status, body, http.StatusOK, "OK")
}

func TestUploadedArtifactCanBeDownloaded(t *testing.T) {
	t.Parallel()
	h := newHandler(t, newFakeS3(t).URL, options{})

	status, body := put(t, h, "abc", accessToken, "artifact bytes")
	expect(t, status, body, http.StatusOK, "Successfully uploaded")

	status, body = get(t, h, "abc", accessToken)
	expect(t, status, body, http.StatusOK, "artifact bytes")
}

func TestDownloadOfUnknownHashIsACacheMiss(t *testing.T) {
	t.Parallel()
	h := newHandler(t, newFakeS3(t).URL, options{})
	status, body := get(t, h, "missing", accessToken)
	expect(t, status, body, http.StatusNotFound, "The record was not found")
}

func TestCacheRoutesRejectMissingOrInvalidTokens(t *testing.T) {
	t.Parallel()
	h := newHandler(t, newFakeS3(t).URL, options{})
	headers := []string{"", "Bearer wrong", "Bearer ", "Basic test-token", "Bearer test-token extra"}
	for _, header := range headers {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			status, body := doWithHeader(t, h, call{method: method, path: "/v1/cache/abc"}, header)
			expect(t, status, body, http.StatusUnauthorized, "Missing or invalid authentication token")
		}
	}
}

func TestEveryListedTokenIsAccepted(t *testing.T) {
	t.Parallel()
	h := newHandler(t, newFakeS3(t).URL, options{configure: func(c *server.Config) {
		c.AccessTokens = []string{"old-write", "new-write"}
		c.ReadOnlyTokens = []string{"old-read", "new-read"}
	}})
	for _, token := range []string{"old-write", "new-write"} {
		status, body := put(t, h, token, token, "by "+token)
		expect(t, status, body, http.StatusOK, "Successfully uploaded")
	}
	for _, token := range []string{"old-write", "new-write", "old-read", "new-read"} {
		status, body := get(t, h, "old-write", token)
		expect(t, status, body, http.StatusOK, "by old-write")
	}
	status, body := put(t, h, "denied", "new-read", "data")
	expect(t, status, body, http.StatusForbidden, "Read-only credentials cannot upload artifacts")
}

func TestUploadRequiresContentLength(t *testing.T) {
	t.Parallel()
	h := newHandler(t, newFakeS3(t).URL, options{})
	status, body := do(t, h, call{
		method: http.MethodPut, path: "/v1/cache/abc", token: accessToken,
		body: http.NoBody, length: -1,
	})
	expect(t, status, body, http.StatusLengthRequired, "Content-Length header is required")
}

func TestUploadCannotOverrideExistingRecord(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	h := newHandler(t, s3.URL, options{})
	put(t, h, "abc", accessToken, "first")

	status, body := put(t, h, "abc", accessToken, "second")
	expect(t, status, body, http.StatusConflict, "Cannot override an existing record")
	if stored, _ := s3.object("abc"); string(stored) != "first" {
		t.Fatalf("stored %q, want first", stored)
	}
}

func TestConcurrentWriterLosingConditionalPutGetsConflict(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	var sawCondition bool
	s3.override = func(w http.ResponseWriter, r *http.Request) bool {
		switch r.Method {
		case http.MethodHead:
			// Another writer wins between this request's HEAD and PUT.
			w.WriteHeader(http.StatusNotFound)
			return true
		case http.MethodPut:
			sawCondition = r.Header.Get("If-None-Match") == "*"
			writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed", "At least one precondition failed")
			return true
		}
		return false
	}
	h := newHandler(t, s3.URL, options{})
	status, body := put(t, h, "abc", accessToken, "data")
	expect(t, status, body, http.StatusConflict, "Cannot override an existing record")
	if !sawCondition {
		t.Fatal("PUT did not send If-None-Match: *")
	}
}
