package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/the-inconvenience-store/nx-remote-cache/internal/server"
)

func env(values map[string]string) lookupFunc {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

func validEnv() map[string]string {
	return map[string]string{
		"NX_CACHE_ACCESS_TOKEN": "trusted",
		"AWS_ACCESS_KEY_ID":     "key",
		"AWS_SECRET_ACCESS_KEY": "secret",
		"S3_ENDPOINT_URL":       "http://127.0.0.1:1",
	}
}

func TestLoadSettingsAppliesDefaults(t *testing.T) {
	t.Parallel()
	s, err := loadSettings(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if s.port != 3000 || s.metricsPort != 9090 || s.server.Region != "us-east-1" || s.server.Bucket != "nx-cloud" ||
		!s.server.FailOpen || s.server.ReadOnlyTokens != nil || s.tls != nil ||
		s.server.AddressStyle != server.AddressStyleAuto || s.server.StallTimeout != time.Minute {
		t.Fatalf("unexpected defaults: %+v", s)
	}
}

func TestLoadSettingsDisablesMetricsWithPortZero(t *testing.T) {
	t.Parallel()
	values := validEnv()
	values["METRICS_PORT"] = "0"
	s, err := loadSettings(env(values))
	if err != nil || s.metricsPort != 0 {
		t.Fatalf("got metrics port %d, %v; want 0 (disabled)", s.metricsPort, err)
	}
}

func TestLoadSettingsSplitsTokenListsAtCommas(t *testing.T) {
	t.Parallel()
	values := validEnv()
	values["NX_CACHE_ACCESS_TOKEN"] = "old , new,next"
	values["NX_CACHE_READ_ONLY_TOKEN"] = "read-old  ,  read-new"
	s, err := loadSettings(env(values))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.server.AccessTokens, []string{"old", "new", "next"}) ||
		!slices.Equal(s.server.ReadOnlyTokens, []string{"read-old", "read-new"}) {
		t.Fatalf("got access %q, read-only %q", s.server.AccessTokens, s.server.ReadOnlyTokens)
	}
}

func TestLoadSettingsReadsForcedAddressStyle(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]server.AddressStyle{
		"true":  server.AddressStylePath,
		"false": server.AddressStyleVirtualHosted,
	} {
		values := validEnv()
		values["S3_FORCE_PATH_STYLE"] = value
		s, err := loadSettings(env(values))
		if err != nil {
			t.Fatal(err)
		}
		if s.server.AddressStyle != want {
			t.Errorf("S3_FORCE_PATH_STYLE=%s: got style %d, want %d", value, s.server.AddressStyle, want)
		}
	}
}

func TestLoadSettingsRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		key, value string
		want       error
	}{
		"bad port":            {"PORT", "http", errPort},
		"port out of range":   {"PORT", "70000", errPort},
		"bad fail-open":       {"NX_CACHE_FAIL_OPEN", "maybe", errFailOpen},
		"empty fail-open":     {"NX_CACHE_FAIL_OPEN", "", errFailOpen},
		"cert without key":    {"TLS_CERT_PATH", "testdata/cert.pem", errTLSPair},
		"unreadable key pair": {"TLS_KEY_PATH", "missing.pem", errTLSPair},
		"bad path style":      {"S3_FORCE_PATH_STYLE", "yes", errForcePathStyle},
		"empty path style":    {"S3_FORCE_PATH_STYLE", "", errForcePathStyle},
		"bad metrics port":    {"METRICS_PORT", "metrics", errMetricsPort},
		"empty metrics port":  {"METRICS_PORT", "", errMetricsPort},
		"metrics port range":  {"METRICS_PORT", "65536", errMetricsPort},
	}
	for name, tc := range cases {
		values := validEnv()
		values[tc.key] = tc.value
		if name == "unreadable key pair" {
			values["TLS_CERT_PATH"] = "testdata/cert.pem"
		}
		if _, err := loadSettings(env(values)); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
}

func TestLoadSettingsReadsMaxArtifactBytes(t *testing.T) {
	t.Parallel()
	s, err := loadSettings(env(validEnv()))
	if err != nil || s.server.MaxArtifactSize != 0 {
		t.Fatalf("unset: got %d, %v; want 0 (no limit)", s.server.MaxArtifactSize, err)
	}
	for value, want := range map[string]int64{"0": 0, "52428800": 52428800, "5368709120": 5368709120} {
		values := validEnv()
		values["NX_CACHE_MAX_ARTIFACT_BYTES"] = value
		s, err := loadSettings(env(values))
		if err != nil || s.server.MaxArtifactSize != want {
			t.Errorf("NX_CACHE_MAX_ARTIFACT_BYTES=%q: got %d, %v; want %d", value, s.server.MaxArtifactSize, err, want)
		}
	}
	for _, value := range []string{"", "abc", "-1", "1.5", "1e6", " 5", "99999999999999999999"} {
		values := validEnv()
		values["NX_CACHE_MAX_ARTIFACT_BYTES"] = value
		if _, err := loadSettings(env(values)); !errors.Is(err, errMaxArtifactSize) {
			t.Errorf("NX_CACHE_MAX_ARTIFACT_BYTES=%q: got %v, want %v", value, err, errMaxArtifactSize)
		}
	}
}

func TestLoadSettingsReadsStallTimeout(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]time.Duration{"90s": 90 * time.Second, "1m30s": 90 * time.Second, "500ms": 500 * time.Millisecond} {
		values := validEnv()
		values["NX_CACHE_STALL_TIMEOUT"] = value
		s, err := loadSettings(env(values))
		if err != nil || s.server.StallTimeout != want {
			t.Errorf("NX_CACHE_STALL_TIMEOUT=%q: got %s, %v; want %s", value, s.server.StallTimeout, err, want)
		}
	}
	for _, value := range []string{"", "60", "abc", "1x", " 60s"} {
		values := validEnv()
		values["NX_CACHE_STALL_TIMEOUT"] = value
		if _, err := loadSettings(env(values)); !errors.Is(err, server.ErrStallTimeout) {
			t.Errorf("NX_CACHE_STALL_TIMEOUT=%q: got %v, want %v", value, err, server.ErrStallTimeout)
		}
	}
}

func TestRunRejectsInvalidServerConfiguration(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		key, value string
		want       error
	}{
		"empty access token":    {"NX_CACHE_ACCESS_TOKEN", "", server.ErrAccessToken},
		"empty access item":     {"NX_CACHE_ACCESS_TOKEN", "a,,b", server.ErrAccessToken},
		"trailing comma":        {"NX_CACHE_ACCESS_TOKEN", "a, ", server.ErrAccessToken},
		"space outside list":    {"NX_CACHE_ACCESS_TOKEN", " a,b", server.ErrAccessToken},
		"duplicate token":       {"NX_CACHE_ACCESS_TOKEN", "trusted, trusted", server.ErrDuplicateToken},
		"empty read-only token": {"NX_CACHE_READ_ONLY_TOKEN", "", server.ErrReadOnlyToken},
		"empty read-only item":  {"NX_CACHE_READ_ONLY_TOKEN", "r,", server.ErrReadOnlyToken},
		"token in both lists":   {"NX_CACHE_READ_ONLY_TOKEN", "r, trusted", server.ErrSameTokens},
		"zero stall timeout":    {"NX_CACHE_STALL_TIMEOUT", "0s", server.ErrStallTimeout},
		"bare zero stall":       {"NX_CACHE_STALL_TIMEOUT", "0", server.ErrStallTimeout},
		"negative stall":        {"NX_CACHE_STALL_TIMEOUT", "-5s", server.ErrStallTimeout},
	}
	for name, tc := range cases {
		values := validEnv()
		values["PORT"] = "0"
		values["METRICS_PORT"] = "0"
		values[tc.key] = tc.value
		if err := run(context.Background(), env(values), io.Discard); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
}

func TestServeAnswersHealthAndShutsDownGracefully(t *testing.T) {
	t.Parallel()
	s, err := loadSettings(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	assertServes(t, &s, http.DefaultClient, "http")
}

func TestServeUsesTLSWhenConfigured(t *testing.T) {
	t.Parallel()
	values := validEnv()
	values["TLS_CERT_PATH"] = "testdata/cert.pem"
	values["TLS_KEY_PATH"] = "testdata/key.pem"
	s, err := loadSettings(env(values))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := os.ReadFile("testdata/ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}},
	}
	assertServes(t, &s, client, "https")
}

func TestServeExposesMetricsOnMetricsListener(t *testing.T) {
	t.Parallel()
	s, err := loadSettings(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	listener, mainPort := listen(t)
	metricsListener, metricsPort := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, &s, listener, metricsListener, io.Discard) }()

	// Storage at 127.0.0.1:1 is unreachable, so fail-open answers a miss.
	cacheReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://localhost:"+strconv.Itoa(mainPort)+"/v1/cache/abc", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	cacheReq.Header.Set("Authorization", "Bearer trusted")
	if status, _ := fetch(t, http.DefaultClient, cacheReq); status != http.StatusNotFound {
		t.Fatalf("cache GET: status %d, want 404", status)
	}
	status, body := fetch(t, http.DefaultClient, newGet(t, "http://localhost:"+strconv.Itoa(metricsPort)+"/metrics"))
	want := `nx_cache_requests_total{method="GET",outcome="degraded",tier="static"} 1`
	if status != http.StatusOK || !strings.Contains(body, want) {
		t.Fatalf("metrics: %d, want 200 containing %q:\n%s", status, want, body)
	}
	expectShutdown(t, cancel, done)
}

func listen(t *testing.T) (net.Listener, int) {
	t.Helper()
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %T is not TCP", listener.Addr())
	}
	return listener, addr.Port
}

func newGet(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func fetch(t *testing.T, client *http.Client, req *http.Request) (int, string) {
	t.Helper()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(body)
}

func expectShutdown(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v after shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after context cancellation")
	}
}

func assertServes(t *testing.T, s *settings, client *http.Client, scheme string) {
	t.Helper()
	listener, port := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, s, listener, nil, io.Discard) }()

	status, body := fetch(t, client, newGet(t, scheme+"://localhost:"+strconv.Itoa(port)+"/health"))
	if status != http.StatusOK || body != "OK" {
		t.Fatalf("health: %d %q", status, body)
	}
	expectShutdown(t, cancel, done)
}

const issuedKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" // base64 of 32 bytes

func exchangeEnv() map[string]string {
	values := validEnv()
	delete(values, "NX_CACHE_ACCESS_TOKEN")
	values["NX_CACHE_OIDC_PROVIDERS"] = "github"
	values["NX_CACHE_OIDC_AUDIENCE"] = "https://cache.example.com"
	values["NX_CACHE_ISSUED_TOKEN_KEYS"] = issuedKey
	values["NX_CACHE_GITHUB_OWNER_IDS"] = "1001"
	return values
}

func TestLoadSettingsReadsTokenExchange(t *testing.T) {
	t.Parallel()
	values := exchangeEnv()
	values["NX_CACHE_ISSUED_TOKEN_KEYS"] = base64.StdEncoding.EncodeToString(
		[]byte("new-key-0123456789abcdef012345678")) + ", " + issuedKey
	values["NX_CACHE_ISSUED_TOKEN_TTL"] = "12h"
	values["NX_CACHE_GITHUB_ISSUER"] = "https://ghes.example.com/_services/token"
	values["NX_CACHE_GITHUB_OWNER_IDS"] = "1001, 1002"
	values["NX_CACHE_GITHUB_REPOSITORY_IDS"] = "2002"
	values["NX_CACHE_GITHUB_TRUSTED_BRANCHES"] = "main, release/*"
	values["NX_CACHE_GITHUB_READ_ONLY_TOKENS"] = "2002:reader-a, 3003:reader:b"
	s, err := loadSettings(env(values))
	if err != nil {
		t.Fatal(err)
	}
	want := &server.TokenExchange{
		Audience: "https://cache.example.com",
		Keys:     [][]byte{[]byte("new-key-0123456789abcdef012345678"), []byte("0123456789abcdef0123456789abcdef")},
		TTL:      12 * time.Hour,
		GitHub: &server.GitHub{
			Issuer:          "https://ghes.example.com/_services/token",
			OwnerIDs:        []string{"1001", "1002"},
			RepositoryIDs:   []string{"2002"},
			TrustedBranches: []string{"main", "release/*"},
			ReadOnlyTokens: []server.RepositoryToken{
				{RepositoryID: "2002", Token: "reader-a"}, {RepositoryID: "3003", Token: "reader:b"},
			},
		},
	}
	if !reflect.DeepEqual(s.server.TokenExchange, want) || s.server.AccessTokens != nil {
		t.Fatalf("got exchange %+v (GitHub %+v), access tokens %q; want %+v and no access tokens",
			s.server.TokenExchange, s.server.TokenExchange.GitHub, s.server.AccessTokens, want)
	}
}

func TestLoadSettingsAppliesTokenExchangeDefaults(t *testing.T) {
	t.Parallel()
	s, err := loadSettings(env(exchangeEnv()))
	if err != nil {
		t.Fatal(err)
	}
	gh := s.server.TokenExchange.GitHub
	if s.server.TokenExchange.TTL != 6*time.Hour || gh.Issuer != "https://token.actions.githubusercontent.com" ||
		!slices.Equal(gh.TrustedBranches, []string{"main"}) || gh.RepositoryIDs != nil || gh.ReadOnlyTokens != nil {
		t.Fatalf("unexpected defaults: %+v, GitHub %+v", s.server.TokenExchange, gh)
	}
}

func TestLoadSettingsRejectsInvalidTokenExchange(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		key, value string
		want       error
	}{
		"unknown provider":        {"NX_CACHE_OIDC_PROVIDERS", "gitlab", server.ErrOIDCProvider},
		"key not base64":          {"NX_CACHE_ISSUED_TOKEN_KEYS", "not base64!", server.ErrIssuedTokenKeys},
		"bad ttl":                 {"NX_CACHE_ISSUED_TOKEN_TTL", "6", server.ErrIssuedTokenTTL},
		"read-only without colon": {"NX_CACHE_GITHUB_READ_ONLY_TOKENS", "2002", server.ErrRepositoryToken},
	}
	for name, tc := range cases {
		values := exchangeEnv()
		values[tc.key] = tc.value
		if _, err := loadSettings(env(values)); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
	values := validEnv()
	values["NX_CACHE_GITHUB_OWNER_IDS"] = "1001"
	if _, err := loadSettings(env(values)); !errors.Is(err, errExchangeOff) {
		t.Errorf("exchange variable without NX_CACHE_OIDC_PROVIDERS: got %v, want %v", err, errExchangeOff)
	}
}

func TestRunRejectsInvalidTokenExchange(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		key, value string
		want       error
	}{
		"ttl over 120h":      {"NX_CACHE_ISSUED_TOKEN_TTL", "121h", server.ErrIssuedTokenTTL},
		"short key":          {"NX_CACHE_ISSUED_TOKEN_KEYS", "c2hvcnQ=", server.ErrIssuedTokenKeys},
		"no audience":        {"NX_CACHE_OIDC_AUDIENCE", "", server.ErrOIDCAudience},
		"no repositories":    {"NX_CACHE_GITHUB_OWNER_IDS", "", server.ErrGitHubIDs},
		"name instead of id": {"NX_CACHE_GITHUB_REPOSITORY_IDS", "acme/app", server.ErrGitHubIDs},
		"trust every branch": {"NX_CACHE_GITHUB_TRUSTED_BRANCHES", "main, *", server.ErrTrustedBranches},
		"bad branch pattern": {"NX_CACHE_GITHUB_TRUSTED_BRANCHES", "release/[", server.ErrTrustedBranches},
		"issuer not a URL": {
			"NX_CACHE_GITHUB_ISSUER",
			"token.actions.githubusercontent.com",
			server.ErrGitHubIssuer,
		},
		"reader reuses token": {"NX_CACHE_GITHUB_READ_ONLY_TOKENS", "2002:r", server.ErrRepositoryToken},
	}
	for name, tc := range cases {
		values := exchangeEnv()
		values["PORT"] = "0"
		values["METRICS_PORT"] = "0"
		values["NX_CACHE_READ_ONLY_TOKEN"] = "r"
		values[tc.key] = tc.value
		if err := run(context.Background(), env(values), io.Discard); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
}
