# nx-remote-cache

A small Go server implementing the
[Nx self-hosted remote cache specification](https://nx.dev/docs/kb/self-hosted-caching),
backed by S3 or any S3-compatible object store. A copy of the Nx OpenAPI spec
lives in [`docs/nx-remote-cache.openapi.json`](docs/nx-remote-cache.openapi.json)
(source: <https://nx.dev/docs/kb/self-hosted-caching>).

## Features

- Implements `PUT`/`GET /v1/cache/{hash}` from the Nx spec; artifacts are
  immutable (existing hashes return 409).
- Streaming uploads and downloads — artifacts are never buffered in memory or
  on disk.
- Conditional writes (`If-None-Match: *`) where the storage supports them, to
  stop concurrent overwrites.
- Optional read-only token for untrusted CI jobs.
- Graceful degradation during storage outages so Nx keeps building.
- `/health` liveness and `/ready` storage-readiness endpoints.
- Static AWS keys or the AWS default credential chain (IRSA, instance profile).
- Optional HTTPS; distroless multi-arch image; Helm chart.

## Configuration

All configuration comes from environment variables, validated once at startup.
Invalid configuration prints an error to stderr and exits with status 1. See
[`.env.example`](.env.example).

| Variable | Required | Default | Rules |
| --- | --- | --- | --- |
| `NX_CACHE_ACCESS_TOKEN` | yes | — | Trusted read/write token. Nonempty, no whitespace. |
| `NX_CACHE_READ_ONLY_TOKEN` | no | unset | Download-only token. If set (even empty) it must be nonempty, contain no whitespace, and differ from `NX_CACHE_ACCESS_TOKEN`. |
| `PORT` | no | `3000` | Listen port. |
| `AWS_REGION` | no | `us-east-1` | |
| `S3_BUCKET_NAME` | no | `nx-cloud` | |
| `S3_ENDPOINT_URL` | no | unset (AWS S3) | Custom S3-compatible endpoint. Requests always use path-style addressing. |
| `AWS_ACCESS_KEY_ID` | no | unset | Set both AWS keys or neither. Neither → AWS SDK default credential chain. |
| `AWS_SECRET_ACCESS_KEY` | no | unset | See above. |
| `NX_CACHE_FAIL_OPEN` | no | `true` | `true` or `false`; anything else is an error. See [Cache outages](#cache-outages). |
| `TLS_CERT_PATH` | no | unset | PEM certificate. Set both TLS paths or neither; both → HTTPS. |
| `TLS_KEY_PATH` | no | unset | PEM private key. |

## API

| Route | Auth | Responses |
| --- | --- | --- |
| `GET /health` | none | 200 `OK` (process liveness; never touches storage) |
| `GET /ready` | none | 200 `OK`, or 503 `Storage unavailable` when `HeadBucket` fails within 2s |
| `PUT /v1/cache/{hash}` | access token | 200, 401, 403, 409, 411, 500, 503 |
| `GET /v1/cache/{hash}` | either token | 200 (`application/octet-stream`), 401, 404, 503 |

Cache routes require `Authorization: Bearer <token>`; anything else returns 401
`Missing or invalid authentication token`. Response bodies are plain text.

## Running

### Docker

```bash
docker run -p 3000:3000 \
  -e NX_CACHE_ACCESS_TOKEN=your-secure-token \
  -e AWS_REGION=us-east-1 \
  -e AWS_ACCESS_KEY_ID=your-access-key \
  -e AWS_SECRET_ACCESS_KEY=your-secret-key \
  -e S3_BUCKET_NAME=your-bucket \
  ghcr.io/the-inconvenience-store/nx-remote-cache:0.1.0
```

Add `-e S3_ENDPOINT_URL=...` for S3-compatible storage. The image is distroless
(static, nonroot) and listens on port 3000.

### Helm

```bash
helm install nx-cache oci://ghcr.io/the-inconvenience-store/charts/nx-remote-cache \
  --version 0.1.0 \
  --set secrets.nxCacheAccessToken=your-secure-token \
  --set config.s3.bucketName=your-bucket
```

See the [chart README](charts/nx-remote-cache/README.md) for existing Secrets,
IRSA, read-only tokens, readiness, fail-open and TLS.

### From source

Requires [mise](https://mise.jdx.dev) and Docker.

```bash
mise run bootstrap   # install pinned tools, Go modules, git hooks
mise run dev         # start Garage via compose.yaml and run the server with .env.local
```

## Usage with Nx

Set these variables wherever Nx runs:

```bash
NX_SELF_HOSTED_REMOTE_CACHE_SERVER=http://your-server:3000
NX_SELF_HOSTED_REMOTE_CACHE_ACCESS_TOKEN=your-secure-token
```

### Trusted writers and read-only clients

`NX_CACHE_ACCESS_TOKEN` is the trusted read/write credential.
`NX_CACHE_READ_ONLY_TOKEN` optionally grants downloads only. Both credentials
use the same Nx client variable, `NX_SELF_HOSTED_REMOTE_CACHE_ACCESS_TOKEN`;
choose its value according to the job's trust level.

Give the read-write credential only to protected-branch jobs that do not
execute untrusted PR code. Give PR jobs the read-only credential; this prevents
cache poisoning via compromised pull requests
([CREEP / CVE-2025-36852](https://nx.dev/blog/cve-2025-36852-critical-cache-poisoning-vulnerability-creep)).
Read-only uploads return HTTP 403 `Read-only credentials cannot upload
artifacts`; missing or invalid credentials return HTTP 401. Authentication
happens before the body is read or storage is contacted. Health and readiness
checks remain unauthenticated.
Tokens are compared in constant time. Invalid token configuration prevents
startup; omit the optional read-only setting rather than setting it to an empty
string. Use HTTPS between clients and the server, whether TLS terminates here or
at a trusted proxy.

For Helm, set `secrets.nxCacheReadOnlyToken`, or add the optional
`nx-cache-read-only-token` key to the existing Secret.

### Nx version and TLS

Use Nx 22.7.7 or later, or Nx 23.0.2 or later; earlier affected versions have a
cache poisoning vulnerability ([GHSA-vp3h-ghgh-jr7g](https://github.com/nrwl/nx/security/advisories/GHSA-vp3h-ghgh-jr7g)).

For a private CA, configure the Nx native TLS client with `SSL_CERT_FILE` or
`SSL_CERT_DIR`; `NODE_EXTRA_CA_CERTS` does not affect this client
([rustls-native-certs](https://github.com/rustls/rustls-native-certs/blob/v/0.8.3/src/lib.rs#L52-L53)).
Set `NX_SELF_HOSTED_REMOTE_CACHE_SERVER` to the server origin only, without a
path ([Nx self-hosted cache specification](https://nx.dev/docs/kb/self-hosted-caching)).

When the server is configured with `NX_CACHE_FAIL_OPEN=false`, a failed store
can cause Nx to retry up to 6 times, sending the full artifact on each attempt
([Nx retry implementation](https://github.com/nrwl/nx/blob/bf2f593d45d1f8a58458b2226326c3087544df2b/packages/nx/src/tasks-runner/cache.ts#L712-L734)).

## S3 operator setup

The cache does not implement least-recently-used eviction: lifecycle expiration
removes objects by age. On a dedicated cache bucket, configure an expiration
rule, for example, to expire current versions after 90 days and noncurrent
versions after 30 days
([S3 lifecycle expiration](https://docs.aws.amazon.com/AmazonS3/latest/userguide/lifecycle-expire-general-considerations.html)):

```json
{
  "Rules": [
    {
      "ID": "ExpireNxCache",
      "Status": "Enabled",
      "Filter": { "Prefix": "" },
      "Expiration": { "Days": 90 },
      "NoncurrentVersionExpiration": { "NoncurrentDays": 30 }
    }
  ]
}
```

Grant the server only the following minimum bucket and object permissions; no
delete permission is needed. `HeadObject` requires `s3:ListBucket` for missing
keys: without it, S3 returns 403 rather than 404, so the server's default
fail-open behavior silently stores nothing
([HeadObject permissions](https://docs.aws.amazon.com/AmazonS3/latest/API/API_HeadObject.html)):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject"],
      "Resource": "arn:aws:s3:::YOUR_BUCKET/*"
    },
    {
      "Effect": "Allow",
      "Action": "s3:ListBucket",
      "Resource": "arn:aws:s3:::YOUR_BUCKET"
    }
  ]
}
```

To prevent overwrites, require the `If-None-Match` condition on object PUTs
using the AWS `s3:if-none-match` condition key
([enforce conditional writes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes-enforce.html)):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "DenyPutWithoutIfNoneMatch",
      "Effect": "Deny",
      "Principal": "*",
      "Action": "s3:PutObject",
      "Resource": "arn:aws:s3:::YOUR_BUCKET/*",
      "Condition": {
        "Null": { "s3:if-none-match": "true" }
      }
    }
  ]
}
```

## Streaming uploads

Upload bodies stream directly to S3; the server does not buffer the complete
artifact in memory or spool it to disk. `Content-Length` is required (411
otherwise) and is forwarded to S3. Uploads are sent with an unsigned payload,
and request checksums are added only when the storage requires them, so the
upload is an ordinary length-delimited S3 `PutObject`. Each upload is a single
S3 PUT, so the provider's single-PUT size limit applies.

Request bodies are one-shot: the server makes exactly one storage attempt and
never replays a body. A client disconnect or body-read error aborts the storage
request, nothing is stored, and the server returns 500. A retry requires a fresh
client request. Memory use scales with concurrent stream buffers, not artifact
size.

## Upload compatibility

Uploads first check for an existing object with `HeadObject` (409 if present),
then send `PutObject` with `If-None-Match: *` so concurrent writers cannot
overwrite each other on storage that supports conditional writes. A failed
precondition returns 409 `Cannot override an existing record`; transient
conditional-write conflicts are treated as storage failures.

If the storage explicitly reports that conditional writes are unsupported, that
upload returns 403 `Cache storage unavailable; artifact not stored` (Nx treats
it as non-fatal), a warning is logged, and later uploads in the same process are
sent without the condition. The `HeadObject` check is kept. No configuration is
needed. Authentication, checksum, network, and unrelated storage errors remain
errors. See
[S3 conditional-write semantics](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html).

Conditional writes are verified by the integration suite, which races concurrent
writers against [versitygw](https://github.com/versity/versitygw), an S3
gateway that enforces `If-None-Match`, and requires exactly one winner every
round. To measure your own backend's atomic-write capability, run the optional
contract test against a disposable bucket:

```bash
AWS_REGION=your-region \
AWS_ACCESS_KEY_ID=your-access-key \
AWS_SECRET_ACCESS_KEY=your-secret-key \
S3_BUCKET_NAME=your-test-bucket \
S3_ENDPOINT_URL=https://your-s3-endpoint \
mise run test-s3
```

It races concurrent writers, verifies exactly one succeeds, and checks later
writes cannot replace its bytes. Normal operation does not depend on it passing.
Use only a test bucket; the test writes, reads and deletes its own objects.

## Cache outages

Nx 23.3's self-hosted cache client treats only `GET` 404 as a cache miss, and
only `PUT` 403 and 409 as non-fatal. Every other response, including 5xx and
connection failures, fails the Nx run. By default (`NX_CACHE_FAIL_OPEN=true`),
the server degrades when storage fails so CI keeps building:

- `GET` returns 404, so Nx rebuilds the task. This covers S3 errors, throttling,
  credential and permission errors, a missing bucket, unreachable storage, and
  no response headers within 5 seconds.
- `PUT` returns 403 `Cache storage unavailable; artifact not stored`, so Nx
  continues without caching the artifact.
- Each degraded response logs `msg="Storage unavailable"` with `method`, `hash`,
  and the underlying `error`.
- A bad Nx access token still returns 401.

Set `NX_CACHE_FAIL_OPEN=false` to return 503 `Cache storage unavailable`
instead, failing the Nx run on storage errors.

`/ready` checks bucket access with `HeadBucket` (2-second timeout) and returns
503 `Storage unavailable` on failure; details appear only in server logs. It
requires no token. `/health` remains a process-only liveness check. The Helm
chart's readiness probe uses `/ready`, so pods that lose storage access leave
the Service without restarting. `HeadBucket` needs `s3:ListBucket`, which the
upload duplicate check already requires.

The server cannot protect against these failures:

- A storage failure after a download has started streaming truncates the
  response, and Nx fails the run.
- If the cache server itself is unreachable, Nx fails the run. Run more than one
  replica behind the readiness probe.
- Nx has no setting that makes cache errors non-fatal. `skipRemoteCache`
  disables the remote cache entirely.

## Development

[mise](https://mise.jdx.dev) pins every tool (Go, golangci-lint, lefthook,
hadolint, cocogitto, actionlint, helm, typos, govulncheck, deadcode) and runs
all tasks.

| Task | What it does |
| --- | --- |
| `mise run bootstrap` | Install pinned tools, Go modules, and git hooks |
| `mise run fmt` | Format Go code |
| `mise run fmt-check` | Fail if Go code is not formatted |
| `mise run lint` | golangci-lint, hadolint, actionlint, typos, helm lint |
| `mise run test` | Unit tests (no Docker) |
| `mise run test-integration` | Feature suite against Garage and versitygw, plus atomic-write races on versitygw (needs Docker) |
| `mise run e2e` | Real Nx 23.3 builds against the server on Garage and versitygw (needs Docker and Node) |
| `mise run test-s3` | Optional atomic-write contract against a real bucket |
| `mise run audit` | govulncheck |
| `mise run deadcode` | Report functions unreachable from main |
| `mise run check` | Full local gate: fmt-check, lint, test, audit, deadcode |
| `mise run dev` | Run Garage via compose and the server against it |
| `mise run docker` | Build the container image |

`mise run bootstrap` installs [lefthook](https://lefthook.dev) hooks:
pre-commit formats staged Go files and runs the linters on matching staged
files, commit-msg verifies the message, and pre-push runs the unit tests.

Commits follow [Conventional Commits](https://www.conventionalcommits.org),
checked by [cocogitto](https://docs.cocogitto.io) locally and in CI.

Tests: unit tests use the standard library `testing` package with a fake S3 and
need no Docker. Integration tests (`integration` build tag) and Nx end-to-end
tests (`e2e` tag) start real S3 servers with testcontainers and need Docker; e2e
also needs Node to run Nx 23.3. Every feature test runs against both Garage
(accepts but does not enforce `If-None-Match`) and versitygw (enforces it); the
atomic-write race runs on versitygw. The S3 contract test (`s3contract` tag) is
optional and runs against a bucket you configure.

Tool pins in `mise.toml` are updated with `mise upgrade --bump`. Dependabot
updates Go modules, Docker base images and GitHub Actions weekly.

Releases: publishing a GitHub release `vX.Y.Z` builds and pushes the image
(`X.Y.Z`, `X.Y`, `X`, `latest`) and the Helm chart (`X.Y.Z`) to GHCR. Every push
to `main` also pushes `main` and `sha-<short>` image tags.

## Acknowledgements

nx-remote-cache is a Go rewrite of
[nx-cache-server](https://github.com/IKatsuba/nx-cache-server) by Igor Katsuba.

## License

[MIT](LICENSE)
