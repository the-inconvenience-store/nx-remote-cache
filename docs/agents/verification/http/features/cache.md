# Cache reuse and immutability

## User outcome

A trusted build stores an artifact once. Later consumers receive identical bytes;
a duplicate upload cannot replace them, even across cache-server restarts.

## How to reach it

Follow the parent README's prerequisites. Start with a new evidence directory;
the driver creates a fresh bucket and unique hash on isolated real Garage.

## How to drive it

```sh
python3 docs/agents/verification/http/drive.py --evidence /tmp/nx-cache-verification-cache
```

The driver requests a missing hash, uploads a binary payload, downloads it,
attempts replacement, reads using the read-only token, restarts only the cache
server, and reads the same bytes again.

## Proof

Require GET 404 before upload, PUT 200, byte-exact GET 200, replacement PUT 409,
and unchanged GET bytes both before and after process restart. The transcript
records response digests; `server.log` records actual public requests. Storage
survives the process restart, then is deleted during cleanup.

For conditional-write races and streaming boundaries on real backends:

```sh
mise run test-integration
```

For an approved disposable real bucket, configure the AWS/S3 variables documented
in the root README and run `mise run test-s3`. That harness writes and deletes
objects; never point it at a bucket without permission. Neither command replaces
the direct service proof.

## Gotchas

Garage ignores conditional writes; sequential immutability here does not prove
concurrent atomicity. The existing versitygw integration race proves exactly
one winner on that backend. A new backend requires its own contract proof.
No direct-drive artifact is written to a shared bucket.
