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
- Optional per-PR caching and per-repository isolation: GitHub Actions jobs
  exchange their OIDC token for a cache token bound to one repository and one
  tier, so PR builds reuse their own results without poisoning trusted ones.
- Graceful degradation during storage outages so Nx keeps building.
- Stalled S3 transfers stop after a configurable time without bytes, so they
  cannot hang the Nx run.
- `/health` liveness and `/ready` storage-readiness endpoints.
- Prometheus metrics on a separate port.
- Static AWS keys or the AWS default credential chain (IRSA, instance profile).
- Optional HTTPS; distroless multi-arch image; Helm chart.

## Configuration

All configuration comes from environment variables, validated once at startup.
Invalid configuration prints an error to stderr and exits with status 1. See
[`.env.example`](.env.example).

| Variable | Required | Default | Rules |
| --- | --- | --- | --- |
| `NX_CACHE_ACCESS_TOKEN` | yes, unless token exchange is on | — | Trusted read/write tokens for the static tier: a comma-separated list. Each token is nonempty, without whitespace. With token exchange on, unset or empty means none. |
| `NX_CACHE_READ_ONLY_TOKEN` | no | unset | Download-only tokens for the static tier: a comma-separated list. If set (even empty), each token must be nonempty, contain no whitespace, and not appear in `NX_CACHE_ACCESS_TOKEN`. |
| `PORT` | no | `3000` | Listen port. |
| `METRICS_PORT` | no | `9090` | Port for Prometheus `/metrics` (plain HTTP); `0` turns metrics off. An integer from 0 to 65535; anything else, including empty, is an error. See [Metrics](#metrics). |
| `AWS_REGION` | no | `us-east-1` | |
| `S3_BUCKET_NAME` | no | `nx-cloud` | |
| `S3_ENDPOINT_URL` | no | unset (AWS S3) | Custom S3-compatible endpoint. |
| `S3_FORCE_PATH_STYLE` | no | unset | `true` (path-style) or `false` (virtual-hosted style); anything else, including empty, is an error. Unset → virtual-hosted style on AWS S3, path-style when `S3_ENDPOINT_URL` is set. On AWS, a bucket name containing a dot can fail TLS in virtual-hosted style; set `true` for such buckets. |
| `AWS_ACCESS_KEY_ID` | no | unset | Set both AWS keys or neither. Neither → AWS SDK default credential chain. |
| `AWS_SECRET_ACCESS_KEY` | no | unset | See above. |
| `NX_CACHE_FAIL_OPEN` | no | `true` | `true` or `false`; anything else is an error. See [Cache outages](#cache-outages). |
| `NX_CACHE_MAX_ARTIFACT_BYTES` | no | `0` (no limit) | Largest upload `Content-Length` in bytes; a whole number of 0 or more, anything else is an error. A larger upload gets 403 and is not stored. See [Streaming uploads](#streaming-uploads). |
| `NX_CACHE_STALL_TIMEOUT` | no | `60s` | Stop a transfer when none of its bytes move for this long. A [Go duration](https://pkg.go.dev/time#ParseDuration) greater than zero, such as `30s` or `2m`; anything else, including empty, is an error. See [Stalled transfers](#stalled-transfers). |
| `TLS_CERT_PATH` | no | unset | PEM certificate. Set both TLS paths or neither; both → HTTPS. |
| `TLS_KEY_PATH` | no | unset | PEM private key. |

Token exchange variables ([Per-PR and multi-repository caching](#per-pr-and-multi-repository-caching)).
Any of them set while `NX_CACHE_OIDC_PROVIDERS` is unset or empty is an error.

| Variable | Required | Default | Rules |
| --- | --- | --- | --- |
| `NX_CACHE_OIDC_PROVIDERS` | no | unset (off) | `github` turns token exchange on. |
| `NX_CACHE_OIDC_AUDIENCE` | with exchange | — | The `aud` CI tokens must carry; for example the server URL. |
| `NX_CACHE_ISSUED_TOKEN_KEYS` | with exchange | — | Comma-separated standard base64 keys of at least 32 bytes (`openssl rand -base64 32`), identical on every replica. The first signs issued tokens; all verify them. |
| `NX_CACHE_ISSUED_TOKEN_TTL` | no | `6h` | How long an issued token works; a Go duration greater than zero and at most `120h`. |
| `NX_CACHE_GITHUB_ISSUER` | no | `https://token.actions.githubusercontent.com` | Actions OIDC issuer; set it for GitHub Enterprise Server (`https://HOSTNAME/_services/token`) or GHE.com (`https://token.actions.SUBDOMAIN.ghe.com`). One issuer per deployment. |
| `NX_CACHE_GITHUB_OWNER_IDS` | one of these two | unset | Comma-separated numeric owner (user or organisation) IDs; every repository of these owners may use the cache. |
| `NX_CACHE_GITHUB_REPOSITORY_IDS` | one of these two | unset | Comma-separated numeric repository IDs. Names are never accepted: they can be renamed and reused. |
| `NX_CACHE_GITHUB_TRUSTED_BRANCHES` | no | `main` | Comma-separated [`path.Match`](https://pkg.go.dev/path#Match) patterns (`*` does not match `/`); a bare `*` is an error. |
| `NX_CACHE_GITHUB_READ_ONLY_TOKENS` | no | unset | Comma-separated `<repository_id>:<token>` pairs; each token reads that repository's trusted tier. Tokens must not repeat or appear in another token list. |

## API

| Route | Auth | Responses |
| --- | --- | --- |
| `GET /health` | none | 200 `OK` (process liveness; never touches storage) |
| `GET /ready` | none | 200 `OK`, or 503 `Storage unavailable` when `HeadBucket` fails within 2s |
| `PUT /v1/cache/{hash}` | write token | 200, 400, 401, 403, 409, 411, 500, 503 |
| `GET /v1/cache/{hash}` | any token | 200 (`application/octet-stream`), 400, 401, 404, 503 |
| `POST /v1/token` | CI OIDC token | 200 (the issued token as `text/plain`), 401, 403, 503; only with token exchange on |

Cache routes require `Authorization: Bearer <token>`; anything else returns 401
`Missing or invalid authentication token`. An issued token that expired returns
401 `Cache token expired; exchange a new CI token for it`, and one whose
repository is no longer admitted returns 401 `This repository is not allowed to
use this cache`; Nx shows these messages. A hash must match
`^[0-9A-Za-z_-]{1,128}$`; any other hash returns 400 `Invalid hash` without a
storage request. Response bodies are plain text.

## Running

### Docker

```bash
docker run -p 3000:3000 \
  -e NX_CACHE_ACCESS_TOKEN=your-secure-token \
  -e AWS_REGION=us-east-1 \
  -e AWS_ACCESS_KEY_ID=your-access-key \
  -e AWS_SECRET_ACCESS_KEY=your-secret-key \
  -e S3_BUCKET_NAME=your-bucket \
  ghcr.io/the-inconvenience-store/nx-remote-cache:0.3.0
```

Add `-e S3_ENDPOINT_URL=...` for S3-compatible storage. The image is distroless
(static, nonroot) and listens on port 3000.

### Helm

```bash
helm install nx-cache oci://ghcr.io/the-inconvenience-store/charts/nx-remote-cache \
  --version 0.3.0 \
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

A trailing slash in the server URL also works: the server changes each `//` in
the request path to `/` before routing, without a redirect. The server has no
base path setting; use an ingress to remove a path prefix.

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

Each stored artifact includes S3 metadata `nx-writer` (the first 12 hexadecimal
characters of the SHA-256 hash of the uploading bearer token) and
`nx-uploaded-at` (the upload time in RFC 3339 UTC). The PUT request log records
the same writer identifier; the token itself is never stored or logged.
Uploads with an issued token also record `nx-repository`, `nx-namespace`,
`nx-run-id` and `nx-actor`.

For Helm, set `secrets.nxCacheReadOnlyToken`, or add the optional
`nx-cache-read-only-token` key to the existing Secret.

### Per-PR and multi-repository caching

A read-only PR token stops cache poisoning, but PR builds then rebuild
everything. With token exchange on (`NX_CACHE_OIDC_PROVIDERS=github`), each
GitHub Actions job trades its OIDC token for an *issued token* bound to one
repository and one tier; Nx uses it like any other token. The design and its
rejected alternatives are in
[ADR 0001](docs/adr/0001-per-repository-tiers-via-ci-oidc.md).

| Tier | Storage key | Written by | Read by |
| --- | --- | --- | --- |
| Static | `<hash>` | `NX_CACHE_ACCESS_TOKEN` | static tokens |
| Trusted | `trusted/github/<repository_id>/<hash>` | pushes to a protected trusted branch | every token of that repository |
| Scoped | `scoped/github/<repository_id>/<namespace>/<hash>` | runs of that namespace | that namespace's tokens |

The server picks the namespace from the verified token's `event_name`, never
from the job's own settings:

| Event | Grant |
| --- | --- |
| `push` to a trusted branch that GitHub reports protected (`ref_protected`) | trusted tier |
| `push` to any other branch, or to an unprotected trusted branch | `branch/<escaped branch>` (a warning is logged for the latter) |
| `pull_request` (`refs/pull/<N>/merge`) | `pr/<N>` |
| `merge_group` (`refs/heads/gh-readonly-queue/...`) | `queue/<escaped branch>` |
| anything else, including `pull_request_target`, `workflow_run`, `issue_comment`, `workflow_dispatch`, tags | refused (403) |

A scoped token reads its repository's trusted tier first, then its own
namespace, and writes only its namespace. Repositories sharing the server never
read each other's artifacts, and repository tiers never read the static tier, so
turning token exchange on starts the trusted tier cold. Every request re-checks
that the token's repository is still admitted; there is no other revocation:
rotate `NX_CACHE_ISSUED_TOKEN_KEYS` to revoke every issued token. Startup and
`/ready` never contact GitHub; while GitHub is unreachable, `POST /v1/token`
returns 503 and issued tokens keep working.

In each workflow, add the action from this repository, pinned to a release
commit (only immutable `vX.Y.Z` tags are published), before any Nx step:

```yaml
permissions:
  contents: read
  id-token: write
steps:
  - uses: actions/checkout@v5
  - uses: the-inconvenience-store/nx-remote-cache/actions/token@bce5700ce85ad8b8cd08b0f5bab8f31d4a8ad242 # v0.3.0
    with:
      server-url: https://nx-cache.example.com
      audience: https://nx-cache.example.com # NX_CACHE_OIDC_AUDIENCE
  - run: npx nx affected -t build
```

The action exports `NX_SELF_HOSTED_REMOTE_CACHE_SERVER` and a masked
`NX_SELF_HOSTED_REMOTE_CACHE_ACCESS_TOKEN`, and outputs `tier` and
`namespace`. Do not set those two variables yourself. Without an OIDC token
(fork pull requests, or no `id-token: write`) or when the server is unreachable,
it warns and leaves the remote cache off; a 401 or 403 from the server fails the
step. Without the action, fetch the token from `ACTIONS_ID_TOKEN_REQUEST_URL`
with `audience=<NX_CACHE_OIDC_AUDIENCE>`, `POST` it as a bearer token to
`/v1/token`, mask the response body and export it.

Trust rests on each repository's branch protection or rulesets for its trusted
branches; a repository without them gets branch namespaces instead of trusted
writes. Keep any static `NX_CACHE_ACCESS_TOKEN` out of reach of PR jobs (a
GitHub environment restricted to protected branches, not a repository secret):
same-repository PR jobs can read repository secrets. The default 6h token
lifetime matches the GitHub-hosted job limit; raise `NX_CACHE_ISSUED_TOKEN_TTL`
(up to `120h`) for longer self-hosted runner jobs, because an expired token
fails the Nx run. Developers get a repository read-only token from
`NX_CACHE_GITHUB_READ_ONLY_TOKENS`. Find numeric IDs with
`gh api repos/OWNER/REPO --jq '.id, .owner.id'`.

### Rotating tokens

Both variables accept a comma-separated list, and every listed token works. To
rotate without downtime, add the new token (`old-token,new-token`), move the CI
jobs to it, then remove the old token. Spaces around each comma are removed.
The server does not start if an item is empty (`a,,b` or a trailing comma), a
token is listed twice, or a token is in both lists. A comma always separates
tokens, so a token cannot contain a comma.

With `helm --set`, escape each comma (`--set 'secrets.nxCacheAccessToken=old\,new'`)
or set the value in a values file.

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

With token exchange on, scoped tiers (one per pull request, merge queue entry
and branch) accumulate; expire them sooner with a second rule on the `scoped/`
prefix. When rules overlap, S3 applies the shorter expiration, so `trusted/`
and static artifacts keep the 90 days:

```json
{
  "ID": "ExpireNxScopedTiers",
  "Status": "Enabled",
  "Filter": { "Prefix": "scoped/" },
  "Expiration": { "Days": 14 },
  "NoncurrentVersionExpiration": { "NoncurrentDays": 1 }
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

The S3 client keeps up to 256 idle connections, so the burst of parallel
downloads at the start of an Nx run reuses storage connections instead of
opening and closing one per request.

Set `NX_CACHE_MAX_ARTIFACT_BYTES` to limit the artifact size. The server
compares the limit with `Content-Length` before it reads the body or contacts
storage. A larger upload returns 403 `Artifact exceeds the maximum size;
artifact not stored`, and nothing is stored. The status is 403, not 413,
because Nx continues the run after a 403 but stops it on 413. An upload exactly
at the limit is accepted. The default, `0`, sets no limit.

## Upload compatibility

Uploads first check for an existing object with `HeadObject` (409 if present),
then send `PutObject` with `If-None-Match: *` so concurrent writers cannot
overwrite each other on storage that supports conditional writes. A failed
precondition returns 409 `Cannot override an existing record`; transient
conditional-write conflicts are treated as storage failures.

If the storage explicitly reports that conditional writes are unsupported, that
upload returns 403 `Cache storage unavailable; artifact not stored` (Nx treats
it as non-fatal), and later uploads in the same process are sent without the
condition. The server keeps running but logs one warning at that switch
(`Storage does not support conditional writes; later uploads are sent without
If-None-Match and can overwrite each other`), and the
`nx_cache_unconditional_uploads_total` [metric](#metrics) counts each upload
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
  response, and Nx fails the run. This includes a download stopped by the
  [stall timeout](#stalled-transfers).
- If the cache server itself is unreachable, Nx fails the run. Run more than one
  replica behind the readiness probe.
- Nx has no setting that makes cache errors non-fatal. `skipRemoteCache`
  disables the remote cache entirely.

## Stalled transfers

Nx's cache client has no read or total timeout, so a transfer that stops
without an error would hang the Nx run. The server stops an artifact transfer
when none of its bytes move for `NX_CACHE_STALL_TIMEOUT` (default `60s`). The
clock restarts each time the server receives a chunk of the artifact (from S3
for a download, from Nx for an upload) and each time it has passed a chunk on.
A slow transfer that keeps moving is never stopped, and there is no limit on
the total transfer time. Chunks are up to 32 KiB, so a transfer slower than one
chunk per timeout counts as stalled.

Bytes stop moving when either end stops: the sender stops sending, or the
receiver stops taking bytes and the connection buffers fill. A client that
stops sending an upload therefore stalls it too, because storage receives
nothing. When the clock runs out, the server cancels the S3 request, releases
any read or write blocked on the Nx connection, and answers by the side it was
waiting on:

| Request | Side | Cause | Response | Outcome |
| --- | --- | --- | --- | --- |
| `GET` | `storage` | S3 stopped sending the artifact. | The started response is cut off, and Nx fails the run instead of hanging. | `error` |
| `GET` | `client` | Nx stopped reading the response. | The response is cut off. | `error` |
| `PUT` | `storage` | S3 stopped taking the body, or did not answer. | A storage failure: 403 `Cache storage unavailable; artifact not stored` with fail-open, else 503. | `degraded` or `error` |
| `PUT` | `client` | Nx stopped sending the body. | 500 `Internal server error`, as for an aborted upload. | `error` |

Nothing is stored from a stopped upload. Each stopped transfer logs
`msg="Transfer stalled"` with `method`, `hash`, `side`, and `timeout`, and
increments `nx_cache_stalled_transfers_total`. A stalled upload to storage also
logs `msg="Storage unavailable"`, like every degraded response.

The stall clock starts when the artifact transfer starts: for a download, when
S3 has sent its response headers (S3 has 5 seconds to send them); for an
upload, when the server starts the S3 `PutObject` after the duplicate check.
For an upload it runs until S3 answers. Separately, the S3 client waits at most
30 seconds for S3 to answer once the body is sent.

## Request log

Each request logs one `msg=request` line with `method`, `path`, `status`, and
`duration`. Cache requests add:

- `hash`: the requested hash.
- `bytes`: the artifact size; the declared `Content-Length` of an upload, or the
  bytes sent for a download.
- `access`: `rw` for a token that can write, `ro` for a read-only one; absent
  when the token is missing or invalid.
- `tier`: `static`, `trusted` or `scoped`: the tier that served or stored the
  artifact, otherwise the token's own tier; absent when the token is missing or
  invalid.
- `repository` and `namespace`: for repository tokens, `github/<repository_id>`
  and the scoped namespace.
- `outcome`: what happened to the request.

| Outcome | Meaning |
| --- | --- |
| `hit` | `GET` sent the artifact. |
| `miss` | `GET` found no artifact (404). |
| `stored` | `PUT` stored the artifact. |
| `conflict` | `PUT` for a hash that already exists (409). |
| `read-only` | `PUT` with a read-only token (403). |
| `degraded` | Storage failed and the server answered a miss (404) or "not stored" (403); see [Cache outages](#cache-outages). |
| `unauthorised` | Missing or invalid token (401). |
| `bad-request` | The request is not valid: an invalid hash (400) or no `Content-Length` (411). |
| `too-large` | `PUT` with a `Content-Length` over `NX_CACHE_MAX_ARTIFACT_BYTES` (403). |
| `error` | 503 when `NX_CACHE_FAIL_OPEN=false`, an upload the client aborted or [stalled](#stalled-transfers) (500), or a download interrupted or stalled after it started. |

Tokens are never logged.

## Metrics

The server serves Prometheus metrics at `GET /metrics` on `METRICS_PORT`
(default 9090), apart from the cache port, so a public ingress for the cache
does not show them. The cache port answers 404 for `/metrics`. The metrics
port has no authentication and no TLS; keep it on the internal network.
`METRICS_PORT=0` turns metrics off, and no metrics port listens.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `nx_cache_requests_total` | counter | `method`, `outcome`, `tier` | Cache requests by HTTP method, [outcome](#request-log) and tier (`static`, `trusted`, `scoped`; empty for a missing or invalid token). |
| `nx_cache_received_bytes_total` | counter | — | Artifact bytes read from upload bodies; an aborted upload counts the bytes read before it stopped. |
| `nx_cache_sent_bytes_total` | counter | — | Artifact bytes written to download responses; an interrupted download counts the bytes sent. |
| `nx_cache_s3_request_duration_seconds` | histogram | `operation` | S3 API call duration (`HeadObject`, `PutObject`, `GetObject`, `HeadBucket`), including SDK retries. `PutObject` includes streaming the upload; `GetObject` ends when the response headers arrive, before the artifact streams. |
| `nx_cache_unconditional_uploads_total` | counter | — | Uploads sent without `If-None-Match` because storage reported conditional writes unsupported; see [Upload compatibility](#upload-compatibility). Above 0, concurrent uploads of one hash can overwrite each other. |
| `nx_cache_stalled_transfers_total` | counter | `method`, `side` | Transfers stopped by the [stall timeout](#stalled-transfers), by HTTP method and the side the server was waiting on: `storage` or `client`. |
| `nx_cache_token_exchanges_total` | counter | `provider`, `result` | Token exchanges: `issued`, `refused` (403), `invalid` (401) or `unavailable` (503, issuer unreachable). `provider` is empty until a CI token's issuer is recognised. |

The standard Go runtime (`go_*`) and process (`process_*`) metrics are also
served.

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
| `mise run e2e` | Real Nx 23.3 and 24.0 builds against the server on Garage and versitygw, with static and issued tokens (needs Docker and Node) |
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
