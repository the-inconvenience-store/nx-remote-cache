// Package server implements the Nx self-hosted remote cache HTTP API backed by S3.
package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/prometheus/client_golang/prometheus"
)

// Configuration errors returned by New.
var (
	ErrAccessToken = errors.New(
		"NX_CACHE_ACCESS_TOKEN must be a comma-separated list of nonempty tokens without whitespace")
	ErrReadOnlyToken = errors.New(
		"NX_CACHE_READ_ONLY_TOKEN must be a comma-separated list of nonempty tokens without whitespace")
	ErrSameTokens = errors.New(
		"a token must not be in both NX_CACHE_ACCESS_TOKEN and NX_CACHE_READ_ONLY_TOKEN")
	ErrDuplicateToken = errors.New(
		"a token must not be listed twice in NX_CACHE_ACCESS_TOKEN or NX_CACHE_READ_ONLY_TOKEN")
	ErrAWSCredentials  = errors.New("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY must be set together")
	ErrBucketNotConfig = errors.New("S3 bucket name must not be empty")
	ErrStallTimeout    = errors.New("NX_CACHE_STALL_TIMEOUT must be a Go duration greater than zero, such as 60s")
)

const readyTimeout = 2 * time.Second

// Config holds validated runtime settings for the cache server.
type Config struct {
	// AccessTokens grant read and write access; at least one is required.
	// Several tokens let operators rotate them without downtime.
	AccessTokens []string
	// ReadOnlyTokens grant download-only access.
	ReadOnlyTokens []string
	Bucket         string
	Region         string
	// Endpoint overrides the S3 endpoint for S3-compatible storage.
	Endpoint string
	// AddressStyle selects path-style or virtual-hosted-style bucket addressing.
	AddressStyle AddressStyle
	// AccessKeyID and SecretAccessKey select static credentials. When both are
	// empty the AWS SDK default credential chain is used.
	AccessKeyID     string
	SecretAccessKey string
	// MaxArtifactSize, when positive, is the largest upload Content-Length in
	// bytes. A larger upload gets 403 before its body is read; Nx continues on
	// 403 but stops the run on 413.
	MaxArtifactSize int64
	// FailOpen degrades storage failures into cache misses (GET 404) and
	// non-fatal "not stored" responses (PUT 403) instead of 503.
	FailOpen bool
	// StallTimeout stops an artifact transfer when the server receives no
	// bytes of it for this long. It must be positive; there is no limit on the
	// total transfer time.
	StallTimeout time.Duration
	Logger       *slog.Logger
	// Metrics receives the server's collectors. Nil leaves them unregistered.
	// New panics if they are already registered with it.
	Metrics prometheus.Registerer
}

type handler struct {
	cfg     Config
	s3      *s3.Client
	log     *slog.Logger
	metrics *metrics
	// conditionalUnsupported records that storage rejected If-None-Match, so
	// later uploads in this process are sent unconditionally.
	conditionalUnsupported atomic.Bool
}

// New validates cfg and returns the cache server's HTTP handler.
func New(ctx context.Context, cfg *Config) (http.Handler, error) {
	if err := validate(cfg); err != nil {
		return nil, err
	}
	h := &handler{cfg: *cfg, log: cfg.Logger}
	if h.log == nil {
		h.log = slog.Default()
	}
	client, err := newS3Client(ctx, cfg)
	if err != nil {
		return nil, err
	}
	h.s3 = client
	// Registered last, so a failed New leaves the registry untouched.
	h.metrics = newMetrics(cfg.Metrics)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /ready", h.ready)
	mux.HandleFunc("PUT /v1/cache/{hash}", h.cacheRoute(h.upload))
	mux.HandleFunc("GET /v1/cache/{hash}", h.cacheRoute(h.download))
	return h.logRequests(h.countRequests(collapseSlashes(mux))), nil
}

// collapseSlashes changes each run of slashes in the path to one slash before
// routing. A server URL with a trailing slash makes Nx request
// //v1/cache/<hash>; ServeMux would redirect it, and Nx would send the upload
// body a second time. Encoded slashes (%2F) are not changed.
func collapseSlashes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		escaped := r.URL.EscapedPath()
		if !strings.Contains(escaped, "//") {
			next.ServeHTTP(w, r)
			return
		}
		for strings.Contains(escaped, "//") {
			escaped = strings.ReplaceAll(escaped, "//", "/")
		}
		// Removing slashes keeps EscapedPath's valid escaping, so this cannot fail.
		path, _ := url.PathUnescape(escaped)
		collapsed := new(http.Request)
		*collapsed = *r
		collapsed.URL = new(url.URL)
		*collapsed.URL = *r.URL
		collapsed.URL.Path = path
		collapsed.URL.RawPath = escaped
		next.ServeHTTP(w, collapsed)
	})
}

// validHash admits Nx's decimal hashes with room for future formats, and keeps
// path separators and other characters out of S3 keys.
var validHash = regexp.MustCompile(`^[0-9A-Za-z_-]{1,128}$`)

// cacheRoute authenticates a cache request, rejects an invalid hash before any
// storage call, and records the hash, declared length and access for the
// request log before serve runs.
func (h *handler) cacheRoute(serve func(http.ResponseWriter, *http.Request, *cacheRequest)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req := cacheRequestFrom(r.Context())
		req.hash = r.PathValue("hash")
		req.bytes = max(r.ContentLength, 0)
		req.access = h.access(r)
		if req.access == accessNone {
			writeText(w, http.StatusUnauthorized, "Missing or invalid authentication token")
			req.outcome = outcomeUnauthorised
			return
		}
		if !validHash.MatchString(req.hash) {
			writeText(w, http.StatusBadRequest, "Invalid hash")
			req.outcome = outcomeBadRequest
			return
		}
		serve(w, r, req)
	}
}

func validate(cfg *Config) error {
	if err := validateTokens(cfg.AccessTokens, cfg.ReadOnlyTokens); err != nil {
		return err
	}
	if (cfg.AccessKeyID == "") != (cfg.SecretAccessKey == "") {
		return ErrAWSCredentials
	}
	if cfg.Bucket == "" {
		return ErrBucketNotConfig
	}
	if cfg.StallTimeout <= 0 {
		return ErrStallTimeout
	}
	return nil
}

func validateTokens(access, readOnly []string) error {
	if len(access) == 0 {
		return ErrAccessToken
	}
	writers, err := tokenSet(access, ErrAccessToken)
	if err != nil {
		return err
	}
	if _, err := tokenSet(readOnly, ErrReadOnlyToken); err != nil {
		return err
	}
	for _, token := range readOnly {
		if _, both := writers[token]; both {
			return ErrSameTokens
		}
	}
	return nil
}

// tokenSet returns malformed for any ill-formed token and rejects repeats.
func tokenSet(tokens []string, malformed error) (map[string]struct{}, error) {
	set := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if !wellFormedToken(token) {
			return nil, malformed
		}
		if _, repeated := set[token]; repeated {
			return nil, ErrDuplicateToken
		}
		set[token] = struct{}{}
	}
	return set, nil
}

func wellFormedToken(token string) bool {
	return token != "" && !strings.ContainsFunc(token, ecmaSpace)
}

// ecmaSpace matches JavaScript's \s, keeping token rules identical to the
// original server (unicode.IsSpace differs on U+0085 and U+FEFF).
func ecmaSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', '\u00a0', '\u1680', '\u2028', '\u2029',
		'\u202f', '\u205f', '\u3000', '\ufeff':
		return true
	default:
		return r >= '\u2000' && r <= '\u200a'
	}
}

type access int

const (
	accessNone access = iota
	accessRead
	accessWrite
)

func (h *handler) access(r *http.Request) access {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !wellFormedToken(token) {
		return accessNone
	}
	write := matchesAny(token, h.cfg.AccessTokens)
	read := matchesAny(token, h.cfg.ReadOnlyTokens)
	switch {
	case write:
		return accessWrite
	case read:
		return accessRead
	default:
		return accessNone
	}
}

// matchesAny compares token with every candidate, without stopping at a match,
// so response timing does not reveal which candidate matched.
func matchesAny(token string, candidates []string) bool {
	given := []byte(token)
	match := 0
	for _, candidate := range candidates {
		match |= subtle.ConstantTimeCompare(given, []byte(candidate))
	}
	return match == 1
}

func health(w http.ResponseWriter, _ *http.Request) {
	writeText(w, http.StatusOK, "OK")
}

// ready reports whether the bucket is reachable, so orchestrators can route
// around instances that lost storage access. Details stay in the logs.
func (h *handler) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()
	if _, err := h.s3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &h.cfg.Bucket}); err != nil {
		h.log.ErrorContext(r.Context(), "Readiness check failed; storage unavailable", "error", err)
		writeText(w, http.StatusServiceUnavailable, "Storage unavailable")
		return
	}
	writeText(w, http.StatusOK, "OK")
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

type statusRecorder struct {
	http.ResponseWriter

	status int
}

func (s *statusRecorder) WriteHeader(status int) {
	if s.status == 0 {
		s.status = status
	}
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(p) //nolint:wrapcheck // transparent ResponseWriter wrapper
}

func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
