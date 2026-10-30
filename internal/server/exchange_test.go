package server_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/the-inconvenience-store/nx-remote-cache/internal/server"
)

const (
	testAudience = "nx-remote-cache-test"
	ownerID      = "1001"
	repoID       = "2002"
	otherRepoID  = "3003"
)

// fakeIssuer is a GitHub-like OIDC issuer: it serves discovery and JWKS and
// signs CI tokens. Down makes both endpoints fail; fetches counts JWKS loads.
type fakeIssuer struct {
	URL     string
	key     *rsa.PrivateKey
	kid     string
	down    atomic.Bool
	fetches atomic.Int32
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{key: key, kid: "key-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		if f.down.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": f.URL, "jwks_uri": f.URL + "/jwks"})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		f.fetches.Add(1)
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &f.key.PublicKey, KeyID: f.kid, Algorithm: "RS256", Use: "sig"},
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

// sign returns a CI token for claims over defaults valid for five minutes.
func (f *fakeIssuer) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	return f.signWith(t, f.key, f.kid, claims)
}

func (f *fakeIssuer) signWith(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	now := time.Now()
	all := map[string]any{
		"iss": f.URL, "aud": testAudience,
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"repository": "acme/app", "repository_id": repoID, "repository_owner_id": ownerID,
		"run_id": "777", "actor": "octocat",
	}
	maps.Copy(all, claims)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return compact
}

func pullRequest(number int) map[string]any {
	return map[string]any{
		"event_name": "pull_request", "ref": "refs/pull/" + strconv.Itoa(number) + "/merge",
		"ref_type": "branch", "ref_protected": "false",
	}
}

func push(branch string, protected bool) map[string]any {
	return map[string]any{
		"event_name": "push", "ref": "refs/heads/" + branch,
		"ref_type": "branch", "ref_protected": strconv.FormatBool(protected),
	}
}

// withExchange turns on token exchange with the GitHub provider at issuer.
func withExchange(issuer *fakeIssuer, adjust ...func(*server.TokenExchange)) func(*server.Config) {
	return func(c *server.Config) {
		c.TokenExchange = &server.TokenExchange{
			Audience: testAudience,
			Keys:     [][]byte{[]byte("0123456789abcdef0123456789abcdef")},
			TTL:      time.Hour,
			GitHub: &server.GitHub{
				Issuer:          issuer.URL,
				OwnerIDs:        []string{ownerID},
				TrustedBranches: []string{"main"},
			},
		}
		for _, a := range adjust {
			a(c.TokenExchange)
		}
	}
}

func exchange(t *testing.T, h http.Handler, jwt string) (int, string) {
	t.Helper()
	return do(t, h, call{method: http.MethodPost, path: "/v1/token", token: jwt})
}

// issue exchanges claims for a cache token and fails the test unless it is issued.
func issue(t *testing.T, h http.Handler, issuer *fakeIssuer, claims map[string]any) string {
	t.Helper()
	status, token := exchange(t, h, issuer.sign(t, claims))
	if status != http.StatusOK {
		t.Fatalf("token exchange: %d %q", status, token)
	}
	return token
}

func TestPullRequestTokenWritesOnlyItsOwnNamespace(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	issuer := newFakeIssuer(t)
	h := newHandler(t, s3.URL, options{configure: withExchange(issuer)})
	pr := issue(t, h, issuer, pullRequest(12))

	status, body := put(t, h, "abc", pr, "pr artifact")
	expect(t, status, body, http.StatusOK, "Successfully uploaded")
	if _, ok := s3.object("scoped/github/" + repoID + "/pr/12/abc"); !ok {
		t.Fatal("artifact not stored under the pull request's scoped prefix")
	}
	status, body = get(t, h, "abc", pr)
	expect(t, status, body, http.StatusOK, "pr artifact")

	for name, claims := range map[string]map[string]any{
		"trusted branch":     push("main", true),
		"other pull request": pullRequest(13),
	} {
		status, body = get(t, h, "abc", issue(t, h, issuer, claims))
		if status != http.StatusNotFound {
			t.Errorf("%s read the pull request's artifact: %d %q", name, status, body)
		}
	}
}
