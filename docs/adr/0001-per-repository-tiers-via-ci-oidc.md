---
status: accepted
---

# Per-repository tiers via CI OIDC token exchange

Read-only PR tokens stop cache poisoning
([CVE-2025-36852](https://nx.dev/blog/creep-vulnerability-build-cache-security)),
but PR builds cannot reuse their own results, and every repository using one
server shares one keyspace, so one repository's trusted runs can poison
another's. Opt-in token exchange fixes both: the server issues tokens bound to
one repository and one namespace, derived from a CI OIDC token. Nx sends only a
bearer token to `{server}/v1/cache/{hash}`, so the token is the only place the
repository and namespace can come from: the URL is chosen by the CI job, and
any static secret a PR job can read is readable by the PR author.

## Design

- **Opt-in.** Off unless `NX_CACHE_OIDC_PROVIDERS` is set. When off,
  `POST /v1/token` is not routed and behaviour is unchanged: static tokens and
  the static tier only. When on, `NX_CACHE_ACCESS_TOKEN` becomes optional; with
  neither set the server refuses to start.
- **Tiers and keys.**

  | Tier | Key | Written by | Read by |
  |---|---|---|---|
  | Static | `<hash>` | static access tokens | static tokens |
  | Trusted | `trusted/<provider>/<repository_id>/<hash>` | issued trusted tokens | every token of that repository |
  | Scoped | `scoped/<provider>/<repository_id>/<namespace>/<hash>` | issued scoped tokens | that namespace's issued tokens |

  Hashes cannot contain `/`, so tiers never collide. Repository tiers never
  read the static tier: a static write token is not bound to a repository, so
  reading it would reopen cross-repository poisoning. Existing users who enable
  token exchange therefore start with a cold trusted tier.
- **Token exchange.** A CI job sends `POST /v1/token` with
  `Authorization: Bearer <OIDC JWT>`. The server rejects a JWT whose unverified
  `iss` or `aud` is not configured before any key work, then verifies the RS256
  signature (issuer JWKS, refetch rate-limited), `iss`, `aud`
  (`NX_CACHE_OIDC_AUDIENCE`, required) and expiry, asks the provider for a
  repository and namespace, and answers `text/plain` with an issued token:
  `nxi1.<base64url payload>.<base64url HMAC-SHA256>`, payload = provider, owner
  ID, repository ID, namespace, expiry, run ID. The job exports it as
  `NX_SELF_HOSTED_REMOTE_CACHE_ACCESS_TOKEN`. 401 for an invalid JWT, 403 when
  the provider refuses, 503 when the issuer is unreachable.
  - The JWT is not given to Nx directly: Nx reads its token once and never
    refreshes it, and CI JWTs are short-lived (GitHub's example: 300 s).
    Exchange also keeps JWT verification off the per-request path.
  - Issuer discovery is lazy: startup and `/ready` never contact the issuer,
    and issued tokens keep working through an issuer outage.
- **Keys.** `NX_CACHE_ISSUED_TOKEN_KEYS`: comma-separated base64 keys of at
  least 32 bytes, identical on every replica; the first signs, all verify, so
  keys rotate without downtime. `NX_CACHE_ISSUED_TOKEN_TTL` defaults to 6h
  (the GitHub-hosted job limit); startup rejects more than 120h (the
  self-hosted runner job limit). Rotating keys revokes every issued token.
- **Issued token checks.** Every cache request re-checks that the token's
  owner or repository is still admitted, so removing a repository takes effect
  at once. Branch trust is checked only at exchange. An expired, forged or
  no-longer-admitted token gets 401 with a `text/plain` reason, which fails the
  Nx run visibly rather than degrading silently.
- **Access.** An issued trusted token reads and writes its repository's trusted
  tier. An issued scoped token reads its repository's trusted tier, then its
  namespace, and writes only its namespace: most PR tasks match main, and the
  trusted copy wins when both exist. Writes keep HeadObject + conditional
  PutObject. Upload metadata adds repository, namespace, run ID and actor.
- **Developer reads.** Developers have no OIDC token, and a token that reads
  every repository would serve one repository's artifacts to another.
  `NX_CACHE_GITHUB_READ_ONLY_TOKENS` holds `<repository_id>:<token>` pairs; each
  reads only that repository's trusted tier.
- **Provider seam.** A provider supplies its issuer and maps verified claims to
  a repository and namespace, or a refusal. Signature checks, JWKS caching,
  issuing and tiered storage are shared, so a GitLab provider adds only claim
  rules and its repository settings.
- **GitHub provider** (`github`).
  - Issuer: `NX_CACHE_GITHUB_ISSUER`, default
    `https://token.actions.githubusercontent.com`; set it for GitHub Enterprise
    Server or GHE.com data residency. One issuer per deployment, because
    repository IDs from different GitHub instances can collide.
  - Repositories are admitted by immutable ID, never by name (names can be
    renamed and reused): `repository_owner_id` in `NX_CACHE_GITHUB_OWNER_IDS`
    (whole organisations, including future repositories) or `repository_id` in
    `NX_CACHE_GITHUB_REPOSITORY_IDS`. At least one is required.
  - Trusted branches: `NX_CACHE_GITHUB_TRUSTED_BRANCHES` (default `main`),
    `path.Match` patterns applied to every admitted repository; a bare `*` is a
    startup error and the patterns are logged at startup. A trusted write also
    requires `ref_protected` = `"true"`, so trust rests on verified branch
    protection; a push to an unprotected trusted branch gets its branch
    namespace and a warning.
  - Namespace by `event_name`:

    | `event_name` | Namespace |
    |---|---|
    | `push` to a protected trusted branch | trusted tier |
    | `push` to another branch (`ref_type` = `branch`) | `branch/<escaped branch>` |
    | `pull_request` (`ref` exactly `refs/pull/<N>/merge`) | `pr/<N>` |
    | `merge_group` (`ref` under `refs/heads/gh-readonly-queue/`) | `queue/<escaped branch>` |
    | anything else | refused |

  - Mapping by event, not ref, matters: `pull_request_target`, `issue_comment`
    and `workflow_run` report the base branch as `ref` while often running PR
    code, so they are refused. GitHub does not document whether fork or
    Dependabot PRs can get OIDC tokens; either way such a token maps only to
    `pr/<N>` of the base repository. Branch names are escaped with
    `url.PathEscape`.
- **GitHub Action.** `actions/token` in this repository (composite) fetches the
  OIDC JWT, exchanges it, masks the token and exports it. No OIDC available
  (e.g. fork PRs): warn and leave the remote cache off. 401/403: fail the step.
  Server unreachable or 503: warn and leave the remote cache off. Published
  only under immutable `vX.Y.Z` tags; docs pin it by commit SHA.
- **Expiry.** Scoped tiers accumulate; operators expire them with one S3
  lifecycle rule on `scoped/`. The `trusted/` prefix is never in that rule.
- **Observability.** Request logs record tier, provider, repository ID and
  namespace. `nx_cache_requests_total` gains a `tier` label (`static` |
  `trusted` | `scoped`; always `static` when exchange is off), and
  `nx_cache_token_exchanges_total{provider,result}` counts exchanges
  (`issued` | `refused` | `invalid` | `unavailable`). Never label by repository
  or namespace.

## Considered options

- **One shared untrusted token and prefix.** Protects the trusted tier, but any
  PR can poison another PR's results (e.g. fake passing tests). Rejected.
- **Namespace in the server URL path.** Nx preserves a path prefix, but the CI
  job chooses it, so it cannot be authority. Rejected.
- **Static per-PR tokens.** PR jobs can read any secret given to them. Rejected.
- **Static write tokens mapped to repositories.** Works without OIDC, but every
  repository needs a long-lived write secret kept away from its PR jobs, and
  rotation is per repository. Rejected for repository tiers; static tokens
  remain for the static tier.
- **Repository tiers also read the static tier.** Warm start after enabling,
  but any static write token could poison every repository. Rejected.
- **Merge queue and branch pushes refused.** Simpler, but loses reuse for those
  runs. Rejected.
- **Merge queue reads its PR's namespace.** The merge queue exists to verify
  approved code; reading output the PR author could have uploaded defeats it.
  Rejected.
