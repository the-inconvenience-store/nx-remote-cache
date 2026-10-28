# Security Policy

## Supported versions

Only the latest published release receives security fixes. Images are published
to `ghcr.io/the-inconvenience-store/nx-remote-cache` and the chart to
`oci://ghcr.io/the-inconvenience-store/charts/nx-remote-cache`; older tags are
not patched.

## Reporting a vulnerability

Please do not open a public issue, a pull request, or a discussion for a
suspected vulnerability.

Report it only through
[GitHub private vulnerability reporting](https://github.com/the-inconvenience-store/nx-remote-cache/security/advisories/new).

Helpful things to include, as far as you have them:

- what an attacker can do, and what access they need to do it
- the affected image tag, chart version, or commit
- steps to reproduce, or the command output that shows the problem
- any workaround you already found

## What to expect

- An acknowledgement within 5 days.
- An assessment — whether the report is confirmed, and the severity — within 14
  days.
- A fix released before any public disclosure, coordinated with you on timing.
- Credit in the advisory and the release notes, unless you prefer otherwise.

The Inconvenience Store maintains this project in spare time, so these are
honest targets rather than a contractual SLA. If a report goes unanswered past
those windows, a comment on the private advisory is welcome.

## Scope

In scope: this server, its container image, the Helm chart in `charts/`, and the
GitHub Actions workflows in `.github/workflows/`.

Out of scope: vulnerabilities in Nx itself, in your S3 provider, or in
deployments that expose the server with a weak or leaked
`NX_CACHE_ACCESS_TOKEN`.

## Cache credential trust

Keep `NX_CACHE_ACCESS_TOKEN` restricted to trusted builds. Jobs executing
untrusted PR code must never receive it, including through privileged workflow
arrangements. `NX_CACHE_READ_ONLY_TOKEN` permits downloads but rejects uploads;
omit credentials entirely when those jobs must not read cached outputs.

Require HTTPS for clients, keep S3 credentials server-side, and use distinct
cache tokens. TLS does not turn untrusted writers into trusted writers.
Read-only credentials prevent new writes; they do not remove artifacts
previously written by a compromised trusted credential.
