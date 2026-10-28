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
- `e2e` — real Nx 23.3 builds on both backends (Docker + Node)
- `test-s3` — optional atomic-write contract against a real bucket
- `audit` (govulncheck), `deadcode`, `check` (full local gate)
- `dev` — Garage via `compose.yaml` + server with `.env.local`
- `docker` — build the image

## Architecture

- `cmd/nx-remote-cache` — reads and validates env config (exit 1 on error),
  builds the S3 client, optional TLS, `http.Server` (ReadHeaderTimeout 10s,
  IdleTimeout 120s), graceful shutdown on SIGTERM/SIGINT (25s drain).
- `internal/server` — `New(Config) (http.Handler, error)`. Stdlib routing:
  `GET /health`, `GET /ready` (HeadBucket, 2s), `PUT`/`GET /v1/cache/{hash}`.
  Bearer auth with constant-time compare before reading body or touching
  storage; read-only token may only GET. Uploads stream with Content-Length,
  HeadObject then PutObject with `If-None-Match: *` (dropped per process once
  storage reports it unsupported). Storage failures: fail-open (default) →
  GET 404 / PUT 403; `NX_CACHE_FAIL_OPEN=false` → 503. Plain-text bodies.
- `internal` must not import `cmd`.

## Testing

- Unit: package `server_test` exercises the handler against an httptest fake S3.
- `//go:build integration`: testcontainers-go via `internal/s3test`. Feature
  tests run on Garage (ignores `If-None-Match`) and versitygw (enforces it);
  the 20-round atomic-write race runs on versitygw only.
- `//go:build e2e` (`e2e/`): Nx 23.3 builds on both backends: miss + upload,
  read-only remote hit, rebuild during storage outage.
- `//go:build s3contract`: env-configured contract against real S3.

## Environment Variables

Required: `NX_CACHE_ACCESS_TOKEN`. Optional: `NX_CACHE_READ_ONLY_TOKEN`,
`PORT` (3000), `AWS_REGION` (us-east-1), `S3_BUCKET_NAME` (nx-cloud),
`S3_ENDPOINT_URL`, `AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY` (both or
neither; neither → default credential chain), `NX_CACHE_FAIL_OPEN` (true),
`TLS_CERT_PATH` + `TLS_KEY_PATH` (both or neither). See README for rules.

## CI

`.github/workflows/main.yml` is dormant during the rewrite: `workflow_dispatch`
only, and the workflow is disabled in GitHub. Jobs (via jdx/mise-action):
check, integration, e2e, cog check on PR commits, helm lint, then multi-arch
distroless image and chart publish to GHCR. Restore push/PR/release triggers
and add Dependabot (gomod, docker, github-actions) once publishing is set up.

## Conventions

- Stdlib `testing` only (no testify).
- Logging with `log/slog`.
- Conventional Commits, enforced by cocogitto.
- `.golangci.yml` is the source of truth for lint and formatting rules.
