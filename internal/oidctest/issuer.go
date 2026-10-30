// Package oidctest runs a fake CI OIDC issuer: it serves discovery and JWKS
// documents over HTTP and signs RS256 tokens, as GitHub Actions does.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

const (
	keyBits = 2048
	// tokenLifetime matches GitHub's example Actions token.
	tokenLifetime = 5 * time.Minute
)

// Issuer is a running fake issuer. Down makes its endpoints fail; Fetches
// counts JWKS requests.
type Issuer struct {
	URL string
	Key *rsa.PrivateKey
	KID string
	// Defaults are claims added to every token before the caller's claims.
	Defaults map[string]any
	Down     atomic.Bool
	Fetches  atomic.Int32
}

// NewIssuer starts an issuer that stops when the test ends.
func NewIssuer(tb testing.TB) *Issuer {
	tb.Helper()
	key, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		tb.Fatal(err)
	}
	issuer := &Issuer{Key: key, KID: "key-1", Defaults: map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		if issuer.Down.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer.URL, "jwks_uri": issuer.URL + "/jwks"})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		issuer.Fetches.Add(1)
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &issuer.Key.PublicKey, KeyID: issuer.KID, Algorithm: string(jose.RS256), Use: "sig"},
		}})
	})
	srv := httptest.NewServer(mux)
	tb.Cleanup(srv.Close)
	issuer.URL = srv.URL
	return issuer
}

// Sign returns a token from this issuer, valid for five minutes, with
// Defaults and then claims applied.
func (i *Issuer) Sign(tb testing.TB, claims map[string]any) string {
	tb.Helper()
	return i.SignWith(tb, i.Key, i.KID, claims)
}

// SignWith signs with any key and key ID, to forge tokens.
func (i *Issuer) SignWith(tb testing.TB, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	tb.Helper()
	now := time.Now()
	all := map[string]any{
		"iss": i.URL, "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(tokenLifetime).Unix(),
	}
	maps.Copy(all, i.Defaults)
	maps.Copy(all, claims)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid))
	if err != nil {
		tb.Fatal(err)
	}
	payload, err := json.Marshal(all)
	if err != nil {
		tb.Fatal(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		tb.Fatal(err)
	}
	compact, err := jws.CompactSerialize()
	if err != nil {
		tb.Fatal(err)
	}
	return compact
}
