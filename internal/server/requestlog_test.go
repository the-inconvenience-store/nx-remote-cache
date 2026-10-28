package server_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// expectRequestLog checks fields of the last request log line; an empty
// value asserts the field is absent.
func expectRequestLog(t *testing.T, logs *bytes.Buffer, want map[string]string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	for _, line := range slices.Backward(lines) {
		got := map[string]string{}
		for field := range strings.FieldsSeq(line) {
			key, value, _ := strings.Cut(field, "=")
			got[key] = value
		}
		if got["msg"] != "request" {
			continue
		}
		for key, value := range want {
			if got[key] != value {
				t.Fatalf("request log %s=%q, want %q:\n%s", key, got[key], value, line)
			}
		}
		return
	}
	t.Fatalf("no request log line:\n%s", logs.String())
}

func TestRequestLogReportsCacheOutcomes(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	h := newHandler(t, newFakeS3(t).URL, options{logs: &logs})

	put(t, h, "abc", accessToken, "artifact bytes")
	expectRequestLog(t, &logs, map[string]string{
		"method": "PUT", "status": "200", "hash": "abc", "bytes": "14", "access": "rw", "outcome": "stored",
	})
	get(t, h, "abc", accessToken)
	expectRequestLog(t, &logs, map[string]string{
		"method": "GET", "status": "200", "hash": "abc", "bytes": "14", "access": "rw", "outcome": "hit",
	})
	put(t, h, "abc", accessToken, "again")
	expectRequestLog(t, &logs, map[string]string{
		"method": "PUT", "status": "409", "hash": "abc", "bytes": "5", "access": "rw", "outcome": "conflict",
	})
	get(t, h, "missing", accessToken)
	expectRequestLog(t, &logs, map[string]string{
		"method": "GET", "status": "404", "hash": "missing", "bytes": "0", "access": "rw", "outcome": "miss",
	})
	do(t, h, call{method: http.MethodPut, path: "/v1/cache/abc", token: accessToken})
	expectRequestLog(t, &logs, map[string]string{"method": "PUT", "status": "411", "outcome": "bad-request"})
	do(t, h, call{method: http.MethodGet, path: "/health"})
	expectRequestLog(t, &logs, map[string]string{
		"path": "/health", "status": "200", "hash": "", "bytes": "", "access": "", "outcome": "",
	})
}

func TestRequestLogShowsAccessWithoutTokens(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	h := newHandler(t, newFakeS3(t).URL, options{readOnly: true, logs: &logs})

	put(t, h, "abc", readOnlyToken, "data")
	expectRequestLog(t, &logs, map[string]string{
		"method": "PUT", "status": "403", "hash": "abc", "bytes": "4", "access": "ro", "outcome": "read-only",
	})
	get(t, h, "abc", readOnlyToken)
	expectRequestLog(t, &logs, map[string]string{"method": "GET", "access": "ro", "outcome": "miss"})
	const intruder = "intruder-secret"
	get(t, h, "abc", intruder)
	expectRequestLog(t, &logs, map[string]string{
		"method": "GET", "status": "401", "hash": "abc", "access": "", "outcome": "unauthorised",
	})
	put(t, h, "abc", intruder, "data")
	expectRequestLog(t, &logs, map[string]string{
		"method": "PUT", "status": "401", "hash": "abc", "bytes": "4", "access": "", "outcome": "unauthorised",
	})
	put(t, h, "abc", accessToken, "data")
	expectRequestLog(t, &logs, map[string]string{"method": "PUT", "access": "rw", "outcome": "stored"})

	for _, token := range []string{accessToken, readOnlyToken, intruder} {
		if strings.Contains(logs.String(), token) {
			t.Fatalf("log contains token %q:\n%s", token, logs.String())
		}
	}
}

func TestRequestLogSeparatesDegradedResponsesFromErrors(t *testing.T) {
	t.Parallel()
	endpoint := failingS3(t, http.StatusForbidden, "AccessDenied")
	var logs bytes.Buffer
	h := newHandler(t, endpoint, options{logs: &logs})
	get(t, h, "abc", accessToken)
	expectRequestLog(t, &logs, map[string]string{"method": "GET", "status": "404", "outcome": "degraded"})
	put(t, h, "abc", accessToken, "data")
	expectRequestLog(t, &logs, map[string]string{"method": "PUT", "status": "403", "outcome": "degraded"})

	h = newHandler(t, endpoint, options{failOpen: new(false), logs: &logs})
	get(t, h, "abc", accessToken)
	expectRequestLog(t, &logs, map[string]string{"method": "GET", "status": "503", "outcome": "error"})
	put(t, h, "abc", accessToken, "data")
	expectRequestLog(t, &logs, map[string]string{"method": "PUT", "status": "503", "outcome": "error"})
}

func TestRequestLogReportsAbortedTransfersAsErrors(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	s3.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet {
			return false
		}
		// Storage fails after the server has started streaming the artifact.
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "partial")
		return true
	}
	var logs bytes.Buffer
	h := newHandler(t, s3.URL, options{logs: &logs})

	body := readerFunc(func([]byte) (int, error) { return 0, errClientGone })
	do(t, h, call{method: http.MethodPut, path: "/v1/cache/abc", token: accessToken, body: body, length: 15})
	expectRequestLog(t, &logs, map[string]string{"method": "PUT", "status": "500", "outcome": "error"})

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/cache/abc", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := srv.Client().Do(req)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("truncated download completed without error")
	}
	srv.Close() // waits for the handler, so its log line is complete
	expectRequestLog(t, &logs, map[string]string{
		"method": "GET", "status": "200", "bytes": "7", "outcome": "error",
	})
}
