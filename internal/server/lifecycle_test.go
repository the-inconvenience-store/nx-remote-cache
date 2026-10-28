package server_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/the-inconvenience-store/nx-remote-cache/internal/server"
)

var errClientGone = errors.New("client disconnected")

func testLogger(w *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(&lockedWriter{w: w}, nil))
}

func TestReadyReportsBucketReachability(t *testing.T) {
	t.Parallel()
	status, body := do(t, newHandler(t, newFakeS3(t).URL, options{}), call{method: http.MethodGet, path: "/ready"})
	expect(t, status, body, http.StatusOK, "OK")

	for name, endpoint := range map[string]string{
		"denied":      failingS3(t, http.StatusForbidden, "AccessDenied"),
		"failing":     failingS3(t, http.StatusInternalServerError, "InternalError"),
		"unreachable": unreachableEndpoint(t),
	} {
		status, body := do(t, newHandler(t, endpoint, options{}), call{method: http.MethodGet, path: "/ready"})
		if status != http.StatusServiceUnavailable || body != "Storage unavailable" {
			t.Fatalf("%s: got %d %q", name, status, body)
		}
	}
}

func TestReadyGivesUpWithinTwoSeconds(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	s3.override = func(_ http.ResponseWriter, _ *http.Request) bool {
		<-release
		return true
	}
	start := time.Now()
	status, body := do(t, newHandler(t, s3.URL, options{}), call{method: http.MethodGet, path: "/ready"})
	expect(t, status, body, http.StatusServiceUnavailable, "Storage unavailable")
	if elapsed := time.Since(start); elapsed < 1900*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("ready answered after %s, want about 2s", elapsed)
	}
}

func TestUnsupportedConditionLosesOneArtifactThenUploadsUnconditionally(t *testing.T) {
	t.Parallel()
	for name, failure := range map[string][2]string{
		"not implemented":           {"NotImplemented", "A header you provided implies functionality that is not implemented"},
		"invalid request about INM": {"InvalidRequest", "If-None-Match is not supported"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s3 := newFakeS3(t)
			var conditionalPuts atomic.Int32
			s3.override = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method != http.MethodPut || r.Header.Get("If-None-Match") == "" {
					return false
				}
				conditionalPuts.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				writeS3Error(w, http.StatusNotImplemented, failure[0], failure[1])
				return true
			}
			h := newHandler(t, s3.URL, options{failOpen: new(false)})

			status, body := put(t, h, "first", accessToken, "one")
			expect(t, status, body, http.StatusForbidden, "Cache storage unavailable; artifact not stored")
			status, body = put(t, h, "second", accessToken, "two")
			expect(t, status, body, http.StatusOK, "Successfully uploaded")
			status, body = put(t, h, "second", accessToken, "again")
			expect(t, status, body, http.StatusConflict, "Cannot override an existing record")
			if got := conditionalPuts.Load(); got != 1 {
				t.Fatalf("%d conditional PUTs, want 1", got)
			}
		})
	}
}

func TestUnrelatedInvalidRequestIsNotTreatedAsUnsupportedCondition(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	var conditionalPuts atomic.Int32
	s3.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPut {
			return false
		}
		if r.Header.Get("If-None-Match") == "*" {
			conditionalPuts.Add(1)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		writeS3Error(w, http.StatusBadRequest, "InvalidRequest", "Invalid checksum")
		return true
	}
	h := newHandler(t, s3.URL, options{failOpen: new(false)})
	for range 2 {
		status, body := put(t, h, "abc", accessToken, "data")
		expect(t, status, body, http.StatusServiceUnavailable, "Cache storage unavailable")
	}
	if got := conditionalPuts.Load(); got != 2 {
		t.Fatalf("%d of 2 PUTs were conditional; an unrelated error disabled If-None-Match", got)
	}
}

func TestUploadWithoutContentLengthHeaderIsRejectedButExplicitZeroIsAllowed(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	h := newHandler(t, s3.URL, options{})
	status, body := do(t, h, call{method: http.MethodPut, path: "/v1/cache/absent", token: accessToken})
	expect(t, status, body, http.StatusLengthRequired, "Content-Length header is required")
	if _, stored := s3.object("absent"); stored {
		t.Fatal("upload without Content-Length reached storage")
	}
	status, body = put(t, h, "empty", accessToken, "")
	expect(t, status, body, http.StatusOK, "Successfully uploaded")
}

func TestTokenWhitespaceMatchesECMAScript(t *testing.T) {
	t.Parallel()
	base := server.Config{
		AccessToken:     "trusted",
		Bucket:          "b",
		Region:          "us-east-1",
		AccessKeyID:     "k",
		SecretAccessKey: "s",
	}
	accepted := base
	accepted.AccessToken = "a\u0085b" // not whitespace for JavaScript's \s
	if _, err := server.New(context.Background(), &accepted); err != nil {
		t.Fatalf("token with U+0085 rejected: %v", err)
	}
	rejected := base
	rejected.AccessToken = "a\ufeffb" // whitespace for JavaScript's \s
	if _, err := server.New(context.Background(), &rejected); !errors.Is(err, server.ErrAccessToken) {
		t.Fatalf("token with U+FEFF: got %v, want %v", err, server.ErrAccessToken)
	}
}

func TestStorageReceivesUploadBytesBeforeClientFinishes(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	prefixArrived := make(chan struct{})
	var once atomic.Bool
	s3.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPut {
			return false
		}
		var got bytes.Buffer
		buf := make([]byte, 64)
		for {
			n, err := r.Body.Read(buf)
			got.Write(buf[:n])
			if n > 0 && once.CompareAndSwap(false, true) {
				close(prefixArrived)
			}
			if err != nil {
				break
			}
		}
		if got.String() != "firstlast" {
			writeS3Error(w, http.StatusBadRequest, "IncompleteBody", got.String())
			return true
		}
		w.WriteHeader(http.StatusOK)
		return true
	}
	h := newHandler(t, s3.URL, options{})

	chunks := []string{"first", "last"}
	body := readerFunc(func(p []byte) (int, error) {
		if len(chunks) == 0 {
			return 0, io.EOF
		}
		if chunks[0] == "last" {
			select {
			case <-prefixArrived:
			case <-time.After(5 * time.Second):
				return 0, errors.New("storage received nothing before client EOF")
			}
		}
		n := copy(p, chunks[0])
		chunks = chunks[1:]
		return n, nil
	})
	status, resp := do(
		t,
		h,
		call{method: http.MethodPut, path: "/v1/cache/abc", token: accessToken, body: body, length: 9},
	)
	expect(t, status, resp, http.StatusOK, "Successfully uploaded")
}

func TestClientBodyErrorAbortsUploadWithoutStoring(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	var logs bytes.Buffer
	h := newHandler(t, s3.URL, options{logs: &logs})
	body := readerFunc(func([]byte) (int, error) { return 0, errClientGone })
	status, resp := do(
		t,
		h,
		call{method: http.MethodPut, path: "/v1/cache/abc", token: accessToken, body: body, length: 15},
	)
	expect(t, status, resp, http.StatusInternalServerError, "Internal server error")
	if _, stored := s3.object("abc"); stored {
		t.Fatal("aborted upload was stored")
	}
	if strings.Contains(logs.String(), "Storage unavailable") {
		t.Fatalf("client abort reported as storage failure:\n%s", logs.String())
	}
}

func TestStorageRejectionStopsReadingTheClientBody(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	s3.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPut {
			return false
		}
		writeS3Error(w, http.StatusForbidden, "AccessDenied", "denied")
		return true
	}
	h := newHandler(t, s3.URL, options{})
	var served atomic.Int64
	body := readerFunc(func(p []byte) (int, error) {
		served.Add(int64(len(p)))
		return len(p), nil
	})
	const declared = 1 << 30
	status, resp := do(
		t,
		h,
		call{method: http.MethodPut, path: "/v1/cache/abc", token: accessToken, body: body, length: declared},
	)
	expect(t, status, resp, http.StatusForbidden, "Cache storage unavailable; artifact not stored")
	if served.Load() >= declared {
		t.Fatal("server read the whole 1 GiB body after storage rejected it")
	}
}

func TestNewRejectsInvalidCredentialsConfiguration(t *testing.T) {
	t.Parallel()
	base := server.Config{
		AccessToken:     "trusted",
		Bucket:          "b",
		Region:          "us-east-1",
		AccessKeyID:     "k",
		SecretAccessKey: "s",
	}
	cases := map[string]struct {
		mutate func(*server.Config)
		want   error
	}{
		"empty access token":      {func(c *server.Config) { c.AccessToken = "" }, server.ErrAccessToken},
		"whitespace access token": {func(c *server.Config) { c.AccessToken = "a b" }, server.ErrAccessToken},
		"whitespace read token":   {func(c *server.Config) { c.ReadOnlyToken = "r o" }, server.ErrReadOnlyToken},
		"same tokens":             {func(c *server.Config) { c.ReadOnlyToken = "trusted" }, server.ErrSameTokens},
		"only key id":             {func(c *server.Config) { c.SecretAccessKey = "" }, server.ErrAWSCredentials},
		"only secret":             {func(c *server.Config) { c.AccessKeyID = "" }, server.ErrAWSCredentials},
	}
	for name, tc := range cases {
		cfg := base
		tc.mutate(&cfg)
		if _, err := server.New(context.Background(), &cfg); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
}
