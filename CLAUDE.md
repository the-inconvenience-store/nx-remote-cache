# CLAUDE.md

Guidance for Claude Code working in this repository.

## Project Overview

Go HTTP server implementing the
[Nx self-hosted remote cache specification](https://nx.dev/docs/kb/self-hosted-caching)
(spec copy: `docs/nx-remote-cache.openapi.json`), backed by S3 or S3-compatible
storage via aws-sdk-go-v2. Module `github.com/the-inconvenience-store/nx-remote-cache`.
Image `ghcr.io/the-inconvenience-store/nx-remote-cache`; Helm chart in
`charts/nx-remote-cache`.

## Commands

mise pins all tools and runs tasks (`mise run <task>`):

- `bootstrap` — install tools, Go modules, lefthook hooks
- `fmt` / `fmt-check` — golangci-lint formatters
- `lint` — golangci-lint, hadolint, actionlint, typos, helm lint
- `test` — unit tests (no Docker)
- `test-integration` — Garage + versitygw via testcontainers (Docker)
- `e2e` — real Nx 23.3 and 24.0 builds on both backends (Docker + Node)
- `audit` (govulncheck), `deadcode`, `check` (full local gate)
- `dev` — Garage via `compose.yaml` + server with `.env.local`
- `docker` — build the image

## Architecture

- `cmd/nx-remote-cache` — reads and validates env config (exit 1 on error),
  builds the S3 client, optional TLS, `http.Server` (ReadHeaderTimeout 10s,
  IdleTimeout 120s), graceful shutdown on SIGTERM/SIGINT (25s drain). Unless
  `METRICS_PORT=0`, a second plain-HTTP server serves only `GET /metrics` from
  a dedicated Prometheus registry (plus Go/process collectors) and shuts down
  with the cache server.
- `internal/server` — `New(Config) (http.Handler, error)`. Stdlib routing:
  `GET /health`, `GET /ready` (HeadBucket, 2s), `PUT`/`GET /v1/cache/{hash}`.
  Bearer auth with constant-time compare against every listed token before
  reading body or touching storage; read-only tokens may only GET. Uploads
  stream with Content-Length, HeadObject then PutObject with `If-None-Match: *`
  and metadata identifying the uploader by the first 12 hex characters of the
  token's SHA-256 plus the RFC 3339 UTC upload time. (The condition is dropped
  per process once storage reports it unsupported: one warning at the switch,
  then `nx_cache_unconditional_uploads_total` counts each unconditional PUT.)
  Storage failures:
  fail-open (default) → GET 404 / PUT 403; `NX_CACHE_FAIL_OPEN=false` → 503.
  Plain-text bodies. S3 transport keeps up to 256 idle connections (SDK
  default 10 per host) so Nx's burst of parallel GETs reuses connections;
  SDK timeouts and retries otherwise unchanged.
- Stall timeout (`internal/server/stall.go`): a `transfer` wraps the body the
  server receives (S3 GET body, client PUT body). Each `Read` records progress
  on entry (previous chunk passed on) and on bytes returned; a timer re-arms
  from the latest progress and, after `NX_CACHE_STALL_TIMEOUT` idle, cancels
  the S3 request context and expires the client connection deadline via
  `http.ResponseController`. The handler attributes the stall to `storage` or
  `client` by whether the body read failed. No total-time limit.
- Cache request checks (`internal/server`): each `//` in the path becomes `/`
  before routing (no 307); after auth the hash must match
  `^[0-9A-Za-z_-]{1,128}$` (else 400); an upload whose Content-Length exceeds
  `NX_CACHE_MAX_ARTIFACT_BYTES` gets 403 before its body is read. Each cache
  request records one `outcome` (`requestlog.go`) on its request log line with
  `hash`, `bytes`, `access`, `tier` (plus `repository`/`namespace` for
  repository tokens); never token text.
- Token exchange (ADR 0001; `exchange.go`, `github.go`, `issued.go`,
  `tier.go`), on only with `Config.TokenExchange`: `POST /v1/token` verifies a
  CI OIDC JWT (RS256 via go-oidc; unverified `iss`/`aud` pre-check; issuer keys
  fetched lazily, at most every 30s) and returns an HMAC-signed `nxi1.` token
  bound to one repository and the trusted tier or one namespace. The GitHub
  `provider` maps by `event_name` (push → trusted if branch matches and
  `ref_protected`, else `branch/…`; `pull_request` → `pr/N`; `merge_group` →
  `queue/…`; else 403). Keys: static `<hash>`,
  `trusted/<provider>/<repo_id>/<hash>`, `scoped/<provider>/<repo_id>/<ns>/<hash>`;
  a `credential` lists read locations (scoped: trusted first) and one write
  key. Issued tokens re-check repository admission on every request.
- Metrics (`internal/server/metrics.go`): declare every collector once in
  `newMetrics`; `New` registers them all with `Config.Metrics` (nil →
  unregistered) only after it succeeds.
  `nx_cache_requests_total{method,outcome,tier}` counts cache requests;
  `nx_cache_token_exchanges_total{provider,result}` counts exchanges;
  `nx_cache_stalled_transfers_total{method,side}` counts stopped transfers;
  never label by hash, repository or namespace. Byte counters count bytes
  actually read/written
  (`receivedBody`, download copy), not declared lengths. S3 call durations come
  from one SDK Initialize middleware (`timeS3Calls`) labelled by operation name.
- `internal` must not import `cmd`.

## Testing

- Unit: package `server_test` exercises the handler against an httptest fake S3
  and a fake OIDC issuer (`internal/oidctest`).
- `//go:build integration`: testcontainers-go via `internal/s3test`. Feature
  tests run on Garage (ignores `If-None-Match`) and versitygw (enforces it);
  the 20-round atomic-write race runs on versitygw only. A Garage-only test
  counts S3 connections through a TCP proxy across two 200-GET bursts.
- `//go:build e2e` (`e2e/`): Nx 23.3 and 24.0 builds on both backends: miss +
  upload, read-only remote hit, rebuild during storage outage; and issued
  tokens through PR, trusted and repository read-only tiers.
- `//go:build s3contract`: env-configured contract against real S3.

## Environment Variables

Required: `NX_CACHE_ACCESS_TOKEN` (optional with token exchange on). Optional: `NX_CACHE_READ_ONLY_TOKEN` (both
comma-separated token lists; empty item, duplicate or overlap → startup error),
`PORT` (3000), `METRICS_PORT` (9090; 0 = off), `AWS_REGION` (us-east-1), `S3_BUCKET_NAME` (nx-cloud),
`S3_ENDPOINT_URL`, `S3_FORCE_PATH_STYLE` (`true`|`false`; unset → path-style
only with an endpoint), `AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY` (both or
neither; neither → default credential chain), `NX_CACHE_FAIL_OPEN` (true),
`TLS_CERT_PATH` + `TLS_KEY_PATH` (both or neither), `NX_CACHE_MAX_ARTIFACT_BYTES`
(0 = no limit), `NX_CACHE_STALL_TIMEOUT` (60s; Go duration > 0). Token exchange
(`cmd/nx-remote-cache/exchange.go`; any of these without the first → startup
error): `NX_CACHE_OIDC_PROVIDERS` (`github`), `NX_CACHE_OIDC_AUDIENCE`,
`NX_CACHE_ISSUED_TOKEN_KEYS` (base64, ≥32 bytes), `NX_CACHE_ISSUED_TOKEN_TTL`
(6h; ≤120h), `NX_CACHE_GITHUB_ISSUER`, `NX_CACHE_GITHUB_OWNER_IDS` /
`NX_CACHE_GITHUB_REPOSITORY_IDS` (numeric), `NX_CACHE_GITHUB_TRUSTED_BRANCHES`
(main), `NX_CACHE_GITHUB_READ_ONLY_TOKENS` (`<repo_id>:<token>`). See README
for rules.

## CI

`.github/workflows/main.yml` runs on pushes and PRs to `main`, GitHub releases,
and manual dispatch. Jobs (via jdx/mise-action): check, integration, e2e, cog
check on PR commits, helm lint, a live token exchange (push and PR only:
the server in the job plus `actions/token`, the composite action users pin),
then (not on PRs) a multi-arch distroless image
push to GHCR tagged `main` and `sha-<short>`. A published `vX.Y.Z` release also
tags the image `X.Y.Z`, `X.Y`, `X`, `latest` and pushes the chart at that
version. Dependabot updates Go modules, the Dockerfile and actions weekly.

## Conventions

- Stdlib `testing` only (no testify).
- Logging with `log/slog`.
- Conventional Commits, enforced by cocogitto.
- `.golangci.yml` is the source of truth for lint and formatting rules.

## Agent skills

### Issue tracker

Issues live in GitHub Issues for `the-inconvenience-store/nx-remote-cache`. See `docs/agents/issue-tracker.md`.

### Triage labels

Use the five canonical triage labels. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: root `GLOSSARY.md` and `docs/adr/`. See `docs/agents/domain.md`.

### Verification

Follow `docs/agents/verification/README.md` to launch and doctor each real user-facing surface, drive its public path, capture evidence, and clean up.
