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
	"path"
	"regexp"
	"slices"
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
	ErrOIDCAudience    = errors.New("NX_CACHE_OIDC_AUDIENCE must not be empty when token exchange is on")
	ErrIssuedTokenKeys = errors.New(
		"NX_CACHE_ISSUED_TOKEN_KEYS must list at least one base64 key of 32 bytes or more")
	ErrIssuedTokenTTL = errors.New(
		"NX_CACHE_ISSUED_TOKEN_TTL must be a Go duration greater than zero and at most 120h")
	ErrOIDCProvider = errors.New(`NX_CACHE_OIDC_PROVIDERS must be "github"`)
	ErrGitHubIssuer = errors.New("NX_CACHE_GITHUB_ISSUER must be an absolute http or https URL")
	ErrGitHubIDs    = errors.New("NX_CACHE_GITHUB_OWNER_IDS or NX_CACHE_GITHUB_REPOSITORY_IDS must " +
		"list at least one ID, and every ID must be a decimal number")
	ErrTrustedBranches = errors.New("NX_CACHE_GITHUB_TRUSTED_BRANCHES must list at least one valid " +
		`path.Match pattern, and none may be "*"`)
	ErrRepositoryToken = errors.New("NX_CACHE_GITHUB_READ_ONLY_TOKENS must be a comma-separated list of " +
		"<repository_id>:<token> pairs, with tokens without whitespace that appear in no other token list")
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
	// StallTimeout stops an artifact transfer when none of its bytes move for
	// this long. It must be positive; there is no limit on the total transfer
	// time.
	StallTimeout time.Duration
	Logger       *slog.Logger
	// Metrics receives the server's collectors. Nil leaves them unregistered.
	// New panics if they are already registered with it.
	Metrics prometheus.Registerer
	// TokenExchange turns on repository tiers and POST /v1/token. When it is
	// set, AccessTokens may be empty.
	TokenExchange *TokenExchange
}

type handler struct {
	cfg     Config
	s3      *s3.Client
	log     *slog.Logger
	metrics *metrics
	// exchange is nil when token exchange is off.
	exchange *exchange
	// conditionalUnsupported records that storage rejected If-None-Match, so
	// later uploads in this process are sent unconditionally.
	conditionalUnsupported atomic.Bool
}

// New validates cfg and returns the cache server's HTTP handler.
func New(ctx context.Context, cfg *Config) (http.Handler, error) {
	if err := validate(cfg); err != nil {
		return nil, err
	}
	h := &handler{cfg: *cfg, log: cfg.Logger, metrics: newMetrics()}
	if h.log == nil {
		h.log = slog.Default()
	}
	client, err := newS3Client(ctx, cfg, h.metrics.timeS3Calls)
	if err != nil {
		return nil, err
	}
	h.s3 = client
	// Registered last, so a failed New leaves the registry untouched.
	h.metrics.register(cfg.Metrics)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /ready", h.ready)
	mux.HandleFunc("PUT /v1/cache/{hash}", h.cacheRoute(h.upload))
	mux.HandleFunc("GET /v1/cache/{hash}", h.cacheRoute(h.download))
	if cfg.TokenExchange != nil {
		h.exchange = newExchange(cfg.TokenExchange)
		mux.HandleFunc("POST /v1/token", h.exchangeToken)
		logExchangeConfig(h.log, cfg.TokenExchange.GitHub)
	}
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
		cred, refusal := h.authenticate(r)
		if refusal != "" {
			writeText(w, http.StatusUnauthorized, refusal)
			req.outcome = outcomeUnauthorised
			return
		}
		req.cred, req.authenticated, req.tier = cred, true, cred.tier
		if !validHash.MatchString(req.hash) {
			writeText(w, http.StatusBadRequest, "Invalid hash")
			req.outcome = outcomeBadRequest
			return
		}
		serve(w, r, req)
	}
}

func validate(cfg *Config) error {
	if len(cfg.AccessTokens) == 0 && cfg.TokenExchange == nil {
		return ErrAccessToken
	}
	tokens, err := validateTokens(cfg.AccessTokens, cfg.ReadOnlyTokens)
	if err != nil {
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
	if cfg.TokenExchange != nil {
		return validateExchange(cfg.TokenExchange, tokens)
	}
	return nil
}

// validateTokens returns the set of every static token.
func validateTokens(access, readOnly []string) (map[string]struct{}, error) {
	writers, err := tokenSet(access, ErrAccessToken)
	if err != nil {
		return nil, err
	}
	readers, err := tokenSet(readOnly, ErrReadOnlyToken)
	if err != nil {
		return nil, err
	}
	for token := range readers {
		if _, both := writers[token]; both {
			return nil, ErrSameTokens
		}
		writers[token] = struct{}{}
	}
	return writers, nil
}

var decimalID = regexp.MustCompile(`^[0-9]+$`)

// validateExchange checks the token exchange settings; static holds every
// static token, which repository read-only tokens must not repeat.
func validateExchange(x *TokenExchange, static map[string]struct{}) error {
	if x.Audience == "" {
		return ErrOIDCAudience
	}
	if len(x.Keys) == 0 {
		return ErrIssuedTokenKeys
	}
	for _, key := range x.Keys {
		if len(key) < MinIssuedTokenKeyBytes {
			return ErrIssuedTokenKeys
		}
	}
	if x.TTL <= 0 || x.TTL > MaxIssuedTokenTTL {
		return ErrIssuedTokenTTL
	}
	if x.GitHub == nil {
		return ErrOIDCProvider
	}
	return validateGitHub(x.GitHub, static)
}

func validateGitHub(gh *GitHub, static map[string]struct{}) error {
	issuer, err := url.Parse(gh.Issuer)
	if err != nil || (issuer.Scheme != "https" && issuer.Scheme != "http") || issuer.Host == "" {
		return ErrGitHubIssuer
	}
	if len(gh.OwnerIDs)+len(gh.RepositoryIDs) == 0 ||
		slices.ContainsFunc(slices.Concat(gh.OwnerIDs, gh.RepositoryIDs), notDecimalID) {
		return ErrGitHubIDs
	}
	if len(gh.TrustedBranches) == 0 || slices.ContainsFunc(gh.TrustedBranches, badBranchPattern) {
		return ErrTrustedBranches
	}
	return validateRepositoryTokens(gh.ReadOnlyTokens, static)
}

// validateRepositoryTokens rejects malformed pairs and any token already in
// static, then adds each token to it.
func validateRepositoryTokens(tokens []RepositoryToken, static map[string]struct{}) error {
	for _, t := range tokens {
		_, reused := static[t.Token]
		if notDecimalID(t.RepositoryID) || !wellFormedToken(t.Token) || reused {
			return ErrRepositoryToken
		}
		static[t.Token] = struct{}{}
	}
	return nil
}

func notDecimalID(id string) bool {
	return !decimalID.MatchString(id)
}

// badBranchPattern rejects invalid patterns and a bare "*", which would trust
// every branch without a slash.
func badBranchPattern(pattern string) bool {
	_, err := path.Match(pattern, "")
	return err != nil || pattern == "" || pattern == "*"
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

// authenticate returns what the request's bearer token grants, or the 401
// message. Static tokens are compared in full before any issued-token check.
func (h *handler) authenticate(r *http.Request) (credential, string) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !wellFormedToken(token) {
		return credential{}, msgInvalidToken
	}
	write := matchesAny(token, h.cfg.AccessTokens)
	read := matchesAny(token, h.cfg.ReadOnlyTokens)
	switch {
	case write:
		return credential{write: true, tier: tierStatic}, ""
	case read:
		return credential{tier: tierStatic}, ""
	case h.exchange == nil:
		return credential{}, msgInvalidToken
	}
	if repo, ok := h.exchange.repositoryReader(token); ok {
		return credential{tier: tierTrusted, repo: repo}, ""
	}
	return h.exchange.authenticate(token, time.Now())
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
	_, _ = io.WriteString(w, body) //nolint:gosec // text/plain, never rendered as HTML
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
