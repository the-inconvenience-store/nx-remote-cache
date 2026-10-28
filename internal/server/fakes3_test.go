package server_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/the-inconvenience-store/nx-remote-cache/internal/server"
)

const (
	testBucket    = "cache"
	accessToken   = "test-token"
	readOnlyToken = "read-only-token"
)

// fakeS3 is a minimal path-style S3 endpoint for one bucket. Override lets a
// test replace the response for selected requests; calls counts every request.
type fakeS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	override func(w http.ResponseWriter, r *http.Request) bool
	calls    atomic.Int32
	URL      string
}

func newFakeS3(t *testing.T) *fakeS3 {
	t.Helper()
	f := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

func (f *fakeS3) object(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.objects[key]
	return body, ok
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	if f.override != nil && f.override(w, r) {
		return
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != testBucket {
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist")
		return
	}
	if key == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	switch r.Method {
	case http.MethodHead:
		if _, ok := f.object(key); !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		body, ok := f.object(key)
		if !ok {
			writeS3Error(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	case http.MethodPut:
		f.put(w, r, key)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeS3) put(w http.ResponseWriter, r *http.Request, key string) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeS3Error(w, http.StatusBadRequest, "IncompleteBody", err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.objects[key]; exists && r.Header.Get("If-None-Match") == "*" {
		writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed", "At least one precondition failed")
		return
	}
	f.objects[key] = data
	w.Header().Set("ETag", `"etag"`)
	w.WriteHeader(http.StatusOK)
}

func writeS3Error(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, message)
}

type options struct {
	readOnly        bool
	failOpen        *bool
	logs            *bytes.Buffer
	maxArtifactSize int64
}

func newHandler(t *testing.T, endpoint string, opts options) http.Handler {
	t.Helper()
	failOpen := true
	if opts.failOpen != nil {
		failOpen = *opts.failOpen
	}
	logOut := io.Discard
	if opts.logs != nil {
		logOut = &lockedWriter{w: opts.logs}
	}
	cfg := server.Config{
		AccessToken:     accessToken,
		Bucket:          testBucket,
		Region:          "us-east-1",
		Endpoint:        endpoint,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		FailOpen:        failOpen,
		Logger:          slog.New(slog.NewTextHandler(logOut, nil)),
	}
	if opts.readOnly {
		cfg.ReadOnlyToken = readOnlyToken
	}
	cfg.MaxArtifactSize = opts.maxArtifactSize
	h, err := server.New(context.Background(), &cfg)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return h
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

type call struct {
	method string
	path   string
	token  string
	body   io.Reader
	// length is the declared Content-Length; -1 omits it.
	length int64
}

func do(t *testing.T, h http.Handler, c call) (int, string) {
	t.Helper()
	header := ""
	if c.token != "" {
		header = "Bearer " + c.token
	}
	return doWithHeader(t, h, c, header)
}

func doWithHeader(t *testing.T, h http.Handler, c call, authorization string) (int, string) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), c.method, c.path, c.body)
	req.ContentLength = c.length
	// A real server only sees the header when the client sent one.
	if c.body != nil && c.length >= 0 {
		req.Header.Set("Content-Length", strconv.FormatInt(c.length, 10))
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func put(t *testing.T, h http.Handler, key, token, body string) (int, string) {
	t.Helper()
	return do(t, h, call{
		method: http.MethodPut, path: "/v1/cache/" + key, token: token,
		body: strings.NewReader(body), length: int64(len(body)),
	})
}

func get(t *testing.T, h http.Handler, key, token string) (int, string) {
	t.Helper()
	return do(t, h, call{method: http.MethodGet, path: "/v1/cache/" + key, token: token})
}

func expect(t *testing.T, gotStatus int, gotBody string, wantStatus int, wantBody string) {
	t.Helper()
	if gotStatus != wantStatus || gotBody != wantBody {
		t.Fatalf("got %d %q, want %d %q", gotStatus, gotBody, wantStatus, wantBody)
	}
}
