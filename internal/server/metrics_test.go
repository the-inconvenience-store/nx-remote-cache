package server_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/the-inconvenience-store/nx-remote-cache/internal/server"
)

func TestMetricsCountCacheRequestsByMethodAndOutcome(t *testing.T) {
	t.Parallel()
	registry := prometheus.NewRegistry()
	h := newHandler(t, newFakeS3(t).URL, options{configure: func(c *server.Config) { c.Metrics = registry }})

	put(t, h, "abc", accessToken, "artifact bytes")
	get(t, h, "abc", accessToken)
	get(t, h, "missing", accessToken)
	do(t, h, call{method: http.MethodGet, path: "/health"})

	want := `
# HELP nx_cache_requests_total Cache requests by HTTP method and outcome.
# TYPE nx_cache_requests_total counter
nx_cache_requests_total{method="GET",outcome="hit"} 1
nx_cache_requests_total{method="GET",outcome="miss"} 1
nx_cache_requests_total{method="PUT",outcome="stored"} 1
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want), "nx_cache_requests_total"); err != nil {
		t.Fatal(err)
	}
}

func TestMainPortDoesNotServeMetrics(t *testing.T) {
	t.Parallel()
	h := newHandler(t, newFakeS3(t).URL, options{
		configure: func(c *server.Config) { c.Metrics = prometheus.NewRegistry() },
	})
	status, body := do(t, h, call{method: http.MethodGet, path: "/metrics"})
	expect(t, status, body, http.StatusNotFound, "404 page not found\n")
}
