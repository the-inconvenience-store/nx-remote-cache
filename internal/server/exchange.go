package server

import (
	"context"
	"crypto"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
)

// TokenExchange turns on repository tiers and POST /v1/token, which trades a
// CI OIDC token for an issued token bound to one repository and one tier.
type TokenExchange struct {
	// Audience is the "aud" CI tokens must be issued for.
	Audience string
	// Keys sign and verify issued tokens; the first signs. Each must be at
	// least MinIssuedTokenKeyBytes long and identical on every replica.
	Keys [][]byte
	// TTL is how long an issued token stays valid: more than zero and at most
	// MaxIssuedTokenTTL.
	TTL time.Duration
	// GitHub configures the GitHub Actions provider. It is the only provider,
	// so it is required.
	GitHub *GitHub
}

const (
	// MinIssuedTokenKeyBytes is the shortest accepted issued-token key.
	MinIssuedTokenKeyBytes = 32
	// MaxIssuedTokenTTL is the self-hosted GitHub runner job limit (5 days).
	MaxIssuedTokenTTL = 120 * time.Hour
	// issuerTimeout bounds each discovery or JWKS request to an issuer.
	issuerTimeout = 5 * time.Second
	// keyRefreshInterval is the least time between issuer key fetches, so CI
	// tokens with unknown key IDs cannot make the server flood the issuer.
	keyRefreshInterval = 30 * time.Second
	// maxIssuerResponse bounds discovery and JWKS documents.
	maxIssuerResponse = 1 << 20
)

// Messages an issued token is refused with. Nx shows 401 text to the user.
const (
	msgInvalidToken  = "Missing or invalid authentication token"
	msgTokenExpired  = "Cache token expired; exchange a new CI token for it"
	msgNotAdmitted   = "This repository is not allowed to use this cache"
	msgInvalidCI     = "Missing or invalid CI OIDC token"
	msgIssuerOffline = "CI token issuer unavailable; try again later"
)

// exchangeResult labels nx_cache_token_exchanges_total.
type exchangeResult string

const (
	exchangeIssued      exchangeResult = "issued"
	exchangeRefused     exchangeResult = "refused"
	exchangeInvalid     exchangeResult = "invalid"
	exchangeUnavailable exchangeResult = "unavailable"
)

// provider maps one CI system's verified OIDC claims to a grant. Signature
// checks, issued tokens and storage are shared, so a new CI system adds only
// a provider.
type provider interface {
	name() string
	issuer() string
	// admits reports whether a repository may still use the cache; it is
	// checked at exchange and on every request with an issued token.
	admits(ownerID, repositoryID string) bool
	// decide maps verified claims to a grant or a refusal.
	decide(claims []byte) decision
}

// decision is a provider's answer to verified claims.
type decision struct {
	ownerID      string
	repositoryID string
	// namespace is empty for the trusted tier.
	namespace string
	runID     string
	actor     string
	// refusal, when not empty, refuses the exchange and is sent to the client.
	refusal string
	// warning, when not empty, is logged with the exchange.
	warning string
	// attrs describe the CI run for the exchange log line.
	attrs []any
}

// exchange is the configured token exchange.
type exchange struct {
	audience  string
	keys      [][]byte
	ttl       time.Duration
	providers []provider
	issuers   map[string]*issuerKeys
	// repoTokens are static read-only tokens bound to one repository.
	repoTokens []repositoryToken
}

type repositoryToken struct {
	repo  repository
	token string
}

func newExchange(cfg *TokenExchange) *exchange {
	client := &http.Client{Timeout: issuerTimeout}
	gh := newGitHubProvider(cfg.GitHub)
	x := &exchange{
		audience:  cfg.Audience,
		keys:      cfg.Keys,
		ttl:       cfg.TTL,
		providers: []provider{gh},
		issuers:   map[string]*issuerKeys{gh.issuer(): {issuer: gh.issuer(), client: client}},
	}
	for _, t := range cfg.GitHub.ReadOnlyTokens {
		x.repoTokens = append(x.repoTokens, repositoryToken{
			repo: repository{provider: gh.name(), id: t.RepositoryID}, token: t.Token,
		})
	}
	return x
}

func (x *exchange) provider(name string) provider {
	for _, p := range x.providers {
		if p.name() == name {
			return p
		}
	}
	return nil
}

// repositoryReader returns the repository a static repository read-only
// token reads. It compares every token, so timing does not reveal a match.
func (x *exchange) repositoryReader(token string) (repository, bool) {
	var found repository
	ok := false
	for _, t := range x.repoTokens {
		if matchesAny(token, []string{t.token}) {
			found, ok = t.repo, true
		}
	}
	return found, ok
}

// authenticate checks an issued token: signature, expiry, then whether its
// repository is still admitted. A refusal is the 401 message.
func (x *exchange) authenticate(token string, now time.Time) (credential, string) {
	claims, ok := parseToken(x.keys, token)
	if !ok {
		return credential{}, msgInvalidToken
	}
	p := x.provider(claims.Provider)
	if p == nil {
		return credential{}, msgInvalidToken
	}
	if !now.Before(time.Unix(claims.Expires, 0)) {
		return credential{}, msgTokenExpired
	}
	if !p.admits(claims.OwnerID, claims.RepositoryID) {
		return credential{}, msgNotAdmitted
	}
	cred := credential{
		write:     true,
		tier:      tierTrusted,
		repo:      repository{provider: claims.Provider, id: claims.RepositoryID},
		namespace: claims.Namespace,
		runID:     claims.RunID,
		actor:     claims.Actor,
	}
	if claims.Namespace != "" {
		cred.tier = tierScoped
	}
	return cred, ""
}

// exchangeToken serves POST /v1/token.
func (h *handler) exchangeToken(w http.ResponseWriter, r *http.Request) {
	p, claims, failure := h.exchange.verify(r)
	if failure != nil {
		h.metrics.exchanges.WithLabelValues("", string(failure.result)).Inc()
		level := slog.LevelWarn
		if failure.result == exchangeUnavailable {
			level = slog.LevelError
		}
		h.log.Log(r.Context(), level, "Token exchange failed",
			"result", string(failure.result), "reason", failure.reason)
		writeText(w, failure.status, failure.message)
		return
	}
	d := p.decide(claims)
	attrs := append([]any{"provider", p.name()}, d.attrs...)
	if d.refusal != "" {
		h.metrics.exchanges.WithLabelValues(p.name(), string(exchangeRefused)).Inc()
		h.log.WarnContext(r.Context(), "Token exchange refused", append(attrs, "reason", d.refusal)...)
		writeText(w, http.StatusForbidden, d.refusal)
		return
	}
	if d.warning != "" {
		h.log.WarnContext(r.Context(), "Token exchange warning", append(attrs, "warning", d.warning)...)
	}
	issued := tierTrusted
	if d.namespace != "" {
		issued = tierScoped
		attrs = append(attrs, "namespace", d.namespace)
	}
	token := mintToken(h.exchange.keys[0], &issuedClaims{
		Provider:     p.name(),
		OwnerID:      d.ownerID,
		RepositoryID: d.repositoryID,
		Namespace:    d.namespace,
		Expires:      time.Now().Add(h.exchange.ttl).Unix(),
		RunID:        d.runID,
		Actor:        d.actor,
	})
	h.metrics.exchanges.WithLabelValues(p.name(), string(exchangeIssued)).Inc()
	h.log.InfoContext(r.Context(), "Token issued", append(attrs, "tier", string(issued))...)
	writeText(w, http.StatusOK, token)
}

// verifyFailure is why a CI token was not verified. The provider label is
// empty for these: the token's issuer is not trusted yet.
type verifyFailure struct {
	result  exchangeResult
	status  int
	message string
	reason  string
}

func invalidCI(reason string) *verifyFailure {
	return &verifyFailure{exchangeInvalid, http.StatusUnauthorized, msgInvalidCI, reason}
}

// verify checks the request's CI token and returns its provider and claims.
// Cheap checks on the unverified token come first, so tokens for other
// issuers or audiences never cause key fetches.
func (x *exchange) verify(r *http.Request) (provider, json.RawMessage, *verifyFailure) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || raw == "" {
		return nil, nil, invalidCI("no bearer token")
	}
	jws, err := jose.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil || len(jws.Signatures) != 1 {
		return nil, nil, invalidCI("malformed or not RS256")
	}
	p := x.unverifiedProvider(jws.UnsafePayloadWithoutVerification())
	if p == nil {
		return nil, nil, invalidCI("issuer or audience not accepted")
	}
	key, err := x.issuers[p.issuer()].key(r, jws.Signatures[0].Header.KeyID)
	if errors.Is(err, errIssuerUnavailable) {
		return nil, nil, &verifyFailure{
			exchangeUnavailable,
			http.StatusServiceUnavailable,
			msgIssuerOffline,
			err.Error(),
		}
	}
	if err != nil {
		return nil, nil, invalidCI(err.Error())
	}
	verifier := oidc.NewVerifier(p.issuer(), &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{key}},
		&oidc.Config{ClientID: x.audience, SupportedSigningAlgs: []string{oidc.RS256}})
	idToken, err := verifier.Verify(r.Context(), raw)
	if err != nil {
		return nil, nil, invalidCI(err.Error())
	}
	var claims json.RawMessage
	if err := idToken.Claims(&claims); err != nil {
		return nil, nil, invalidCI(err.Error())
	}
	return p, claims, nil
}

// unverifiedProvider returns the provider whose issuer the unverified payload
// names, or nil when no provider matches or the audience is not this server.
func (x *exchange) unverifiedProvider(payload []byte) provider {
	var unverified struct {
		Issuer   string   `json:"iss"`
		Audience audience `json:"aud"`
	}
	if json.Unmarshal(payload, &unverified) != nil || !slices.Contains(unverified.Audience, x.audience) {
		return nil
	}
	for _, p := range x.providers {
		if p.issuer() == unverified.Issuer {
			return p
		}
	}
	return nil
}

// audience accepts the "aud" claim as one string or a list.
type audience []string

func (a *audience) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("aud: %w", err)
	}
	*a = many
	return nil
}

var (
	errIssuerUnavailable = errors.New("CI token issuer unavailable")
	errUnknownKey        = errors.New("CI token signed with an unknown key")
	errDiscovery         = errors.New("issuer discovery document does not match the issuer")
	errIssuerStatus      = errors.New("issuer answered with an error status")
)

// issuerKeys caches one issuer's signing keys. Keys are fetched lazily, so
// startup and readiness never depend on the issuer, and at most once per
// keyRefreshInterval.
type issuerKeys struct {
	issuer string
	client *http.Client

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	attempted time.Time
	fetchErr  error
}

// key returns the RSA key with ID kid, fetching the issuer's keys when kid is
// unknown and the last fetch is old enough. It returns errIssuerUnavailable
// when the issuer cannot be reached.
func (k *issuerKeys) key(r *http.Request, kid string) (*rsa.PublicKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if key, ok := k.keys[kid]; ok && kid != "" {
		return key, nil
	}
	if !k.attempted.IsZero() && time.Since(k.attempted) < keyRefreshInterval {
		if k.fetchErr != nil {
			return nil, k.fetchErr
		}
		return nil, errUnknownKey
	}
	k.attempted = time.Now()
	keys, err := k.fetch(r)
	if err != nil {
		k.fetchErr = fmt.Errorf("%w: %w", errIssuerUnavailable, err)
		return nil, k.fetchErr
	}
	k.keys, k.fetchErr = keys, nil
	if key, ok := k.keys[kid]; ok && kid != "" {
		return key, nil
	}
	return nil, errUnknownKey
}

func (k *issuerKeys) fetch(r *http.Request) (map[string]*rsa.PublicKey, error) {
	var discovery struct {
		Issuer  string `json:"issuer"`
		JWKSURL string `json:"jwks_uri"`
	}
	if err := k.getJSON(r, strings.TrimSuffix(k.issuer, "/")+"/.well-known/openid-configuration",
		&discovery); err != nil {
		return nil, err
	}
	if discovery.Issuer != k.issuer || discovery.JWKSURL == "" {
		return nil, fmt.Errorf("%w: names issuer %q, want %q", errDiscovery, discovery.Issuer, k.issuer)
	}
	var set jose.JSONWebKeySet
	if err := k.getJSON(r, discovery.JWKSURL, &set); err != nil {
		return nil, err
	}
	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for i := range set.Keys {
		jwk := &set.Keys[i]
		key, ok := jwk.Key.(*rsa.PublicKey)
		if ok && jwk.KeyID != "" && (jwk.Use == "" || jwk.Use == "sig") {
			keys[jwk.KeyID] = key
		}
	}
	return keys, nil
}

func (k *issuerKeys) getJSON(r *http.Request, url string, v any) error {
	// Detached from the request, so a client hanging up does not fail the
	// fetch other exchanges are waiting on.
	req, err := http.NewRequestWithContext(context.WithoutCancel(r.Context()), http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", url, err)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: get %s: status %d", errIssuerStatus, url, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxIssuerResponse)).Decode(v); err != nil {
		return fmt.Errorf("decode %s: %w", url, err)
	}
	return nil
}

// logExchangeConfig records the exchange settings operators most need to see.
func logExchangeConfig(log *slog.Logger, gh *GitHub) {
	log.Info("Token exchange enabled", "provider", "github", "issuer", gh.Issuer,
		"trusted_branches", strings.Join(gh.TrustedBranches, ","),
		"owner_ids", strings.Join(gh.OwnerIDs, ","), "repository_ids", strings.Join(gh.RepositoryIDs, ","))
}
