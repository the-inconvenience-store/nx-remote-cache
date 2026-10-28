# Stalled transfers

## User outcome

A transfer that stops without an error does not hang the Nx run. The server
stops it once none of its bytes move for `NX_CACHE_STALL_TIMEOUT`, logs it, and
counts it in `nx_cache_stalled_transfers_total`. A zero, negative or invalid
timeout stops startup.

## How to reach it

Use the parent README's prerequisites. The driver points the server at Garage
through its own loopback relay and sets `NX_CACHE_STALL_TIMEOUT=2s`. The relay
forwards everything until the stall step, then forwards a byte budget in one
direction and holds the rest without reading, as stalled storage does.

## How to drive it

```sh
python3 docs/agents/verification/http/drive.py --evidence /tmp/nx-cache-verification-stalls
```

After the metrics proof, the driver stores a 4 MiB artifact, holds Garage's
download response after 64 KiB, and downloads it. It then holds the upload
after 64 KiB and sends a 32 MiB upload. Before launch it starts the binary with
`NX_CACHE_STALL_TIMEOUT=0s`.

## Proof

The download ends cut off, not complete, between 2 and 15 seconds. The upload
ends in the same window. The server log has one `msg="Transfer stalled"` line
with `side=storage` for each, the download's request line has `status=200` and
`outcome=error`, and the upload's has `status=403` (`503` with
`--fail-closed`). The metrics port shows
`nx_cache_stalled_transfers_total{method="GET",side="storage"} 1` and the same
for `PUT`. After the relay resumes, the artifact downloads byte-exact and the
stalled upload is absent. The `0s` startup exits 1 naming the variable.

## Gotchas

A client that is still sending when the server answers a stalled upload may see
a reset connection instead of the 403; the server's request log is the proof of
the answer. Client-side stalls (Nx stops sending or reading) are proved by the
unit tests in `internal/server/stall_test.go`, not by the driver.
