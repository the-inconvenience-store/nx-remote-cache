package server_test

import (
	"bytes"
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/the-inconvenience-store/nx-remote-cache/internal/oidctest"
	"github.com/the-inconvenience-store/nx-remote-cache/internal/server"
)

const (
	testAudience = "nx-remote-cache-test"
	ownerID      = "1001"
	repoID       = "2002"
	otherRepoID  = "3003"
)

// newFakeIssuer starts a GitHub-like issuer whose tokens are for this test
// server's audience and come from acme/app.
func newFakeIssuer(t *testing.T) *oidctest.Issuer {
	t.Helper()
	issuer := oidctest.NewIssuer(t)
	maps.Copy(issuer.Defaults, map[string]any{
		"aud": testAudience, "repository": "acme/app", "repository_id": repoID, "repository_owner_id": ownerID,
		"run_id": "777", "actor": "octocat",
	})
	return issuer
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
func withExchange(issuer *oidctest.Issuer, adjust ...func(*server.TokenExchange)) func(*server.Config) {
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
func issue(t *testing.T, h http.Handler, issuer *oidctest.Issuer, claims map[string]any) string {
	t.Helper()
	status, token := exchange(t, h, issuer.Sign(t, claims))
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

func scopedKey(namespace, hash string) string {
	return "scoped/github/" + repoID + "/" + namespace + "/" + hash
}

func trustedKey(hash string) string {
	return "trusted/github/" + repoID + "/" + hash
}

func TestScopedTokenReadsTrustedTierFirst(t *testing.T) {
	t.Parallel()
	issuer := newFakeIssuer(t)
	h := newHandler(t, newFakeS3(t).URL, options{configure: withExchange(issuer)})
	trusted := issue(t, h, issuer, push("main", true))
	pr := issue(t, h, issuer, pullRequest(7))

	put(t, h, "shared", pr, "pr copy")
	put(t, h, "shared", trusted, "trusted copy")
	put(t, h, "main-only", trusted, "from main")

	status, body := get(t, h, "shared", pr)
	expect(t, status, body, http.StatusOK, "trusted copy")
	status, body = get(t, h, "main-only", pr)
	expect(t, status, body, http.StatusOK, "from main")
}

func TestPushesAndMergeQueueMapToTheirTier(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		claims  map[string]any
		wantKey string
	}{
		"protected trusted branch":            {push("main", true), trustedKey("h")},
		"protected branch matching a pattern": {push("release/1.2", true), trustedKey("h")},
		"unprotected trusted branch":          {push("main", false), scopedKey("branch/main", "h")},
		"other branch":                        {push("feature/x y", true), scopedKey("branch/feature%2Fx%20y", "h")},
		"merge queue": {
			map[string]any{"event_name": "merge_group", "ref": "refs/heads/gh-readonly-queue/main/pr-12-abc"},
			scopedKey("queue/main%2Fpr-12-abc", "h"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s3 := newFakeS3(t)
			issuer := newFakeIssuer(t)
			h := newHandler(t, s3.URL, options{configure: withExchange(issuer, func(x *server.TokenExchange) {
				x.GitHub.TrustedBranches = []string{"main", "release/*"}
			})})
			status, body := put(t, h, "h", issue(t, h, issuer, tc.claims), "artifact")
			expect(t, status, body, http.StatusOK, "Successfully uploaded")
			if _, ok := s3.object(tc.wantKey); !ok {
				t.Fatalf("artifact not stored at %s", tc.wantKey)
			}
		})
	}
}

func TestExchangeRefusesRunsThatMustNotWrite(t *testing.T) {
	t.Parallel()
	issuer := newFakeIssuer(t)
	h := newHandler(t, newFakeS3(t).URL, options{configure: withExchange(issuer)})
	for name, tc := range map[string]struct {
		claims   map[string]any
		wantBody string
	}{
		"pull_request_target": {
			map[string]any{"event_name": "pull_request_target", "ref": "refs/heads/main", "ref_protected": "true"},
			`Event "pull_request_target" cannot get a cache token`,
		},
		"workflow_run": {
			map[string]any{"event_name": "workflow_run", "ref": "refs/heads/main", "ref_protected": "true"},
			`Event "workflow_run" cannot get a cache token`,
		},
		"issue_comment": {
			map[string]any{"event_name": "issue_comment", "ref": "refs/heads/main"},
			`Event "issue_comment" cannot get a cache token`,
		},
		"tag push": {
			map[string]any{"event_name": "push", "ref": "refs/tags/v1", "ref_type": "tag"},
			"Only pushes to branches can get a cache token, not refs/tags/v1",
		},
		"pull request head ref": {
			map[string]any{"event_name": "pull_request", "ref": "refs/pull/3/head"},
			"Pull request ref refs/pull/3/head is not refs/pull/<number>/merge",
		},
		"merge queue outside the queue prefix": {
			map[string]any{"event_name": "merge_group", "ref": "refs/heads/main"},
			"Merge queue ref refs/heads/main is not under refs/heads/gh-readonly-queue/",
		},
		"repository of another owner": {
			map[string]any{
				"event_name": "push", "ref": "refs/heads/main", "ref_type": "branch",
				"repository_owner_id": "9", "repository_id": "99", "repository": "evil/app",
			},
			"Repository evil/app (ID 99) is not allowed to use this cache",
		},
		"no repository id": {
			map[string]any{"event_name": "push", "ref": "refs/heads/main", "ref_type": "branch", "repository_id": ""},
			"CI token has no repository_id or repository_owner_id claim",
		},
	} {
		status, body := exchange(t, h, issuer.Sign(t, tc.claims))
		if status != http.StatusForbidden || body != tc.wantBody {
			t.Errorf("%s: got %d %q, want 403 %q", name, status, body, tc.wantBody)
		}
	}
}

func TestRepositoriesSharingABucketAreIsolated(t *testing.T) {
	t.Parallel()
	issuer := newFakeIssuer(t)
	h := newHandler(t, newFakeS3(t).URL, options{configure: withExchange(issuer, func(x *server.TokenExchange) {
		x.GitHub.RepositoryIDs = []string{otherRepoID}
	})})
	other := map[string]any{"repository_owner_id": "4004", "repository_id": otherRepoID, "repository": "other/app"}
	otherTrusted := push("main", true)
	maps.Copy(otherTrusted, other)
	otherPR := pullRequest(1)
	maps.Copy(otherPR, other)

	put(t, h, "h", issue(t, h, issuer, push("main", true)), "acme artifact")
	put(t, h, "p", issue(t, h, issuer, pullRequest(1)), "acme pr artifact")

	for _, token := range []string{issue(t, h, issuer, otherTrusted), issue(t, h, issuer, otherPR)} {
		for _, hash := range []string{"h", "p"} {
			status, body := get(t, h, hash, token)
			expect(t, status, body, http.StatusNotFound, "The record was not found")
		}
	}
}

func TestInvalidCITokensAreRejected(t *testing.T) {
	t.Parallel()
	issuer := newFakeIssuer(t)
	stranger := newFakeIssuer(t)
	h := newHandler(t, newFakeS3(t).URL, options{configure: withExchange(issuer)})
	issue(t, h, issuer, pullRequest(1))

	expired := pullRequest(1)
	expired["exp"] = time.Now().Add(-time.Minute).Unix()
	wrongAudience := pullRequest(1)
	wrongAudience["aud"] = "someone-else"
	for name, jwt := range map[string]string{
		"no token":            "",
		"not a JWT":           "abc.def.ghi",
		"expired":             issuer.Sign(t, expired),
		"wrong audience":      issuer.Sign(t, wrongAudience),
		"other issuer":        stranger.Sign(t, pullRequest(1)),
		"other key, same kid": issuer.SignWith(t, stranger.Key, issuer.KID, pullRequest(1)),
		"unknown kid":         issuer.SignWith(t, stranger.Key, "key-2", pullRequest(1)),
	} {
		status, body := exchange(t, h, jwt)
		if status != http.StatusUnauthorized || body != "Missing or invalid CI OIDC token" {
			t.Errorf("%s: got %d %q, want 401", name, status, body)
		}
	}
	if got := issuer.Fetches.Load(); got != 1 {
		t.Errorf("issuer keys fetched %d times, want 1: unknown key IDs must not refetch within the interval", got)
	}
}

func TestIssuerOutageFailsOnlyNewExchanges(t *testing.T) {
	t.Parallel()
	issuer := newFakeIssuer(t)
	issuer.Down.Store(true)
	h := newHandler(t, newFakeS3(t).URL, options{configure: withExchange(issuer)})
	status, body := exchange(t, h, issuer.Sign(t, pullRequest(1)))
	expect(t, status, body, http.StatusServiceUnavailable, "CI token issuer unavailable; try again later")

	up := newFakeIssuer(t)
	h = newHandler(t, newFakeS3(t).URL, options{configure: withExchange(up)})
	token := issue(t, h, up, push("main", true))
	up.Down.Store(true)
	status, body = put(t, h, "h", token, "artifact")
	expect(t, status, body, http.StatusOK, "Successfully uploaded")
}

func TestIssuedTokensAreCheckedOnEveryRequest(t *testing.T) {
	t.Parallel()
	s3 := newFakeS3(t)
	issuer := newFakeIssuer(t)
	h := newHandler(t, s3.URL, options{configure: withExchange(issuer)})
	token := issue(t, h, issuer, pullRequest(1))
	oldKey := []byte("0123456789abcdef0123456789abcdef")
	newKey := []byte("fedcba9876543210fedcba9876543210")

	rotated := newHandler(t, s3.URL, options{configure: withExchange(issuer, func(x *server.TokenExchange) {
		x.Keys = [][]byte{newKey, oldKey}
	})})
	status, _ := put(t, rotated, "h", token, "artifact")
	if status != http.StatusOK {
		t.Errorf("token signed with a rotated-out signing key that is still listed: status %d", status)
	}

	for name, tc := range map[string]struct {
		h        http.Handler
		token    string
		wantBody string
	}{
		"key no longer listed": {
			newHandler(t, s3.URL, options{configure: withExchange(issuer, func(x *server.TokenExchange) {
				x.Keys = [][]byte{newKey}
			})}),
			token, "Missing or invalid authentication token",
		},
		"repository no longer admitted": {
			newHandler(t, s3.URL, options{configure: withExchange(issuer, func(x *server.TokenExchange) {
				x.GitHub.OwnerIDs = nil
				x.GitHub.RepositoryIDs = []string{otherRepoID}
			})}),
			token, "This repository is not allowed to use this cache",
		},
		"expired": {
			h: func() http.Handler {
				return newHandler(t, s3.URL, options{configure: withExchange(issuer, func(x *server.TokenExchange) {
					x.TTL = time.Nanosecond
				})})
			}(),
			wantBody: "Cache token expired; exchange a new CI token for it",
		},
		"tampered payload": {h, token[:len(token)-2] + "AA", "Missing or invalid authentication token"},
	} {
		if tc.token == "" {
			tc.token = issue(t, tc.h, issuer, pullRequest(1))
		}
		status, body := get(t, tc.h, "h", tc.token)
		if status != http.StatusUnauthorized || body != tc.wantBody {
			t.Errorf("%s: got %d %q, want 401 %q", name, status, body, tc.wantBody)
		}
	}
}

func TestRepositoryReadOnlyTokenReadsOnlyItsTrustedTier(t *testing.T) {
	t.Parallel()
	issuer := newFakeIssuer(t)
	h := newHandler(t, newFakeS3(t).URL, options{configure: withExchange(issuer, func(x *server.TokenExchange) {
		x.GitHub.RepositoryIDs = []string{otherRepoID}
		x.GitHub.ReadOnlyTokens = []server.RepositoryToken{{RepositoryID: repoID, Token: "acme-reader"}}
	})})
	put(t, h, "h", issue(t, h, issuer, push("main", true)), "trusted artifact")
	put(t, h, "p", issue(t, h, issuer, pullRequest(1)), "pr artifact")
	other := push("main", true)
	maps.Copy(other, map[string]any{"repository_owner_id": "4004", "repository_id": otherRepoID})
	put(t, h, "o", issue(t, h, issuer, other), "other repository artifact")

	status, body := get(t, h, "h", "acme-reader")
	expect(t, status, body, http.StatusOK, "trusted artifact")
	for _, hash := range []string{"p", "o"} {
		status, body = get(t, h, hash, "acme-reader")
		expect(t, status, body, http.StatusNotFound, "The record was not found")
	}
	status, body = put(t, h, "new", "acme-reader", "data")
	expect(t, status, body, http.StatusForbidden, "Read-only credentials cannot upload artifacts")
}

func TestStaticTierStaysSeparateFromRepositoryTiers(t *testing.T) {
	t.Parallel()
	issuer := newFakeIssuer(t)
	h := newHandler(t, newFakeS3(t).URL, options{configure: withExchange(issuer)})
	trusted := issue(t, h, issuer, push("main", true))

	put(t, h, "static", accessToken, "static artifact")
	put(t, h, "repo", trusted, "repository artifact")

	status, body := get(t, h, "static", trusted)
	expect(t, status, body, http.StatusNotFound, "The record was not found")
	status, body = get(t, h, "repo", accessToken)
	expect(t, status, body, http.StatusNotFound, "The record was not found")
	status, body = get(t, h, "static", accessToken)
	expect(t, status, body, http.StatusOK, "static artifact")
}

func TestExchangeWithoutStaticTokensServesOnlyIssuedTokens(t *testing.T) {
	t.Parallel()
	issuer := newFakeIssuer(t)
	h := newHandler(t, newFakeS3(t).URL, options{configure: func(c *server.Config) {
		withExchange(issuer)(c)
		c.AccessTokens = nil
	}})
	status, body := put(t, h, "h", issue(t, h, issuer, push("main", true)), "artifact")
	expect(t, status, body, http.StatusOK, "Successfully uploaded")
	status, body = get(t, h, "h", accessToken)
	expect(t, status, body, http.StatusUnauthorized, "Missing or invalid authentication token")
}

func TestTokenRouteExistsOnlyWithExchange(t *testing.T) {
	t.Parallel()
	h := newHandler(t, newFakeS3(t).URL, options{})
	status, _ := exchange(t, h, "anything")
	if status != http.StatusMethodNotAllowed && status != http.StatusNotFound {
		t.Fatalf("POST /v1/token without exchange: status %d, want 404 or 405", status)
	}
}

func TestIssuedUploadRecordsRunMetadataAndLog(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	s3 := newFakeS3(t)
	issuer := newFakeIssuer(t)
	h := newHandler(t, s3.URL, options{logs: &logs, configure: withExchange(issuer)})
	token := issue(t, h, issuer, pullRequest(12))

	put(t, h, "m", token, "artifact")
	metadata := s3.objectMetadata(scopedKey("pr/12", "m"))
	for name, want := range map[string]string{
		"nx-repository": "github/" + repoID, "nx-namespace": "pr/12", "nx-run-id": "777", "nx-actor": "octocat",
	} {
		if metadata[name] != want {
			t.Errorf("metadata %s = %q, want %q", name, metadata[name], want)
		}
	}
	expectRequestLog(t, &logs, map[string]string{
		"tier": "scoped", "repository": "github/" + repoID, "namespace": "pr/12", "access": "rw",
	})
	if strings.Contains(logs.String(), token) {
		t.Error("logs contain the issued token")
	}
}

// A request waiting for an issuer key fetch another exchange started must stop
// waiting when its client goes away, instead of queueing behind the fetch.
func TestExchangeWaitingOnAKeyFetchHonoursCancellation(t *testing.T) {
	t.Parallel()
	issuer := newFakeIssuer(t)
	arrived, release := make(chan struct{}), make(chan struct{})
	issuer.BeforeJWKS = func() {
		arrived <- struct{}{}
		<-release
	}
	h := newHandler(t, newFakeS3(t).URL, options{configure: withExchange(issuer)})
	first := make(chan int, 1)
	go func() {
		status, _ := exchange(t, h, issuer.Sign(t, pullRequest(1)))
		first <- status
	}()
	<-arrived

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/token", nil)
	req.Header.Set("Authorization", "Bearer "+issuer.Sign(t, pullRequest(2)))
	start := time.Now()
	h.ServeHTTP(httptest.NewRecorder(), req)
	waited := time.Since(start)
	close(release)

	if waited > 2*time.Second {
		t.Errorf("cancelled exchange waited %s for another exchange's key fetch", waited)
	}
	if status := <-first; status != http.StatusOK {
		t.Errorf("exchange that started the fetch: status %d, want 200", status)
	}
}
