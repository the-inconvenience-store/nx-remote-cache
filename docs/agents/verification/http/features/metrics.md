# Prometheus metrics

## User outcome

An operator scrapes cache request counts by method and outcome from
`METRICS_PORT`, a plain-HTTP port apart from the cache port. The cache port
answers 404 for `/metrics`, and `METRICS_PORT=0` opens no metrics port.

## How to reach it

Use the parent README's prerequisites. The driver allocates a free loopback
metrics port; the metrics port never uses TLS, even with `--tls`.

## How to drive it

```sh
python3 docs/agents/verification/http/drive.py --evidence /tmp/nx-cache-verification-metrics
```

After the miss, upload, hits and rejected overwrite, the driver scrapes
`/metrics` on the metrics port, requests `/metrics` on the cache port, and
restarts the server with `METRICS_PORT=0`.

## Proof

Require exactly `GET hit 2`, `GET miss 1`, `PUT stored 1` and `PUT conflict 1`
in `nx_cache_requests_total`, 404 for `/metrics` on the cache port, and a
refused connection to the former metrics port after the `METRICS_PORT=0`
restart.

## Gotchas

The driver reserves the metrics port by binding and releasing it, so another
process could take it first; rerun with a new evidence directory if launch fails.
