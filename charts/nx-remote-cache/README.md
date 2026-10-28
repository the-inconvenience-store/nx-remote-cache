# nx-remote-cache Helm chart

Helm chart for deploying
[nx-remote-cache](https://github.com/the-inconvenience-store/nx-remote-cache) —
a self-hosted Nx remote cache backed by S3-compatible storage — to Kubernetes.

## Install

Charts are published as OCI artifacts to GHCR.

```bash
helm install nx-cache oci://ghcr.io/the-inconvenience-store/charts/nx-remote-cache \
  --version 0.1.0 \
  --namespace nx-cache --create-namespace \
  --set secrets.nxCacheAccessToken=<token> \
  --set secrets.awsAccessKeyId=<key> \
  --set secrets.awsSecretAccessKey=<secret> \
  --set config.s3.bucketName=nx-cloud
```

Set `config.s3.endpointUrl` for S3-compatible storage other than AWS S3; leave
it empty for AWS.

## Using an externally managed Secret

Reference an existing Secret via `secrets.existingSecret`. Keys:

| Key | Required | Env var |
| --- | --- | --- |
| `nx-cache-access-token` | yes | `NX_CACHE_ACCESS_TOKEN` (read/write) |
| `nx-cache-read-only-token` | no | `NX_CACHE_READ_ONLY_TOKEN` (download only) |
| `aws-access-key-id` | no (both or neither) | `AWS_ACCESS_KEY_ID` |
| `aws-secret-access-key` | no (both or neither) | `AWS_SECRET_ACCESS_KEY` |

```bash
kubectl create secret generic nx-cache-creds \
  --from-literal=nx-cache-access-token=<token> \
  --from-literal=aws-access-key-id=<key> \
  --from-literal=aws-secret-access-key=<secret>

helm install nx-cache oci://ghcr.io/the-inconvenience-store/charts/nx-remote-cache \
  --version 0.1.0 \
  --set secrets.existingSecret=nx-cache-creds
```

Omit optional keys rather than storing empty values: the server rejects an empty
read-only token, and setting only one AWS key prevents startup.

## Read-only clients

Add `nx-cache-read-only-token` to the Secret, or set
`secrets.nxCacheReadOnlyToken` when the chart creates it. It must be nonempty,
contain no whitespace, and differ from `nx-cache-access-token`. The access token
keeps read/write permission; the read-only token permits GET and gets 403 on
PUT. Give PR jobs only the read-only token (or none if they must not read cached
outputs).

## Rotating tokens

Both token keys accept a comma-separated list, and every listed token works:
add the new token (`old-token,new-token`), move CI jobs to it, then remove the
old one. The server does not start if an item is empty, a token is listed
twice, or a token is in both lists. With `helm --set`, escape the comma
(`--set 'secrets.nxCacheAccessToken=old-token\,new-token'`) or use a values file.

## IRSA / Workload Identity (no static AWS keys)

When neither AWS key is set, the server uses the AWS SDK default credential
chain (IRSA web identity, instance profile, etc.). Annotate the ServiceAccount
and leave both AWS keys out:

```yaml
serviceAccount:
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/nx-remote-cache

secrets:
  nxCacheAccessToken: <token>
  # awsAccessKeyId / awsSecretAccessKey left empty: omitted from the Secret
```

With `existingSecret`, a Secret holding only `nx-cache-access-token` works too;
the AWS env refs are `optional: true`.

## Health, readiness and storage outages

- Liveness probe: `/health` (process only).
- Readiness probe: `/ready`, which checks bucket access with `HeadBucket`
  (2-second internal timeout, so `probes.readiness.timeoutSeconds` defaults to
  3). Pods that lose storage access leave the Service without restarting.
  `HeadBucket` needs `s3:ListBucket`.
- `config.failOpen` (`NX_CACHE_FAIL_OPEN`, default `true`): on storage failure,
  GET returns 404 (cache miss) and PUT returns 403 (not stored) so Nx keeps
  building. Set `false` to return 503 and fail the Nx run instead.

## Serving over HTTPS

TLS is usually terminated at the Ingress. If you instead want the pod itself to
serve HTTPS, point `tls.secretName` at an existing `kubernetes.io/tls` Secret
(e.g. one issued by cert-manager) and enable TLS. The cert/key are mounted
read-only, `TLS_CERT_PATH`/`TLS_KEY_PATH` are set, and the probes switch to the
HTTPS scheme automatically.

```bash
helm install nx-cache oci://ghcr.io/the-inconvenience-store/charts/nx-remote-cache \
  --set secrets.existingSecret=nx-cache-creds \
  --set tls.enabled=true \
  --set tls.secretName=nx-cache-tls
```

> **Certificate rotation.** The server reads the cert/key once at startup, so a
> renewed certificate is not picked up until the pod restarts. When cert-manager
> rotates the Secret, trigger a rollout (`kubectl rollout restart deployment/...`)
> or use a controller such as [Reloader](https://github.com/stakater/Reloader) to
> restart the pods automatically.

## Values

| Key | Default | Description |
| --- | --- | --- |
| `image.repository` | `ghcr.io/the-inconvenience-store/nx-remote-cache` | Container image |
| `image.tag` | `""` (Chart.AppVersion) | Image tag |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | |
| `replicaCount` | `1` | Run more than one for availability |
| `service.type` | `ClusterIP` | |
| `service.port` | `3000` | |
| `serviceAccount.create` | `true` | |
| `serviceAccount.name` | `""` | |
| `serviceAccount.annotations` | `{}` | IRSA / Workload Identity annotations |
| `probes.liveness.enabled` | `true` | Liveness probe on `/health` |
| `probes.readiness.enabled` | `true` | Readiness probe on `/ready` (checks bucket access) |
| `probes.readiness.timeoutSeconds` | `3` | Must exceed the 2s bucket check |
| `config.port` | `3000` | `PORT` |
| `config.failOpen` | `true` | `NX_CACHE_FAIL_OPEN`; `false` returns 503 on storage failures |
| `config.awsRegion` | `us-east-1` | `AWS_REGION` |
| `config.s3.bucketName` | `nx-cloud` | `S3_BUCKET_NAME` |
| `config.s3.endpointUrl` | `""` | `S3_ENDPOINT_URL`; omitted when empty (AWS S3) |
| `secrets.existingSecret` | `""` | If set, skip Secret creation and use this one |
| `secrets.nxCacheAccessToken` | `""` | Required if `existingSecret` is empty |
| `secrets.nxCacheReadOnlyToken` | `""` | Optional download-only token |
| `secrets.awsAccessKeyId` | `""` | Optional; set both AWS keys or neither |
| `secrets.awsSecretAccessKey` | `""` | Optional; set both AWS keys or neither |
| `tls.enabled` | `false` | Serve over HTTPS using a mounted cert/key |
| `tls.secretName` | `""` | Existing Secret holding the PEM cert/key. Required when `tls.enabled` |
| `tls.certKey` | `tls.crt` | Key in the Secret holding the PEM cert |
| `tls.keyKey` | `tls.key` | Key in the Secret holding the PEM key |
| `tls.mountPath` | `/etc/nx-remote-cache/tls` | Mount path for the cert/key |
| `extraEnv` | `[]` | Extra env vars appended to the container |
| `resources` | `{}` | |
| `nodeSelector` | `{}` | |
| `tolerations` | `[]` | |
| `affinity` | `{}` | |
| `podAnnotations` | `{}` | |
| `podLabels` | `{}` | |
| `podSecurityContext` | `{}` | |
| `securityContext` | `{}` | |
