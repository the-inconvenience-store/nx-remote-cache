package server

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// DefaultGitHubIssuer is the github.com Actions OIDC issuer.
const DefaultGitHubIssuer = "https://token.actions.githubusercontent.com"

// GitHub configures the GitHub Actions token exchange provider.
type GitHub struct {
	// Issuer is the Actions OIDC issuer: DefaultGitHubIssuer, or the issuer of
	// GitHub Enterprise Server or GHE.com. One issuer per deployment, because
	// repository IDs of different GitHub instances can collide.
	Issuer string
	// OwnerIDs and RepositoryIDs admit repositories by immutable numeric ID:
	// every repository of an owner, or single repositories.
	OwnerIDs      []string
	RepositoryIDs []string
	// TrustedBranches are path.Match patterns; a push to a matching branch
	// that GitHub reports as protected writes the repository's trusted tier.
	TrustedBranches []string
	// ReadOnlyTokens each read one repository's trusted tier.
	ReadOnlyTokens []RepositoryToken
}

// RepositoryToken is a static read-only token for one repository.
type RepositoryToken struct {
	RepositoryID string
	Token        string
}

// maxNamespace keeps the longest scoped key under S3's 1024-byte key limit.
const maxNamespace = 768

var pullRequestRef = regexp.MustCompile(`^refs/pull/([0-9]+)/merge$`)

const (
	branchRefPrefix = "refs/heads/"
	mergeQueueRef   = "refs/heads/gh-readonly-queue/"
)

type githubProvider struct {
	iss     string
	owners  map[string]struct{}
	repos   map[string]struct{}
	trusted []string
}

func newGitHubProvider(cfg *GitHub) *githubProvider {
	p := &githubProvider{
		iss:     cfg.Issuer,
		owners:  make(map[string]struct{}, len(cfg.OwnerIDs)),
		repos:   make(map[string]struct{}, len(cfg.RepositoryIDs)),
		trusted: cfg.TrustedBranches,
	}
	for _, id := range cfg.OwnerIDs {
		p.owners[id] = struct{}{}
	}
	for _, id := range cfg.RepositoryIDs {
		p.repos[id] = struct{}{}
	}
	return p
}

func (*githubProvider) name() string { return "github" }

func (p *githubProvider) issuer() string { return p.iss }

func (p *githubProvider) admits(ownerID, repositoryID string) bool {
	_, owner := p.owners[ownerID]
	_, repo := p.repos[repositoryID]
	return owner || repo
}

// githubClaims are the Actions OIDC claims the provider reads.
type githubClaims struct {
	Repository   string     `json:"repository"`
	RepositoryID string     `json:"repository_id"`
	OwnerID      string     `json:"repository_owner_id"`
	EventName    string     `json:"event_name"`
	Ref          string     `json:"ref"`
	RefType      string     `json:"ref_type"`
	RefProtected flagString `json:"ref_protected"`
	RunID        string     `json:"run_id"`
	Actor        string     `json:"actor"`
}

// decide maps a run to a namespace by event_name, never by ref alone:
// pull_request_target, issue_comment and workflow_run report the base branch
// as ref while often running PR code, so every other event is refused.
func (p *githubProvider) decide(raw []byte) decision {
	var c githubClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return decision{refusal: "CI token claims are malformed"}
	}
	d := decision{
		ownerID: c.OwnerID, repositoryID: c.RepositoryID, runID: c.RunID, actor: c.Actor,
		attrs: []any{
			"repository", c.Repository, "repository_id", c.RepositoryID, "event", c.EventName,
			"ref", c.Ref, "actor", c.Actor, "run_id", c.RunID,
		},
	}
	if c.RepositoryID == "" || c.OwnerID == "" {
		d.refusal = "CI token has no repository_id or repository_owner_id claim"
		return d
	}
	if !p.admits(c.OwnerID, c.RepositoryID) {
		d.refusal = fmt.Sprintf("Repository %s (ID %s) is not allowed to use this cache", c.Repository, c.RepositoryID)
		return d
	}
	d.namespace, d.warning, d.refusal = p.namespace(&c)
	if d.refusal == "" && len(d.namespace) > maxNamespace {
		d.refusal = "Branch name is too long for a cache namespace"
	}
	return d
}

// namespace maps an admitted run to its namespace; an empty namespace with no
// refusal is the trusted tier.
func (p *githubProvider) namespace(c *githubClaims) (namespace, warning, refusal string) {
	switch c.EventName {
	case "push":
		branch, ok := strings.CutPrefix(c.Ref, branchRefPrefix)
		if !ok || c.RefType != "branch" {
			return "", "", "Only pushes to branches can get a cache token, not " + c.Ref
		}
		if p.isTrusted(branch) {
			if c.RefProtected {
				return "", "", ""
			}
			warning = "Trusted branch is not protected; issuing a branch token instead"
		}
		return "branch/" + url.PathEscape(branch), warning, ""
	case "pull_request":
		m := pullRequestRef.FindStringSubmatch(c.Ref)
		if m == nil {
			return "", "", "Pull request ref " + c.Ref + " is not refs/pull/<number>/merge"
		}
		return "pr/" + m[1], "", ""
	case "merge_group":
		entry, ok := strings.CutPrefix(c.Ref, mergeQueueRef)
		if !ok || entry == "" {
			return "", "", "Merge queue ref " + c.Ref + " is not under " + mergeQueueRef
		}
		return "queue/" + url.PathEscape(entry), "", ""
	default:
		return "", "", fmt.Sprintf("Event %q cannot get a cache token", c.EventName)
	}
}

func (p *githubProvider) isTrusted(branch string) bool {
	for _, pattern := range p.trusted {
		// Patterns are validated at startup, so Match cannot fail here.
		if ok, _ := path.Match(pattern, branch); ok {
			return true
		}
	}
	return false
}

// flagString is true only for the claim value "true" (GitHub sends strings);
// a JSON true is accepted too.
type flagString bool

func (f *flagString) UnmarshalJSON(data []byte) error {
	*f = flagString(string(data) == `"true"` || string(data) == "true")
	return nil
}
