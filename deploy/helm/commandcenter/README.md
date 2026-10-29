# Command Center Helm chart

Runs [Command Center](https://github.com/sungjinseo/commandcenter), a self-hosted registry for
Maven, npm, PyPI, Go modules and Cargo. By default as one pod: a StatefulSet with one replica and
a ReadWriteOnce volume for `/data`, with `meta.db` in SQLite on it. With `replicaCount` above 1
the pods run active-active on PostgreSQL and one ReadWriteMany volume: see
[High availability](#high-availability).

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
  processes would write to one volume. (With `replicaCount` above 1 they share it anyway, on
  PostgreSQL.)

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
| `replicaCount` | `1` | Above 1: active-active pods on PostgreSQL and a ReadWriteMany volume ([High availability](#high-availability)) |
| `database.existingSecret` | `""` | Secret with the PostgreSQL URL (key `url`); required with `replicaCount` above 1 |
| `persistence.storageClass`, `size` | default class, `50Gi` | Block storage with SQLite; ReadWriteMany with `replicaCount` above 1 |
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

## High availability

With `replicaCount` above 1 the chart renders a Deployment instead of the StatefulSet, and every
pod serves every request: there is no leader for requests and no session affinity. The pods share:

- **PostgreSQL** (`database.existingSecret`), which also coordinates them: cluster-wide locks for
  uploads and index rebuilds, change notifications for the caches, one scheduled task run at a
  time on one leader, webhook delivery claims, sessions and the sign-in throttle.
- **One ReadWriteMany volume** at `/data` (`persistence.accessModes: [ReadWriteMany]` with an NFS,
  CephFS or EFS class, or `persistence.existingClaim`): the default blob store and the temp and
  upload session files of every store, so a Docker push may send each chunk to another pod. Named
  blob stores may be S3, or further ReadWriteMany claims mounted at the same path on every pod
  (`extraVolumes`).
- **The same config and secret key** (`secretKey.existingSecret`); a pod with another key refuses
  to start.

The nodes' clocks must be in sync (NTP), the NFS server's too: compaction compares blob ages.
`commandcenter.validate` refuses `replicaCount` above 1 without PostgreSQL or a ReadWriteMany
volume. The chart adds a PodDisruptionBudget of one pod, a `helm.sh/resource-policy: keep` claim,
and, while `affinity` is empty, a preferred anti-affinity that spreads the pods over nodes. The
readiness probe (`/service/rest/v1/status/writable`) takes a pod out of the Service while its
database or change listener is down or while it shuts down.

```yaml
replicaCount: 3
database:
  existingSecret: commandcenter-database   # key url: postgres://...
secretKey:
  existingSecret: commandcenter-secret-key
persistence:
  accessModes: [ReadWriteMany]
  storageClass: nfs-client
  size: 200Gi
```

What a pod failure costs: requests in flight on that pod fail and clients retry (the others keep
serving); a task it was running runs again from the start on the leader (at least once); a webhook
it was sending is sent again after a minute (at least once, with the same
`X-Nexus-Webhook-Delivery` id); its last 30 s of download counts are lost. Writes stay serialized
in PostgreSQL, so more pods add availability and read capacity, not write throughput. Each pod
keeps its own proxy caches in memory, upstream tokens, Prometheus counters and log viewer.

### From one pod to several

A one-time maintenance window:

1. Back up ("백업과 복원" in the guide) and stop the pod:
   `kubectl -n registry scale statefulset commandcenter --replicas=0`.
2. On SQLite, copy `meta.db` into an empty PostgreSQL database with `commandcenter migrate-db -to
   <url>` (the guide's "Kubernetes" section runs a server command as a Pod), and create the URL's
   Secret.
3. Create a ReadWriteMany claim and copy `/data/blobs` and `/data/blobstores` (named file stores)
   from `data-commandcenter-0` into it, for example with a busybox Pod mounting both claims
   (`cp -a`).
4. Upgrade with the shared claim:

   ```bash
   helm upgrade commandcenter oci://ghcr.io/sungjinseo/charts/commandcenter --version <version> \
     -n registry -f values.yaml \
     --set replicaCount=3,database.existingSecret=commandcenter-database,persistence.existingClaim=<rwx claim>
   ```

   Helm replaces the StatefulSet with the Deployment. The old claim `data-commandcenter-0` stays
   until you delete it.

Back to one pod: `replicaCount: 1` renders the StatefulSet again, with a claim template of its own
(or `persistence.existingClaim`), on the same PostgreSQL database.

## Coming from the Nexus Repository chart

| Nexus Repository chart | This chart |
|---|---|
| `statefulset.enabled`, `deploymentStrategy` | A StatefulSet with one pod; with `replicaCount` above 1 a Deployment (rolling update, a new pod first) |
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

   With `replicaCount` above 1 the Deployment starts a new pod before it stops an old one, so the
   registry stays up. The new pod upgrades the database while the old ones serve when the old
   version runs on the new schema (the CHANGELOG says so); otherwise it refuses to start, saying
   why in its log, and the rollout waits while the old pods serve: scale to 0 first for such a
   release.

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

The data stays. The StatefulSet never deletes its PVC (`data-commandcenter-0`), the shared claim of
`replicaCount` above 1 (`commandcenter-data`) is kept by its resource policy, and the secret key
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
