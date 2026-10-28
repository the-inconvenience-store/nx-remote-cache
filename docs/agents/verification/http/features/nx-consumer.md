# Nx consumer builds

## User outcome

A real Nx 23.3 build uploads its artifact; a read-only build restores it from the
remote cache; loss of storage still allows a fresh build to finish.

## How to reach it

Run from the repository root with mise's Go, Docker, Node.js and npx, plus network
access to npm and container registries. Check `node --version`, `npx --version`,
and `docker info` before running. No shared `mise run dev` instance is needed.

## How to drive it

```sh
mise run e2e
```

The existing `e2e/e2e_test.go` harness builds the current service, launches
isolated Garage and versitygw backends and fresh React Nx 23.3 workspaces, waits
for server health, clears local Nx state before each build, and drives both
trusted and read-only consumer modes. Testcontainers and test cleanup own each
process/container/workspace; the task has a 20-minute deadline.

## Proof

Capture the command and exit status in external evidence. A passing
`TestNxUsesRemoteCacheAndSurvivesStorageOutage` on both backend subtests asserts:
first build `Cache:             0/1 hit` plus a server PUT, read-only second build
`Cache:             1/1 hit`, and successful rebuild with `0/1 hit` plus a storage
failure log after backend shutdown. These are real Nx builds, not mocked results.

Doctor for this launch is the harness's owned fresh process plus `/health` wait;
the first build's required upload and second remote hit prove usable storage.
A successful direct HTTP recipe does not mark this consumer recipe verified.

## Gotchas

The harness creates temporary workspaces, downloads npm packages, and can take
substantially longer than HTTP verification. Do not add verbose auth/environment
logging. npm and image caches can remain; temporary artifact stores and workspaces
must be cleaned by the harness. Failures to launch dependencies are inconclusive,
not evidence of Nx compatibility. This setup does not install or change Node,
quality gates, dependency pins, CI, or hooks.
