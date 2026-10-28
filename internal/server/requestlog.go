package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// outcome is what happened to a cache request. Each cache request has exactly
// one, so an operator can tell apart responses that share a status code.
type outcome string

const (
	outcomeHit      outcome = "hit"
	outcomeMiss     outcome = "miss"
	outcomeStored   outcome = "stored"
	outcomeConflict outcome = "conflict"
	outcomeReadOnly outcome = "read-only"
	// outcomeDegraded is a storage failure answered as a miss or "not stored".
	outcomeDegraded     outcome = "degraded"
	outcomeUnauthorised outcome = "unauthorised"
	outcomeBadRequest   outcome = "bad-request"
	outcomeTooLarge     outcome = "too-large"
	outcomeError        outcome = "error"
)

// cacheRequest is what a cache route learned about its request; the request
// log line reports it.
type cacheRequest struct {
	hash   string
	access access
	// bytes is the artifact size: the declared Content-Length of an upload, or
	// the bytes of a download sent to the client.
	bytes      int64
	outcome    outcome
	writer     string
	uploadedAt string
}

type cacheRequestKey struct{}

// cacheRequestFrom returns the record logRequests attached to ctx.
func cacheRequestFrom(ctx context.Context) *cacheRequest {
	req, ok := ctx.Value(cacheRequestKey{}).(*cacheRequest)
	if !ok {
		panic("server: cache route served without logRequests")
	}
	return req
}

func (a access) String() string {
	switch a {
	case accessRead:
		return "ro"
	case accessWrite:
		return "rw"
	case accessNone:
	}
	return ""
}

func (h *handler) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		req := &cacheRequest{}
		// Deferred so a download aborted mid-stream is still logged.
		defer h.logRequest(r, rec, req, start)
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), cacheRequestKey{}, req)))
	})
}

// requestLogAttrs is the most attributes a request log line has.
const requestLogAttrs = 9

// logRequest writes the request log line. Cache requests add their fields;
// tokens are never logged.
func (h *handler) logRequest(r *http.Request, rec *statusRecorder, req *cacheRequest, start time.Time) {
	attrs := make([]slog.Attr, 0, requestLogAttrs)
	attrs = append(attrs,
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", rec.status),
		slog.Duration("duration", time.Since(start)))
	if req.outcome != "" {
		attrs = append(attrs, slog.String("hash", req.hash), slog.Int64("bytes", req.bytes))
		if req.access != accessNone {
			attrs = append(attrs, slog.String("access", req.access.String()))
		}
		if req.writer != "" {
			attrs = append(attrs, slog.String("writer", req.writer))
		}
		attrs = append(attrs, slog.String("outcome", string(req.outcome)))
	}
	h.log.LogAttrs(r.Context(), slog.LevelInfo, "request", attrs...)
}
