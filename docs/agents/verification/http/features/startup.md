# Secure startup and shutdown

## User outcome

The server refuses missing access credentials, serves authenticated cache traffic
over trusted HTTPS when configured, and exits cleanly on SIGTERM.

## How to reach it

Use the parent README's prerequisites and public certificate fixtures. No real
credentials or private production certificates are needed.

## How to drive it

```sh
python3 docs/agents/verification/http/drive.py --tls --evidence /tmp/nx-cache-verification-startup
```

The helper first starts the binary without an access token and requires exit 1
with the named configuration diagnostic. It then configures the certificate/key
pair, verifies the server using the fixture CA and localhost identity, drives the
cache/auth/outage route, and sends SIGTERM at restart and final cleanup.

## Proof

Require invalid startup exit 1, successful CA/hostname-verified HTTPS requests,
byte-exact cache restoration, and exit 0 after SIGTERM. Server logs record HTTPS
startup and graceful-drain entry. No insecure certificate-validation bypass is used.

## Gotchas

These are public test keys, never production credentials. This route proves idle
shutdown and restart durability, not draining an upload already in flight.
Additional configuration boundaries and in-flight lifecycle behavior are covered
by the existing unit/integration suites; they are not claimed by this drive.
