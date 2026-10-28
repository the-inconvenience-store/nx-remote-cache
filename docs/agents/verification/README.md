# Real-surface verification

## Surfaces

- [HTTP/HTTPS cache service](http/README.md): choose for Nx artifact storage,
  authentication, readiness, outage behavior, TLS, and startup configuration.
  This is the only application surface. Source, Docker, and Helm deploy the
  same service; the Nx consumer harness drives that service rather than a
  second application owned by this repository.

Start with the surface's Launch and Doctor before driving a mapped feature.
Every user-facing feature belongs in its [feature map](http/features/README.md).
Use the verification skill's Maintain route when commands or surfaces change.

## Scope

The direct driver builds the current checkout and uses real Garage, not a fake
S3 service. The existing Nx harness covers actual consumer builds and both
Garage and versitygw. A direct HTTP run does not prove Nx compatibility,
conditional-write atomicity on another backend, image packaging, a Kubernetes
deployment, IRSA, or production S3 permissions. Run the corresponding existing
consumer/integration/contract harness or deploy to a disposable environment
when those are the changed paths. See the surface map for exact commands.

Guardrail setup was intentionally skipped; existing mise tasks, CI, hooks,
formatters, and dependency/tool pins remain authoritative and unchanged.

## Proven setup runs

On 2026-10-28, the documented driver completed with `VERIFIED` in all three
modes: default HTTP, CA/hostname-verified HTTPS, and fail-closed HTTP.
Each run proved binary artifact persistence across a server restart,
immutability, credential boundaries, readiness/liveness separation, outage
responses, missing-token startup rejection, and removal of its own resources.

Local evidence remains outside repository cleanup:
- `/tmp/nx-cache-verification-http/`
- `/tmp/nx-cache-verification-tls/`
- `/tmp/nx-cache-verification-closed/`

Each directory contains `transcript.txt`, `server.log`, and `result.json`.
These temporary evidence paths are machine-local, not portable fixtures.
The Nx consumer, backend race, image, Kubernetes, and real-bucket recipes were
not run as part of this setup; no verification claim is made for those paths.
