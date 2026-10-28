# Health and outages

## User outcome

Operators distinguish process liveness from usable storage. Cache consumers get
nonfatal misses/upload refusals by default, or explicit errors in fail-closed mode.

## How to reach it

Use the parent README's prerequisites. The driver first requires a ready bucket,
then stops only its disposable Garage container after cache and auth checks.

## How to drive it

```sh
python3 docs/agents/verification/http/drive.py --evidence /tmp/nx-cache-verification-outage-open
python3 docs/agents/verification/http/drive.py --fail-closed --evidence /tmp/nx-cache-verification-outage-closed
```

## Proof

Before failure, `/health` and `/ready` return 200 `OK`. After storage stops,
`/health` remains 200 `OK`, `/ready` becomes 503 `Storage unavailable`, fail-open
GET returns 404 and PUT 403. Fail-closed GET and PUT both return 503. Invalid
cache credentials still return 401. Logs record the real storage failure.
Require successful cleanup and `VERIFIED` for each claimed mode.

## Gotchas

Readiness failure intentionally removes a pod from routing without requiring a
restart. A healthy process alone is not a usable cache. HTTP behavior does not
prove Nx tolerates those responses; use the consumer feature for that claim.
