// Package server implements the Nx self-hosted remote cache HTTP API backed by S3.
package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
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
	// Endpoint overrides the S3 endpoint; requests always use path-style URLs.
	Endpoint string
	// AccessKeyID and SecretAccessKey select static credentials. When both are
	// empty the AWS SDK default credential chain is used.
	AccessKeyID     string
	SecretAccessKey string
	// FailOpen degrades storage failures into cache misses (GET 404) and
	// non-fatal "not stored" responses (PUT 403) instead of 503.
	FailOpen bool
	Logger   *slog.Logger
}

type handler struct {
	cfg Config
	s3  *s3.Client
	log *slog.Logger
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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /ready", h.ready)
	mux.HandleFunc("PUT /v1/cache/{hash}", h.upload)
	mux.HandleFunc("GET /v1/cache/{hash}", h.download)
	return h.logRequests(mux), nil
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

func (h *handler) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		h.log.InfoContext(r.Context(), "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start))
	})
}
