# Credential boundaries

## User outcome

Only trusted credentials upload; read-only consumers download existing artifacts;
missing or invalid credentials cannot access cached data.

## How to reach it

Use the parent README's fresh isolated launch. The helper generates distinct
trusted/read-only credentials without capturing them in evidence.

## How to drive it

```sh
python3 docs/agents/verification/http/drive.py --evidence /tmp/nx-cache-verification-access
```

The helper uploads with trusted credentials, downloads with both credential
roles, attempts missing/invalid-token reads, and attempts a read-only upload to
a fresh hash. It checks that denied upload's hash remains absent.

## Proof

Trusted upload and both authorized reads return 200 with byte-identical content.
Missing/invalid reads return 401. Read-only PUT returns 403, followed by trusted
GET 404 for that hash. Invalid credentials still return 401 during the outage.
Capture statuses/digests, not authentication headers.

## Gotchas

A 403 alone does not prove an upload was denied without storage side effects;
the subsequent authenticated miss is required. This local route does not prove
IAM, IRSA, network isolation, or production credential rotation.
