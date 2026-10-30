# Repository tiers and token exchange

## User outcome

With token exchange on, a pull request reuses its own build results, the
trusted branch never reads them, other pull requests and developers read the
trusted tier, and repositories sharing the server stay apart.

## How to reach it

Locally, a fake GitHub issuer (`internal/oidctest`) stands in for GitHub:
the server binary is started with `NX_CACHE_GITHUB_ISSUER` pointing at it.
Against real GitHub, the `token-exchange` job in
`.github/workflows/main.yml` starts the server inside a workflow run and uses
`actions/token` with the job's real OIDC token.

## How to drive it

```sh
NX_VERSION=24.0.2 mise run e2e   # includes TestNxSharesResultsThroughRepositoryTiers
```

The e2e test runs real Nx builds with issued tokens on Garage and versitygw:
PR first build (miss, upload), PR rebuild (remote hit), trusted push build
(miss), another PR (remote hit from trusted), repository read-only token (remote
hit). For the action and real GitHub claims, push a branch or open a pull
request and read the `Live GitHub OIDC Token Exchange` job.

## Proof

Nx's `Cache: 0/1 hit` and `1/1 hit` lines at each step, and server request
logs with `tier=scoped` and `tier=trusted`. In CI, the job prints
`event=… tier=… namespace=…`, checks `pr/<number>` for pull requests (trusted
or `branch/main` for pushes to main, depending on its protection), and
round-trips an artifact with the issued token.

## Gotchas

Fork pull requests get no OIDC token: the action warns and the job passes
without a cache token. Release and `workflow_dispatch` runs are refused by
design, so the CI job does not run for them. The local fake issuer does not
prove GitHub's real claim formats; only the CI job does.
