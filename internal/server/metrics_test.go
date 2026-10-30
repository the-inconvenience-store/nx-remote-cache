package server_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/the-inconvenience-store/nx-remote-cache/internal/server"
)

func withMetrics(registry *prometheus.Registry) func(*server.Config) {
	return func(c *server.Config) { c.Metrics = registry }
}

const bytesHelp = `
# HELP nx_cache_received_bytes_total Artifact bytes read from upload request bodies.
# TYPE nx_cache_received_bytes_total counter
nx_cache_received_bytes_total %d
# HELP nx_cache_sent_bytes_total Artifact bytes written to download responses.
# TYPE nx_cache_sent_bytes_total counter
nx_cache_sent_bytes_total %d
`

func expectBytes(t *testing.T, registry *prometheus.Registry, received, sent int) {
	t.Helper()
	want := strings.NewReader(fmt.Sprintf(bytesHelp, received, sent))
	if err := testutil.GatherAndCompare(registry, want,
		"nx_cache_received_bytes_total", "nx_cache_sent_bytes_total"); err != nil {
		t.Fatal(err)
	}
}

func TestMetricsCountCacheRequestsByMethodAndOutcome(t *testing.T) {
	t.Parallel()
	registry := prometheus.NewRegistry()
	h := newHandler(t, newFakeS3(t).URL, options{configure: withMetrics(registry)})

	put(t, h, "abc", accessToken, "artifact bytes")
	get(t, h, "abc", accessToken)
	get(t, h, "missing", accessToken)
	do(t, h, call{method: http.MethodGet, path: "/health"})

	want := `
# HELP nx_cache_requests_total Cache requests by HTTP method, outcome and tier.
# TYPE nx_cache_requests_total counter
nx_cache_requests_total{method="GET",outcome="hit",tier="static"} 1
nx_cache_requests_total{method="GET",outcome="miss",tier="static"} 1
nx_cache_requests_total{method="PUT",outcome="stored",tier="static"} 1
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want), "nx_cache_requests_total"); err != nil {
		t.Fatal(err)
	}
}

func TestMainPortDoesNotServeMetrics(t *testing.T) {
	t.Parallel()
	h := newHandler(t, newFakeS3(t).URL, options{configure: withMetrics(prometheus.NewRegistry())})
	status, body := do(t, h, call{method: http.MethodGet, path: "/metrics"})
	expect(t, status, body, http.StatusNotFound, "404 page not found\n")
}

// Not parallel: t.Setenv makes the AWS configuration fail to load.
func TestMetricsRegistryUntouchedWhenNewFails(t *testing.T) {
	registry := prometheus.NewRegistry()
	cfg := server.Config{
		AccessTokens: []string{accessToken}, Bucket: testBucket, Region: "us-east-1",
		AccessKeyID: "test", SecretAccessKey: "test", Metrics: registry, StallTimeout: time.Minute,
	}
	t.Setenv("AWS_PROFILE", "nx-cache-missing-profile")
	if _, err := server.New(context.Background(), &cfg); err == nil {
		t.Fatal("New succeeded with a missing AWS profile")
	}
	t.Setenv("AWS_PROFILE", "")
	if _, err := server.New(context.Background(), &cfg); err != nil {
		t.Fatalf("New after a failed New with the same registry: %v", err)
	}
}

func TestMetricsCountTransferredBytes(t *testing.T) {
	t.Parallel()
	registry := prometheus.NewRegistry()
	h := newHandler(t, newFakeS3(t).URL, options{configure: withMetrics(registry)})

	put(t, h, "abc", accessToken, "artifact bytes")
	get(t, h, "abc", accessToken)

	expectBytes(t, registry, 14, 14)
}

func TestMetricsCountBytesTransferredNotDeclared(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	s3.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet {
			return false
		}
		// Storage declares 100 bytes and fails after sending 7.
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "partial")
		return true
	}
	registry := prometheus.NewRegistry()
	h := newHandler(t, s3.URL, options{configure: withMetrics(registry)})

	// The client declares 15 bytes and disconnects after sending 7.
	body := io.MultiReader(strings.NewReader("partial"),
		readerFunc(func([]byte) (int, error) { return 0, errClientGone }))
	do(t, h, call{method: http.MethodPut, path: "/v1/cache/abc", token: accessToken, body: body, length: 15})

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/cache/abc", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if resp, err := srv.Client().Do(req); err == nil {
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	srv.Close() // waits for the handler, so its counts are complete

	expectBytes(t, registry, 7, 7)
}

func TestMetricsTimeS3CallsByOperation(t *testing.T) {
	t.Parallel()
	registry := prometheus.NewRegistry()
	h := newHandler(t, newFakeS3(t).URL, options{configure: withMetrics(registry)})

	put(t, h, "abc", accessToken, "artifact bytes")
	get(t, h, "abc", accessToken)
	do(t, h, call{method: http.MethodGet, path: "/ready"})

	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]uint64{}
	for _, family := range families {
		if family.GetName() != "nx_cache_s3_request_duration_seconds" {
			continue
		}
		for _, m := range family.GetMetric() {
			for _, label := range m.GetLabel() {
				if label.GetName() == "operation" {
					calls[label.GetValue()] = m.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	want := map[string]uint64{"HeadObject": 1, "PutObject": 1, "GetObject": 1, "HeadBucket": 1}
	if !maps.Equal(calls, want) {
		t.Fatalf("S3 call counts by operation: got %v, want %v", calls, want)
	}
}

const unsupportedConditionWarning = `level=WARN msg="Storage does not support conditional writes`

// rejectConditionalPuts makes s3 answer every conditional PUT as storage that
// does not implement If-None-Match, after hold returns.
func rejectConditionalPuts(s3 *fakeS3, hold func()) {
	s3.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPut || r.Header.Get("If-None-Match") == "" {
			return false
		}
		_, _ = io.Copy(io.Discard, r.Body)
		hold()
		writeS3Error(w, http.StatusNotImplemented, "NotImplemented",
			"A header you provided implies functionality that is not implemented")
		return true
	}
}

func TestUnsupportedConditionWarnsOnceAndCountsUnconditionalUploads(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	rejectConditionalPuts(s3, func() {})
	registry := prometheus.NewRegistry()
	var logs bytes.Buffer
	h := newHandler(t, s3.URL, options{logs: &logs, configure: withMetrics(registry)})

	put(t, h, "first", accessToken, "one")   // conditional, rejected: the switch
	put(t, h, "second", accessToken, "two")  // unconditional
	put(t, h, "third", accessToken, "three") // unconditional
	put(t, h, "third", accessToken, "again") // 409 from HeadObject, never sent

	if got := strings.Count(logs.String(), unsupportedConditionWarning); got != 1 {
		t.Fatalf("%d unsupported-condition warnings, want 1:\n%s", got, logs.String())
	}
	want := `
# HELP nx_cache_unconditional_uploads_total Uploads sent to storage without If-None-Match because storage does not support conditional writes.
# TYPE nx_cache_unconditional_uploads_total counter
nx_cache_unconditional_uploads_total 2
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want),
		"nx_cache_unconditional_uploads_total"); err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedConditionWarnsOnceForConcurrentUploads(t *testing.T) {
	t.Parallel()
	const uploads = 4
	s3 := newFakeS3(t)
	var arrived atomic.Int32
	allArrived := make(chan struct{})
	// Every upload is sent with the condition before any is rejected.
	rejectConditionalPuts(s3, func() {
		if arrived.Add(1) == uploads {
			close(allArrived)
		}
		select {
		case <-allArrived:
		case <-time.After(5 * time.Second):
		}
	})
	var logs bytes.Buffer
	h := newHandler(t, s3.URL, options{logs: &logs})

	var wg sync.WaitGroup
	for i := range uploads {
		wg.Go(func() { put(t, h, "hash"+strconv.Itoa(i), accessToken, "data") })
	}
	wg.Wait()

	if got := arrived.Load(); got != uploads {
		t.Fatalf("%d conditional PUTs, want %d", got, uploads)
	}
	if got := strings.Count(logs.String(), unsupportedConditionWarning); got != 1 {
		t.Fatalf("%d unsupported-condition warnings, want 1:\n%s", got, logs.String())
	}
}

func TestMetricsCountRequestsByTierAndExchangesByResult(t *testing.T) {
	t.Parallel()
	registry := prometheus.NewRegistry()
	issuer := newFakeIssuer(t)
	h := newHandler(t, newFakeS3(t).URL, options{configure: func(c *server.Config) {
		withExchange(issuer)(c)
		c.Metrics = registry
	}})

	trusted := issue(t, h, issuer, push("main", true))
	put(t, h, "t", trusted, "trusted artifact")
	pr := issue(t, h, issuer, pullRequest(1))
	get(t, h, "t", pr)
	put(t, h, "p", pr, "pr artifact")
	get(t, h, "p", pr)
	get(t, h, "missing", pr)
	get(t, h, "t", "wrong")
	exchange(t, h, issuer.sign(t, map[string]any{"event_name": "pull_request_target", "ref": "refs/heads/main"}))
	exchange(t, h, "not-a-jwt")

	want := `
# HELP nx_cache_requests_total Cache requests by HTTP method, outcome and tier.
# TYPE nx_cache_requests_total counter
nx_cache_requests_total{method="GET",outcome="hit",tier="scoped"} 1
nx_cache_requests_total{method="GET",outcome="hit",tier="trusted"} 1
nx_cache_requests_total{method="GET",outcome="miss",tier="scoped"} 1
nx_cache_requests_total{method="GET",outcome="unauthorised",tier=""} 1
nx_cache_requests_total{method="PUT",outcome="stored",tier="scoped"} 1
nx_cache_requests_total{method="PUT",outcome="stored",tier="trusted"} 1
# HELP nx_cache_token_exchanges_total Token exchanges by provider and result (issued, refused, invalid, unavailable). The provider is empty until a CI token's issuer is trusted.
# TYPE nx_cache_token_exchanges_total counter
nx_cache_token_exchanges_total{provider="",result="invalid"} 1
nx_cache_token_exchanges_total{provider="github",result="issued"} 2
nx_cache_token_exchanges_total{provider="github",result="refused"} 1
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want),
		"nx_cache_requests_total", "nx_cache_token_exchanges_total"); err != nil {
		t.Fatal(err)
	}
}
