//go:build e2e

package e2e_test

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/the-inconvenience-store/nx-remote-cache/internal/oidctest"
	"github.com/the-inconvenience-store/nx-remote-cache/internal/s3test"
)

const (
	audience   = "e2e-nx-remote-cache"
	repoReader = "e2e-repo-reader"
	// issuedKey is base64 of 32 bytes.
	issuedKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
)

// TestNxSharesResultsThroughRepositoryTiers runs real Nx builds with issued
// tokens: a pull request reuses its own results, the trusted branch never
// sees them, and other pull requests and developers read the trusted tier.
func TestNxSharesResultsThroughRepositoryTiers(t *testing.T) {
	t.Parallel()
	binary := buildServer(t)
	for _, version := range nxVersions() {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			for name, start := range s3test.Starters() {
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					testTiers(t, binary, version, start)
				})
			}
		})
	}
}

func testTiers(t *testing.T, binary, version string, start s3test.Starter) {
	t.Helper()
	issuer := oidctest.NewIssuer(t)
	issuer.Defaults["aud"] = audience
	issuer.Defaults["repository"] = "acme/app"
	issuer.Defaults["repository_id"] = "2002"
	issuer.Defaults["repository_owner_id"] = "1001"
	serverURL, serverLog := startServer(t, binary, start(t),
		"NX_CACHE_OIDC_PROVIDERS=github",
		"NX_CACHE_OIDC_AUDIENCE="+audience,
		"NX_CACHE_ISSUED_TOKEN_KEYS="+issuedKey,
		"NX_CACHE_GITHUB_ISSUER="+issuer.URL,
		"NX_CACHE_GITHUB_OWNER_IDS=1001",
		"NX_CACHE_GITHUB_READ_ONLY_TOKENS=2002:"+repoReader,
	)
	workspace := createWorkspace(t, version)
	pr := func(number int) string {
		return exchangeToken(t, serverURL, issuer.Sign(t, map[string]any{
			"event_name": "pull_request", "ref": "refs/pull/" + strconv.Itoa(number) + "/merge",
		}))
	}

	steps := []struct {
		name, token, want string
	}{
		{"pull request's first build runs and uploads", pr(5), missMarker},
		{"pull request's rebuild restores its own result", pr(5), remoteMarker},
		{"trusted branch does not see the pull request's result", exchangeToken(t, serverURL,
			issuer.Sign(t, map[string]any{
				"event_name": "push", "ref": "refs/heads/main", "ref_type": "branch", "ref_protected": "true",
			})), missMarker},
		{"another pull request restores the trusted result", pr(6), remoteMarker},
		{"repository read-only token restores the trusted result", repoReader, remoteMarker},
	}
	for _, step := range steps {
		if out := nxBuild(t, workspace, serverURL, step.token); !strings.Contains(out, step.want) {
			t.Fatalf("%s: want %q in build output:\n%s\nserver log:\n%s", step.name, step.want, out, serverLog)
		}
	}
	for _, tier := range []string{"tier=scoped", "tier=trusted"} {
		if !strings.Contains(serverLog.String(), tier) {
			t.Fatalf("no request logged %s:\n%s", tier, serverLog)
		}
	}
}

// exchangeToken trades a CI token for an issued cache token.
func exchangeToken(t *testing.T, serverURL, jwt string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, serverURL+"/v1/token", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token exchange: %d %s", resp.StatusCode, body)
	}
	return string(body)
}
