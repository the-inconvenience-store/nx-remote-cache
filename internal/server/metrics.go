package server

import (
	"context"
	"net/http"
	"time"

	"github.com/aws/smithy-go/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// metrics holds the server's Prometheus collectors.
type metrics struct {
	requests    *prometheus.CounterVec
	received    prometheus.Counter
	sent        prometheus.Counter
	s3Durations *prometheus.HistogramVec
	// all lists every collector above for register.
	all collectorList
}

// newMetrics creates the collectors without registering them, so the S3
// client can use them before New knows it will succeed.
func newMetrics() *metrics {
	var all collectorList
	factory := promauto.With(&all)
	m := &metrics{
		requests: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "nx_cache_requests_total",
			Help: "Cache requests by HTTP method and outcome.",
		}, []string{"method", "outcome"}),
		received: factory.NewCounter(prometheus.CounterOpts{
			Name: "nx_cache_received_bytes_total",
			Help: "Artifact bytes read from upload request bodies.",
		}),
		sent: factory.NewCounter(prometheus.CounterOpts{
			Name: "nx_cache_sent_bytes_total",
			Help: "Artifact bytes written to download responses.",
		}),
		s3Durations: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name: "nx_cache_s3_request_duration_seconds",
			Help: "S3 API call duration by operation, including SDK retries. " +
				"A GetObject call ends when the response headers arrive.",
			Buckets: prometheus.DefBuckets,
		}, []string{"operation"}),
	}
	m.all = all
	return m
}

// register registers every collector with reg; a nil reg leaves them
// unregistered, so nothing exposes them.
func (m *metrics) register(reg prometheus.Registerer) {
	if reg != nil {
		reg.MustRegister(m.all...)
	}
}

// collectorList is a prometheus.Registerer that only records collectors, so
// newMetrics can declare each one once with promauto and register them later.
type collectorList []prometheus.Collector

func (l *collectorList) Register(c prometheus.Collector) error {
	*l = append(*l, c)
	return nil
}

func (l *collectorList) MustRegister(cs ...prometheus.Collector) {
	*l = append(*l, cs...)
}

func (*collectorList) Unregister(prometheus.Collector) bool {
	return false
}

// countRequests counts each cache request by method and outcome once it ends,
// including a download aborted mid-stream. It must run inside logRequests,
// which attaches the request record.
func (h *handler) countRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := cacheRequestFrom(r.Context())
		defer func() {
			if req.outcome != "" {
				h.metrics.requests.WithLabelValues(r.Method, string(req.outcome)).Inc()
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// timeS3Calls is an S3 client API option that observes each operation's
// duration, whatever its result. It runs first in the stack, so the time
// covers signing, retries and the wait for response headers.
func (m *metrics) timeS3Calls(stack *middleware.Stack) error {
	//nolint:wrapcheck // the stack's error is returned unchanged to the SDK
	return stack.Initialize.Add(middleware.InitializeMiddlewareFunc("NxCacheS3Duration",
		func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (
			middleware.InitializeOutput, middleware.Metadata, error,
		) {
			start := time.Now()
			out, metadata, err := next.HandleInitialize(ctx, in)
			m.s3Durations.WithLabelValues(middleware.GetOperationName(ctx)).Observe(time.Since(start).Seconds())
			return out, metadata, err //nolint:wrapcheck // SDK errors pass through unchanged
		}), middleware.Before)
}
