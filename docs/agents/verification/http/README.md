# HTTP/HTTPS cache service

## Surface

`cmd/nx-remote-cache` serves `/health`, `/ready`, and authenticated
`PUT`/`GET /v1/cache/{hash}`. It owns startup validation, TLS, graceful shutdown,
and the Nx self-hosted cache contract. Consumers use HTTP or Nx 23.3.

## Launch

Run from the repository root with Python 3, mise's pinned Go, and a running
Docker daemon. First check prerequisites:

```sh
mise exec -- go version
docker info --format '{{.ServerVersion}}'
python3 --version
```

Run the complete isolated Launch → Doctor → Drive → Evidence → Cleanup route:

```sh
python3 docs/agents/verification/http/drive.py --evidence /tmp/nx-cache-verification-http
```

The evidence directory must not already exist; choose a new absolute path for
later runs. The driver builds `./cmd/nx-remote-cache`, creates a uniquely named
Garage v2.5.0 container using `dev/garage.toml`, allocates a loopback storage
port, `PORT=0` and a free `METRICS_PORT` for the server, and waits up to 60
seconds for readiness. The server reaches Garage through the driver's own
loopback TCP relay, which can hold storage traffic to fake a stall, with
`NX_CACHE_STALL_TIMEOUT=2s`. It generates ephemeral credentials in memory, sets
the bucket to `nx-cloud` and region to `garage`, and clears inherited
AWS/cache/TLS/metrics configuration.
It does not read `.env.local` or launch the shared development Compose project.
The script owns the server process handle and container name through teardown.

The shipping server listens on all interfaces; although storage binds only to
loopback, run this local fixture on a trusted machine/network. Generated cache
tokens protect cache requests; health/readiness are intentionally public.

## Doctor

Before any cache drive, the helper confirms its newly built process is alive,
reads that process's listening address from its own log, and requires
`GET /ready` to return exactly `200 OK`. It then checks `GET /health` returns
exactly `200 OK`. Do not treat liveness alone as storage readiness. Failed
Doctor produces `INCONCLUSIVE`, a nonzero exit, and cleanup rather than a pass.

## Drive

The helper sends real HTTP requests with length-delimited binary payloads:
miss → upload → byte-exact download → duplicate rejection → read-only download,
a metrics scrape matching those requests and a 404 for `/metrics` on the cache
port, a download and an upload that the relay stalls mid-body (stopped, logged
and counted; see [stalls.md](features/stalls.md)), unauthenticated/invalid-token
rejection, read-only upload denial and absence, server restart with
`METRICS_PORT=0` (metrics port refused) and byte-exact download, then storage
shutdown and outage checks. It also proves that startup with no access token or
with `NX_CACHE_STALL_TIMEOUT=0s` exits 1. Every status is asserted; fixture
bytes are compared and their SHA-256 digests recorded.

Run the same route over verified TLS, trusting only the repository test CA:

```sh
python3 docs/agents/verification/http/drive.py --tls --evidence /tmp/nx-cache-verification-tls
```

Exercise fail-closed storage responses:

```sh
python3 docs/agents/verification/http/drive.py --fail-closed --evidence /tmp/nx-cache-verification-closed
```

Flags can be combined. TLS uses `cmd/nx-remote-cache/testdata/{ca,cert,key}.pem`,
whose server certificate covers localhost; these are public test fixtures,
not deployable credentials. The default route uses HTTP and fail-open mode.

For real Nx builds, use the existing harness as described in
[nx-consumer.md](features/nx-consumer.md), not a unit-test substitute.

## Evidence

The supplied evidence directory survives cleanup and contains:
- `transcript.txt`: action, method/path, status, body size/digest, durability,
  Doctor and cleanup outcomes, and the final verdict.
- `server.log`: server startup, request status, storage failure, and shutdown logs.
- `result.json`: verdict plus TLS/fail-closed mode.

Tokens, storage credentials, authentication headers, and process environments
are not printed or saved. Do not enable shell tracing, HTTP header tracing, or
Docker inspection dumps containing environment variables. Payloads are synthetic.
`VERIFIED` requires every assertion and cleanup to succeed; failed assertions
produce `NOT VERIFIED`; launch/observation/cleanup failures are `INCONCLUSIVE`.
The driver exits nonzero for either non-pass verdict.

## Cleanup

The helper sends SIGTERM to its server, waits up to 30 seconds, removes only its
named container and anonymous volumes with `docker rm --force --volumes`, and
removes its temporary binary/log workspace after copying logs to evidence.
It confirms the child exited, the container is absent, and scratch is gone.
Cleanup runs on ordinary Python exceptions and failed assertions.

If forcibly interrupted or killed, use the exact container name and scratch
path from the failure output/process inspection to remove only that run's
resources. Do not use Docker prune, the shared Compose teardown, or broad
process-name kills. Preserve the evidence directory. A forced termination that
prevents cleanup is not a verified run.

## Isolation

Concurrent runs use different server/storage ports, container names, tokens,
binary workspaces, and storage. Give each run a distinct evidence directory.
The outage step stops only the run's Garage. It never modifies a real bucket,
shared development volume, or Kubernetes resource. Go's module/build caches and
Docker's image cache are shared caches, not artifact storage fixtures.

## Feature map

[Feature index](features/README.md). Every user-facing feature belongs in this
map; add or update its executable proof when the public contract changes.
