# Command Center Helm chart

Runs [Command Center](https://github.com/sungjinseo/commandcenter), a self-hosted registry for
Maven, npm, PyPI, Go modules and Cargo, as one pod: a StatefulSet with one replica and a
ReadWriteOnce volume for `/data`. There is no replicas setting. All locking is in-process and
`meta.db` is SQLite, so a second pod would be a second, empty registry.

The operations guide is [docs/guide.md](https://github.com/sungjinseo/commandcenter/blob/main/docs/guide.md)
(Korean); its "Kubernetes" section explains the choices made here.

## Install

Make the admin password hash. Pipe the password in, so it does not stay on the screen:

```bash
printf '%s\n' "$PASSWORD" | docker run --rm -i ghcr.io/sungjinseo/commandcenter:<version> hash-password
```

The `commandcenter` binary from the GitHub Release does the same: `commandcenter hash-password`.

Write a values file:

```yaml
config:
  baseURL: https://repo.example.com   # the address clients use
  adminPasswordHash: '$2a$10$...'     # single quotes
persistence:
  storageClass: <block storage class> # see Storage
ingress:
  enabled: true
  className: nginx
  annotations:
    nginx.ingress.kubernetes.io/proxy-body-size: "0" # the server enforces maxUploadMiB
    nginx.ingress.kubernetes.io/proxy-read-timeout: "300"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "300"
    # Stream uploads instead of spooling them on the shared controller's disk.
    nginx.ingress.kubernetes.io/proxy-request-buffering: "off"
  hosts:
    - host: repo.example.com
      paths: [/]
  tls:
    - secretName: repo-example-com-tls
      hosts: [repo.example.com]
```

Install a released version, then check it:

```bash
helm install commandcenter oci://ghcr.io/sungjinseo/charts/commandcenter --version <version> \
  -n registry --create-namespace -f values.yaml
helm test commandcenter -n registry
```

Chart version 1.2.3 runs image 1.2.3. `helm show values oci://ghcr.io/sungjinseo/charts/commandcenter`
lists every value. The chart's values.schema.json rejects unknown keys, so a misspelt key, or one
from another chart, fails the install instead of being ignored.

## Storage

- `meta.db` is SQLite in WAL mode. Put `/data` on block storage: a cloud disk, iSCSI or a local
  volume. Never use NFS, SMB, EFS or Azure Files, whose locking cannot keep the database
  consistent. With `persistence.storageClass` empty the cluster's default class is used, and
  on-premises clusters often default to an NFS provisioner: check `kubectl get storageclass`.
- Named blob stores may live on NFS. Mount the claim with `extraVolumes` and `extraVolumeMounts`,
  then create the store in the web UI with that path.
- `persistence.storageClass`, `size`, `accessModes` and `annotations` go into the StatefulSet's
  volumeClaimTemplates, which Kubernetes does not let you change. Changing one later fails
  `helm upgrade`; see [Resizing the volume](#resizing-the-volume).
- `persistence.existingClaim` mounts a PVC you made instead, such as a restored snapshot.
- Do not force-delete the pod (`--force --grace-period=0`) while its node may still run it: two
  processes would write to one volume.

## Key values

| Value | Default | |
|---|---|---|
| `config.baseURL` | required | The https address clients use |
| `config.adminPasswordHash` | required | bcrypt hash of the `admin` password |
| `config.anonymousRead` | `false` | Reading without signing in |
| `config.repositories` | `[]` | Repositories created on the first start, as in config.example.yaml |
| `config.extra` | `{}` | `users`, `roles`, `maxUploadMiB`, `metricsFlushSeconds`, `secretKeyFile` |
| `config.existingSecret` | `""` | Your own config.yaml in a Secret (key `config.yaml`). The config values above must then stay empty, and you restart the pod after changing it |
| `secretKey.existingSecret` | `""` | Secret with the key that encrypts stored SMTP, LDAP and remoteAuth passwords. Never remove or replace it without `commandcenter secret-key rotate` |
| `image.tag`, `image.digest` | appVersion, `""` | Pin a version; add the digest cosign verified |
| `persistence.storageClass`, `size` | default class, `50Gi` | Block storage only |
| `resources.limits.memory`, `goMemLimit` | `128Mi`, `80MiB` | Raise them together; GOMEMLIMIT about 60-65 % of the limit |
| `proxy.http`, `https`, `noProxy` | `""` | Forward proxy for proxy repositories |
| `ingress.*` | disabled | Body limit of at least `maxUploadMiB` (1024 MiB) and long timeouts |
| `httpRoute.*` | disabled | Gateway API route instead of an Ingress: `parentRefs` (required) and `hostnames` |

`baseURL`, `anonymousRead`, the users (`adminPasswordHash`), `roles`, `maxUploadMiB` and
`repositories` are imported into meta.db on the first start. After that the web UI owns them:
changing the values does nothing, and does not restart the pod. For a lost admin password:

```bash
printf '%s\n' "$PASSWORD" | kubectl exec -i -n registry commandcenter-0 -- \
  commandcenter user reset-password -user admin -admin
```

## Coming from the Nexus Repository chart

| Nexus Repository chart | This chart |
|---|---|
| `statefulset.enabled`, `deploymentStrategy` | Always a StatefulSet with one pod |
| `nexus.resources`, `nexus.env` | `resources`, `extraEnv`; set `goMemLimit` with the memory limit |
| `nexus.securityContext` | `podSecurityContext`, plus `securityContext` for the container |
| `nexus.podAnnotations`, `nexus.nodeSelector` | `podAnnotations`, `nodeSelector` |
| `nexus.livenessProbe`, `nexus.readinessProbe` | `livenessProbe`, `readinessProbe`, `startupProbe`, as whole probe objects |
| `deployment.terminationGracePeriodSeconds` | `terminationGracePeriodSeconds` (60) |
| `deployment.additionalVolumes`, `additionalVolumeMounts` | `extraVolumes`, `extraVolumeMounts` |
| `persistence.storageSize`, `accessMode` | `persistence.size`, `accessModes` (a list) |
| `persistence.enabled: false` | None: `/data` is always a volume |
| `persistence.existingClaim`, `storageClass` (`"-"`) | The same |
| `ingress.ingressClassName` | `ingress.className` |
| `ingress.hostRepo`, `hostPath` | `ingress.hosts[].host`, `ingress.hosts[].paths` |
| `NEXUS_SECURITY_RANDOMPASSWORD` | `config.adminPasswordHash` |

## Upgrades

1. Read the [CHANGELOG](https://github.com/sungjinseo/commandcenter/blob/main/CHANGELOG.md) for
   the new version and back up ("백업과 복원" in the guide). Downgrades are not supported: a new
   version may upgrade meta.db, which older versions cannot use. The way back is that backup with
   the old version, since a `helm rollback` to the old version is a downgrade too.
2. Upgrade:

   ```bash
   helm upgrade commandcenter oci://ghcr.io/sungjinseo/charts/commandcenter --version <new> \
     -n registry -f values.yaml
   ```

   The old pod stops, taking up to 60 seconds, before the new one starts. A database upgrade may
   take up to 5 minutes (the startupProbe), so do not use `--atomic` with a shorter `--timeout`.
   Its rollback would put the old version in front of the upgraded database.

Never change `nameOverride` or `fullnameOverride` after the install: a new name is a new
StatefulSet with a new, empty volume.

### Resizing the volume

The StorageClass must allow volume expansion. Resize the claim, then recreate the StatefulSet
around the running pod:

```bash
kubectl -n registry patch pvc data-commandcenter-0 -p '{"spec":{"resources":{"requests":{"storage":"100Gi"}}}}'
kubectl -n registry delete statefulset commandcenter --cascade=orphan # the pod keeps running
helm upgrade commandcenter oci://ghcr.io/sungjinseo/charts/commandcenter --version <current> \
  -n registry -f values.yaml # with persistence.size: 100Gi
```

With `persistence.existingClaim`, resize that claim and nothing else.

## Uninstall

```bash
helm uninstall commandcenter -n registry
```

The data stays. The StatefulSet never deletes its PVC (`data-commandcenter-0`), and the secret key
Secret is yours. Installing again with the same release name and namespace mounts the same volume,
with its users and repositories; the config values are not imported a second time. To delete the
data for good, delete the PVC: `kubectl -n registry delete pvc data-commandcenter-0`. With the
usual reclaim policy `Delete`, that also deletes the disk.

## OpenShift

The restricted-v2 SCC assigns the user and groups from the namespace's range and rejects the fixed
65532. Leave them to it; the image runs as any user:

```yaml
podSecurityContext:
  runAsUser: null
  runAsGroup: null
  fsGroup: null
```
