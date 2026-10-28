package server

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// metrics holds the server's Prometheus collectors.
type metrics struct {
	requests *prometheus.CounterVec
	stalls   *prometheus.CounterVec
}

// newMetrics creates the collectors and registers them with reg. A nil reg
// leaves them unregistered, so nothing exposes them.
func newMetrics(reg prometheus.Registerer) *metrics {
	factory := promauto.With(reg)
	return &metrics{
		requests: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "nx_cache_requests_total",
			Help: "Cache requests by HTTP method and outcome.",
		}, []string{"method", "outcome"}),
		stalls: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "nx_cache_stalled_transfers_total",
			Help: "Transfers stopped because the server received no bytes for the stall timeout, " +
				"by HTTP method and the side it was waiting on.",
		}, []string{"method", "side"}),
	}
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
