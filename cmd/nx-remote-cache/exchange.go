package main

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/the-inconvenience-store/nx-remote-cache/internal/server"
)

// defaultIssuedTokenTTL is the GitHub-hosted runner job limit.
const defaultIssuedTokenTTL = 6 * time.Hour

var errExchangeOff = errors.New("NX_CACHE_OIDC_PROVIDERS must be set to use the token exchange variables")

// loadTokenExchange returns nil when NX_CACHE_OIDC_PROVIDERS is unset or
// empty, and rejects any other token exchange variable in that case.
// server.New checks the values it can check without parsing.
func loadTokenExchange(lookup lookupFunc) (*server.TokenExchange, error) {
	if providers, _ := lookup("NX_CACHE_OIDC_PROVIDERS"); providers == "" {
		return nil, rejectExchangeVariables(lookup)
	} else if !onlyGitHub(splitTokens(providers)) {
		return nil, server.ErrOIDCProvider
	}
	keys, err := parseIssuedTokenKeys(lookup)
	if err != nil {
		return nil, err
	}
	ttl := defaultIssuedTokenTTL
	if value, ok := lookup("NX_CACHE_ISSUED_TOKEN_TTL"); ok {
		if ttl, err = time.ParseDuration(value); err != nil {
			return nil, server.ErrIssuedTokenTTL
		}
	}
	readOnly, err := parseRepositoryTokens(lookup)
	if err != nil {
		return nil, err
	}
	audience, _ := lookup("NX_CACHE_OIDC_AUDIENCE")
	trusted := []string{"main"}
	if value, ok := lookup("NX_CACHE_GITHUB_TRUSTED_BRANCHES"); ok {
		trusted = splitTokens(value)
	}
	return &server.TokenExchange{
		Audience: audience,
		Keys:     keys,
		TTL:      ttl,
		GitHub: &server.GitHub{
			Issuer:          valueOr(lookup, "NX_CACHE_GITHUB_ISSUER", server.DefaultGitHubIssuer),
			OwnerIDs:        optionalList(lookup, "NX_CACHE_GITHUB_OWNER_IDS"),
			RepositoryIDs:   optionalList(lookup, "NX_CACHE_GITHUB_REPOSITORY_IDS"),
			TrustedBranches: trusted,
			ReadOnlyTokens:  readOnly,
		},
	}, nil
}

// rejectExchangeVariables fails when a setting that only applies with token
// exchange on is set while it is off.
func rejectExchangeVariables(lookup lookupFunc) error {
	for _, key := range []string{
		"NX_CACHE_OIDC_AUDIENCE", "NX_CACHE_ISSUED_TOKEN_KEYS", "NX_CACHE_ISSUED_TOKEN_TTL",
		"NX_CACHE_GITHUB_ISSUER", "NX_CACHE_GITHUB_OWNER_IDS", "NX_CACHE_GITHUB_REPOSITORY_IDS",
		"NX_CACHE_GITHUB_TRUSTED_BRANCHES", "NX_CACHE_GITHUB_READ_ONLY_TOKENS",
	} {
		if _, set := lookup(key); set {
			return errExchangeOff
		}
	}
	return nil
}

func onlyGitHub(providers []string) bool {
	for _, p := range providers {
		if p != "github" {
			return false
		}
	}
	return true
}

// optionalList splits a comma-separated list; unset or empty is no list.
func optionalList(lookup lookupFunc, key string) []string {
	if value, _ := lookup(key); value != "" {
		return splitTokens(value)
	}
	return nil
}

func parseIssuedTokenKeys(lookup lookupFunc) ([][]byte, error) {
	encoded := optionalList(lookup, "NX_CACHE_ISSUED_TOKEN_KEYS")
	keys := make([][]byte, 0, len(encoded))
	for _, value := range encoded {
		key, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, server.ErrIssuedTokenKeys
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// parseRepositoryTokens reads <repository_id>:<token> pairs; the token is
// everything after the first colon.
func parseRepositoryTokens(lookup lookupFunc) ([]server.RepositoryToken, error) {
	pairs := optionalList(lookup, "NX_CACHE_GITHUB_READ_ONLY_TOKENS")
	if pairs == nil {
		return nil, nil
	}
	tokens := make([]server.RepositoryToken, 0, len(pairs))
	for _, pair := range pairs {
		id, token, ok := strings.Cut(pair, ":")
		if !ok {
			return nil, server.ErrRepositoryToken
		}
		tokens = append(tokens, server.RepositoryToken{RepositoryID: id, Token: token})
	}
	return tokens, nil
}
