# Nx remote cache

A self-hosted Nx remote cache that stores task artifacts and controls who may
read and write them, for one or many repositories.

## Language

**Tier**:
A set of artifacts with one rule for who may write them and who may read them.
_Avoid_: cache (for a subset of artifacts), bucket

**Static tier**:
The artifacts written with static access tokens; not bound to any repository.
_Avoid_: legacy cache, global cache

**Trusted tier**:
The artifacts one repository's trusted branches wrote; every client of that
repository reads them.
_Avoid_: main cache, production cache

**Scoped tier**:
The artifacts written under one namespace of one repository; only that
namespace's tokens read them.
_Avoid_: PR cache, untrusted cache, branch cache

**Namespace**:
The identity of one scoped tier: a pull request, merge queue entry or branch.
_Avoid_: prefix

**Trusted branch**:
A branch whose pushes write a repository's trusted tier.
_Avoid_: protected branch, default branch

**Issued token**:
A server-issued token bound to one repository and to either its trusted tier or
one namespace.
_Avoid_: scoped token, PR token, branch token

**Token exchange**:
Trading a CI OIDC token for an issued token.
