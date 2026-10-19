# Architecture

`retycfs-k8s-csi` is a filesystem CSI driver in the same family as `csi-driver-nfs` or `csi-driver-smb`: no
attach/detach step, no block devices, one shared filesystem per volume mounted on every node that needs it. The
filesystem is a Retyc dataroom, mounted with RetycFS, the filesystem built into `retyc-cli` (`retyc fs mount`). Each
mount runs in its own pod, the **mount pod**, so that the node plugin holds no mount and can restart freely.

## Overview

```
   PersistentVolumeClaim (csi.retycfs.com/uid)                       Pod (runAsUser = uid)
          │                                                           │ /data
          ▼                                                           ▼
  ┌─ controller (Deployment) ──────────────┐   ┌─ node plugin (DaemonSet, privileged) ──────────┐
  │ csi-provisioner --extra-create-metadata│   │ csi-node-driver-registrar                      │
  │ csi-resizer (expand + modify)          │   │ retycfs-k8s-csi --mode=node   (no retyc process) │
  │ retycfs-k8s-csi --mode=controller        │   │   NodeStage   → Secret + mount pod, /readyz    │
  │   └─ retyc dataroom create / set / rm  │   │   NodePublish → bind staging path → target     │
  └──────────────────┬─────────────────────┘   │   NodeUnstage → delete the mount pod (drain)   │
                     │                         │   watch + every 30 s: repair, restack binds    │
                     │                         └──────────────┬─────────────────────────────────┘
                     │                                        │ creates, deletes, probes :9567
                     │                         ┌─ mount pod (1 per volume and node) ───────────┐
                     │                         │ retyc fs mount retyc://<id> <staging path>     │
                     │                         │   --allow-other --uid --gid --spool-dir ...   │
                     │                         │ root, privileged, /dev/fuse, Bidirectional    │
                     │                         └──────────────┬─────────────────────────────────┘
                     ▼                                        ▼  encrypted chunks
                                        Retyc API
```

- **Controller** (`--mode=controller`, one replica by default): creates a dataroom per `CreateVolume`, reserving the
  claim's size, deletes it on `DeleteVolume`, enlarges it on `ControllerExpandVolume` and applies a
  VolumeAttributesClass on `ControllerModifyVolume`.
- **Node plugin** (`--mode=node`, one per node): starts a mount pod per staged volume, bind-mounts the staging path
  into every pod that uses the claim, and repairs what dies. It runs no `retyc` process.
- **Mount pods**: one per volume and per node, in the driver's namespace, running `retyc fs mount` from the official
  `ghcr.io/retyc/retyc-cli` image. The pod **is** the mount.
- **Sidecars**: the standard `csi-provisioner`, `csi-resizer` and `csi-node-driver-registrar` from kubernetes-csi.

The driver never speaks to the Retyc API itself: every operation goes through the same vetted `retyc` binary the CLI
team ships, embedded in the controller image (`COPY --from` the retyc-cli image) and run as the mount pods' image.

## Components

| Component | Where | Role |
|-----------|-------|------|
| `retycfs-k8s-csi --mode=controller` | `Deployment` in the driver's namespace, 1 replica by default | `CreateVolume`, `DeleteVolume`, `ControllerExpandVolume`, `ControllerModifyVolume`, `ValidateVolumeCapabilities` |
| `csi-provisioner` | sidecar of the controller | watches PVCs, passes the claim's name and namespace (`--extra-create-metadata`) |
| `csi-resizer` | sidecar of the controller | claim expansion and VolumeAttributesClass changes |
| `retycfs-k8s-csi --mode=node` | `DaemonSet`, privileged | `NodeStageVolume`, `NodePublishVolume`, their inverses, `NodeGetVolumeStats`; manages the mount pods |
| `csi-node-driver-registrar` | sidecar of the node plugin | registers the driver socket with kubelet |
| mount pod `retycfs-mount-<hash>` | `Pod` in the driver's namespace, pinned to its node | `retyc fs mount` of one dataroom on kubelet's staging path |
| `retyc` | controller image, and image of the mount pods | every interaction with the Retyc API and all cryptography |

Both driver modes are the same binary and the same image. The controller runs `retyc --json dataroom
create|set|rm|ls` and `user quota` as short-lived subprocesses; the node plugin only talks to the Kubernetes API and
to the mount pods' `/readyz`.

## Identities

An identity is an offline token plus the passphrase of the account's AGE key. The driver knows two sources:

- the **default identity**, from the driver pods' environment (the chart's credentials Secret), used by requests that
  carry no secrets. Optional: without it, the chart does not render the StorageClasses that would need it;
- **per-request secrets**, resolved by the sidecars and kubelet from a StorageClass's `csi.storage.k8s.io/*-secret-*`
  parameters (`retycfs-rwx-tenant`) and handed to `CreateVolume`, `DeleteVolume`, `ControllerExpandVolume`,
  `ControllerModifyVolume` (which reuses the controller-expand secret) and `NodeStageVolume`.

kubelet takes the node-stage secret from the PV, not from the class: a static PV of a tenant class written without
`nodeStageSecretRef` is staged with no secrets. Before using the default identity for a stage, the node plugin
therefore reads every PV of the volume handle and their StorageClasses, and refuses with `FailedPrecondition` when the class declares
`csi.storage.k8s.io/node-stage-secret-name` (or the PV names a node-stage secret kubelet did not send): a tenant's
volume is never mounted with the cluster-wide account (see [configuration.md](configuration.md#static-pvs)).

The controller runs each `retyc` subprocess with the request's credentials. On the node, every mount pod gets its own
Secret, `retycfs-mount-<hash>-<suffix>`, holding `RETYC_TOKEN` and `RETYC_KEY_PASSPHRASE` (the default identity, or a
copy of the tenant's credentials received at stage) and, with Redis, `RETYC_CACHE_REDIS_URL`. The Secret is created
before the pod and owned by it, so the garbage collector deletes it with the pod. Each mount pod has its own
`RETYC_CONFIG_DIR` in an `emptyDir`: tokens and caches never mix between volumes. The CLI's kernel keyring cache is
turned off (`RETYC_KEYRING_ENABLED=false`) everywhere: every mount pod runs as root and would share root's keyring.

## Volume lifecycle

1. **Provisioning.** A claim on a Retyc StorageClass makes `csi-provisioner` call `CreateVolume`. Before anything is
   created on the Retyc side, the controller reads the claim (`csi.storage.k8s.io/pvc/name` and `pvc/namespace`) and
   refuses it with `InvalidArgument` when its `csi.retycfs.com/uid` annotation is missing or invalid. It then reuses a
   dataroom already titled after the volume (idempotency), or checks the account's quota and runs
   `retyc dataroom create --title <pv-name> --storage-capacity <bytes> [--versioning=<bool>]`. The dataroom ID becomes
   the volume handle; the volume context records `pvcName` and `pvcNamespace`.
2. **Capacity.** The reservation is the claim's request, with a floor of **20 GiB** imposed by the API: a smaller
   request is rounded up, as a block storage rounds up to its allocation unit, and the PV reports the size actually
   reserved (a `1Gi` claim gets a `20Gi` PV). A `limit_bytes` below 20 GiB is `OutOfRange`.
3. **Staging.** When a pod using the claim lands on a node, kubelet calls `NodeStageVolume`. The node plugin reads the
   claim's annotations (over the PV's `volumeAttributes`), creates the mount pod's Secret and the mount pod, and
   returns `Aborted` until the pod's `/readyz` answers 200: kubelet retries, and a first mount (image pull, login,
   scrypt) can outlast one CSI call. One mount pod per volume per node, whatever the number of pods.
4. **Publishing.** `NodePublishVolume` bind-mounts the staging path into the pod's volume directory, read-only for a
   read-only publish.
5. **Unpublish / unstage.** The bind mount goes with the pod. When the last pod of the volume has left the node,
   `NodeUnstageVolume` deletes the mount pod: the CLI gets `SIGTERM`, sends what is still being written (up to
   `--drain-timeout`, 5 min by default), unmounts and exits. The call returns `Aborted` while the pod drains, then
   reads its exit code, removes the finalizer and unmounts whatever is left on the staging path.
6. **Expansion.** Raising the claim's request makes `csi-resizer` call `ControllerExpandVolume`, i.e.
   `retyc dataroom set --storage-capacity`; nothing is done on the node (`NodeExpansionRequired=false`). A
   reservation that already covers the request is left alone, so a retry succeeds.
7. **Deletion.** With `reclaimPolicy: Delete`, deleting the claim makes the provisioner call `DeleteVolume`, i.e.
   `retyc dataroom rm retyc://<id> -y`. A dataroom already gone is a success. With `Retain`, the PV is released and the
   dataroom kept; `examples/manifests/static-pv.yaml` shows how to adopt it again.

### Settings read at stage

Every `csi.retycfs.com/*` annotation of the claim (see [configuration.md](configuration.md#pvc-annotations)) is read
**at stage**, along with the mount pods' chart settings. A change takes effect at the next mount of the volume on a
node, never on a running mount pod: the pod carries a hash of everything it cannot change (credentials, owner,
options, resources, image, cache), and a stage that finds another hash recreates the pod only when the volume is no
longer published on the node. Otherwise the pod keeps its settings and the claim gets a `MountConfigChanged` event.
A new retyc-cli version (chart `mount.image`) follows the same rule: running mount pods keep theirs until their volume
is remounted, and `retycfs_csi_mount_pod_outdated` counts them.

### VolumeAttributesClass and versioning

The only mutable parameter is `versioning` (`true` or `false`), from a VolumeAttributesClass (Kubernetes 1.34+). It is
applied at creation (`mutable_parameters`) and whenever the claim's `volumeAttributesClassName` changes
(`ControllerModifyVolume`, `retyc dataroom set --versioning=<bool> -y`). A class without `versioning` changes nothing;
an unknown key is refused. With versioning, every `close` of a written file stores a version. **Turning it off is
destructive**: the driver confirms the CLI's prompt on the caller's behalf, and the next completed upload of each file
deletes its older versions.

### Errors of the controller

| Condition | CSI code |
|-----------|----------|
| no `csi.retycfs.com/uid`, or an invalid one; unknown StorageClass parameter; unknown or invalid VolumeAttributesClass parameter; block volume; API validation error | `InvalidArgument` |
| claim not found | `NotFound` |
| claim cannot be read (Kubernetes API) | `Unavailable` |
| `limit_bytes` below 20 GiB or below the request; reservation the API refuses to shrink (`capacity_shrink`) or below its minimum (`capacity_too_small`) | `OutOfRange` |
| account read-only, dataroom count reached, no storage left on the plan (`plan_exceeded`, `storage_exceeded`) | `ResourceExhausted` |
| dataroom of the same name with an incompatible reservation | `AlreadyExists` |
| no secrets and no default identity; login refused (`auth_required`, `unauthenticated`); key passphrase missing or wrong (`config`); CLI refused as too old (`cli_too_old`) | `FailedPrecondition` |
| API answered 403 (`forbidden`) | `PermissionDenied` |
| API rate limit (`rate_limited`) | `Unavailable` |
| dataroom missing on expand or modify | `NotFound` |
| anything else from the CLI (`api_error`, `error`, an unknown kind) | `Internal` |

The provisioner and the resizer retry with their own backoff, and record the message as an event on the claim.

The CLI's failures are told apart by the `kind` of the JSON error it prints on stderr under `--json` (retyc-cli
v1.5.0-beta1 on: `{"error", "kind", "status", "code", "detail"}`, kinds in brackets above); `detail`, the API's wording,
is only carried in the message. A login refused is `FailedPrecondition` rather than `Unauthenticated`: in gRPC the
latter says that the RPC's caller (the sidecar) is not authenticated, while here it is the identity of the secrets that
the Retyc API refuses, and a new token is what it waits for. An older CLI prints only `{"error": "..."}`: the driver
then falls back to the text the CLI relays from the API (`API error 404:`, `API error 422:`, `Not enough storage left`,
`can only be enlarged`, `must be at least N bytes`), and the login, passphrase, 403, rate limit and version refusals
stay `Internal`.

`ControllerExpandVolume` reads the dataroom's reservation before `dataroom set`: the API accepts the current value
without effect, but refuses a smaller one, and a reservation already at or above the request (enlarged elsewhere, or
a reused dataroom) must succeed with its real size.

## Mount pods

The node plugin builds each mount pod itself (no Helm template). Name `retycfs-mount-<hash(node, volume)>`, stable so
that a restarted node plugin finds its pods again; labels `app.kubernetes.io/name=retycfs-mount`,
`csi.retycfs.com/volume`, `/node`, `/pvc` and `/namespace`, with the exact values in annotations of the same names.

- **Command**: `retyc fs mount retyc://<volume-id> /staging/globalmount --allow-other --uid=<uid> --gid=<gid>
  --spool-dir=/spool --metrics-addr=0.0.0.0:9567 --drain-timeout=<drainTimeout>`, plus the flags of the claim's
  annotations and `--cache-store=redis` with Redis.
- **Where it mounts**: kubelet's staging path is
  `<kubeletDir>/plugins/kubernetes.io/csi/csi.retycfs.com/<sha256(volume handle)>/globalmount`. The pod receives its
  parent directory, the `<sha256>` one, as a `hostPath` with `Bidirectional` propagation, and mounts
  RetycFS **directly on the staging path**. The RetycFS mount is then the only mount of the volume on the host besides
  the pods' bind mounts, which kubelet's check before `NodeUnstageVolume` requires. The pod sees no other volume.
- **Security context**: root (the retyc-cli image runs as 1000:1000; `mount(2)` needs root), privileged, `/dev/fuse`.
  The driver's namespace must therefore allow privileged pods (Pod Security `privileged`).
- **Scheduling**: `nodeName` fixed, tolerates every taint, `system-node-critical` by default, annotated
  `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` and `karpenter.sh/do-not-disrupt: "true"`.
- **Resources**: 50m CPU and 256 MiB requested, 512 MiB limit, no CPU limit (Burstable). Unlocking the AGE key
  (scrypt) peaks at about 270 MiB at start, then is given back. RetycFS then keeps what the kernel keeps in its
  cache: about 4 KiB of the mount pod's memory per file or folder examined (`ls -l`, `stat`, `open`, a backup),
  2 KiB per entry only listed (`ls`, `find`). The kernel drops these entries when the node runs short of memory,
  never when only the mount pod reaches its limit: the mount pod is then killed (`137`, OOMKilled) and recreated.
  The default limit holds about 100 000 entries examined; raise `csi.retycfs.com/mount-memory-limit` for a larger
  dataroom walked in full. Overridable per claim (`csi.retycfs.com/mount-*` annotations).
- **Spool**: `hostPath` `<mount.spoolDir>/<volume-id>` (`/var/lib/retycfs-csi/spool/<id>`), mode 0700, emptied by the
  node plugin before every creation: a killed process leaves its files there.
- **Restart policy** `Never`: the node plugin recreates the pod, with its own backoff (below).
- **Grace period**: `drainTimeout` + 10 s, so that kubelet only sends `SIGKILL` once the drain is over.
- **Termination message**: `FallbackToLogsOnError`, so the end of the CLI's log stays in the pod status after a failure.
- **Readiness probe** on `/readyz` (port `metrics`), for `kubectl` and Prometheus; the node plugin probes `/readyz`
  itself on the pod IP, which needs no `hostNetwork`.
- **No service account token**, no service links.

### Finalizer

Each mount pod carries the finalizer `csi.retycfs.com/in-use`. It keeps the pod **object** after its container stopped,
until the node plugin has read the exit code (`status.containerStatuses[].state.terminated`). Only
`NodeUnstageVolume`, a recreation by the node plugin or the deletion of an orphan removes it. It does **not** stop the
container from being killed: a `kubectl delete`, an eviction or a `kubectl drain` still stop the mount (see
[Limitations](#limitations)).

### Backoff and give-up

A mount pod that exits while its volume is staged is recreated with an exponential backoff: 5 s, then 10 s, 20 s... up
to 5 min, plus up to 10% of jitter. The backoff starts over once a pod was seen ready, or after a pod ran for a minute.
Exit codes `77` (the login needs a new token: missing, expired or revoked) and `78` (key passphrase missing or wrong)
mean the same credentials will fail again: after three such exits in a row, the node plugin **gives up** on the volume
on that node. `NodeStageVolume` then fails at once with `FailedPrecondition`, without creating a pod nor logging in
again, and the claim gets one `MountGaveUp` event. The give-up lasts until the credentials change (another config
hash), the volume is unstaged from the node, the node plugin restarts, or someone deletes the exited mount pod
(`kubectl delete pod`): each of these starts a full series of tries again. A give-up during a stage is a final error
for kubelet: it removes `vol_data.json` and will never unstage the volume, so the next sweep (30 s at most) releases
the exited pod, its finalizer and its Secret, like any orphan. The give-up itself is remembered for 30 minutes after
the last stage that found it, so that kubelet's retries keep failing at once (with the last output of the pod) instead
of logging in again with refused credentials. Any other code may be transient (API or identity provider unreachable) and is retried
forever. The backoff lives in the node plugin's memory: a restart of the plugin forgets it.

### Exit codes

| Code | Meaning | What follows |
|------|---------|--------------|
| `0` | unmounted, nothing lost (an unstage, or an `umount` of the staging path) | nothing |
| `1` | any other error, a failure at start included (a Redis that does not answer, a mount point left dead) | when the node plugin releases the pod: a `MountFailed` event on the claim, unless the output reports lost files (below); recreated with the backoff while the volume is staged |
| `75` | writes lost: files being written, or background deletes, could not be sent before the filesystem went away (drain timed out or cut short, API refusal) | when the node plugin releases the pod: a `WritesLost` event on the claim; recreated with the backoff while the volume is staged, never a give-up (the mount itself worked) |
| `77` | login cannot recover: `RETYC_TOKEN` missing, expired or revoked; it wins over `75` when both happen | backoff, give-up after three in a row; a `WritesLost` event too if the output reports lost files |
| `78` | key passphrase missing or wrong | backoff, give-up after three in a row |
| `137` | killed (`SIGKILL`: `kill -9`, OOM kill, grace period exceeded) | recreated with the backoff, binds restacked; a `MountOOMKilled` event on the claim for an OOM kill, or any `137` while no deletion asked the pod to stop (containerd does not always say `OOMKilled`) |

A pod that left no exit code (evicted, lost with its node) counts as `-1`.

`75` comes with retyc-cli v1.5.0-beta1. An older CLI (a development image before it) exits `1` on lost writes as on any
other error, with the same message (`N files being written were not sent before the filesystem was unmounted...`): an
exit `1` whose output (the termination message) holds those words, or the drain's `abandoned (no drain timeout)` and
`drain over after ...: ... not sent`, is read as lost writes. A newer CLI never exits `1` on lost writes, so the
fallback holds for both; whether the pod was ever ready only changes the wording of `MountFailed`.

## Recovery

The mount pods are the only record of the staged volumes: there is no state file on the node. Each pod records its
staging path, its settings and its Secret in annotations. A volume counts as staged while kubelet's `vol_data.json`
exists next to its staging path, or while a pod target path still binds its mount.

The node plugin reconciles the mount pods with the mounts:

- **at start**, before it opens the CSI socket (bounded to a minute), so that kubelet's first calls find the mounts
  repaired;
- **on every change of a mount pod** of its node, through a watch (informer): a pod that exits, is deleted or becomes
  ready is repaired at once, within its restart delay;
- **every 30 s**, as a safety net for what an event did not cure (an API error, a pod stuck terminating). A failed
  repair is queued again after 1 s, doubled at each failure in a row up to 5 min;
- from `NodePublishVolume`, when a new pod lands on a node whose staging mount is dead: kubelet does not stage a volume
  again while another pod on the node still uses it.

For each mount pod whose volume is staged:

1. a dead pod is recreated on the same staging path, after the dead RetycFS mount (`ENOTCONN`) is unmounted and the
   spool emptied;
2. once the new pod is ready, a bind mount of the staging path is **stacked** on every pod target path still bound to
   the dead mount (found in `/proc/self/mountinfo` by the source `retyc://<id>` and the device). Past four layers on a
   target path, the dead ones on top are unmounted first.

A mount pod whose volume is neither staged nor bound anywhere is an orphan (an unstage that never completed, a stage
that failed for good) and is deleted. When the node plugin cannot read kubelet's directory of the driver,
`<kubeletDir>/plugins/kubernetes.io/csi/csi.retycfs.com` (a `node.kubeletDir` that is not kubelet's `--root-dir`, or the
plugins directory not mounted), every volume would look unstaged: it then deletes no orphan and logs the error. A volume still mounted on the node without any mount pod (deleted by hand, with
its finalizer removed) is logged: nothing is left to recreate it from, reschedule its pods.

After a `kill -9` of a mount process, the volume is back in about ten seconds: the first restart waits 5 s, then the
login, the key unlock and the restack.

Stacking rather than replacing is what reaches running pods: a container's mount of the volume is a copy of the target
path's mount, and a mount stacked on the target propagates into that copy when the pod mounts the volume with
`mountPropagation: HostToContainer`, as the examples do. `NodeUnpublishVolume` unmounts every layer.

`HostToContainer` is recommended, not required. It is Linux `rslave` propagation: mounts made on the host under the
volume's target path show up in the container, nothing propagates back. It needs neither `privileged` nor any
capability (only `Bidirectional` does), and its scope is the claim's own mount point, where only the node plugin,
already privileged, mounts anything: the pod gains no access to the host. Without it, a pod keeps the dead mount after
its mount pod died (`Transport endpoint is not connected`) until its container restarts. Where a policy forbids it,
give the pod a liveness probe that writes to the volume, so that it restarts onto the new mount:

```yaml
livenessProbe:
  exec:
    command: ["sh", "-c", "touch /data/.liveness"]
  periodSeconds: 30
  timeoutSeconds: 10
  failureThreshold: 2
```

Recovery does not save data: what a pod was writing when its mount process was **killed** is lost. What it was
writing when the process got `SIGTERM` is sent during the drain.

## High availability

There is no active/standby pair and no failover between nodes: each node mounts the volumes its pods use, on its own,
and every node mounting a `ReadWriteMany` claim reaches the same dataroom through the API. Scale out by running the
consumers on more nodes.

- **Node plugin restart** (upgrade, OOM kill, crash): no mount is cut, the mount pods hold them. Stages, publishes and
  repairs on that node wait for the plugin to be back. The DaemonSet rolls with `maxUnavailable: 1`.
- **Mount pod death**: that volume on that node is down until the node plugin recreates the pod (about ten seconds
  after a `kill -9`), and its pods see it again through `HostToContainer` (above).
- **Controller**: one replica by default; provisioning, expansion and deletion wait while it restarts, mounts are
  unaffected. With `controller.replicas` above 1, the chart adds `--leader-election` to csi-provisioner and csi-resizer:
  one replica works, the others wait on a lease.

## The data path

RetycFS mounts the dataroom by its ID, decrypting on read and encrypting on write with the account's AGE identity, in
the mount pod. The kernel enforces the permissions (`default_permissions`).

### Permissions

- Every file and folder belongs to the claim's `csi.retycfs.com/uid` and `csi.retycfs.com/gid` (`--uid`, `--gid`). The
  mode is the one stored with the file (`chmod` works and is kept), 0644 / 0755 for a file without one; `chown` is
  accepted and ignored.
- **A pod must run with that UID to write** (`securityContext.runAsUser`). A pod running as another UID reads (0644 /
  0755) but gets `Permission denied` on every write. The driver does nothing to check it beforehand.
- The owner is set per volume and per node, not per pod: all the pods of a claim get the same owner.
- `fsGroup` changes nothing: the driver declares `fsGroupPolicy: None` (a recursive `chown` would walk the whole
  dataroom through the API, and is ignored anyway).
- **Role of the identity**: a dataroom the identity only views (role *viewer*), or whose plan refuses writes, is
  mounted read-only. The role is read at mount time: after a role change, the volume must be remounted on each node
  (all its pods on the node gone, then back). `csi.retycfs.com/read-only` mounts read-only whatever the role.
- **Role contributor**: a contributor cannot replace an existing file by `rename` (`EACCES`), so the "write a
  temporary file, then rename it over the original" pattern of editors and many tools fails for that role.

### Consistency

- `close()` sends the file and returns the error of the upload; `fsync` sends too. A successful `close` means stored,
  unless `csi.retycfs.com/unsafe-write` is set.
- Every `close` of a file opened for writing stores a version, an unchanged file excepted; with versioning off, only
  the latest is kept.
- Opening a file reads the version current at that moment (close-to-open). Listings and names are cached for the cache
  TTL (the CLI's default, 1 min, or `csi.retycfs.com/cache-ttl`): a change made on another node or outside the cluster is
  seen after that at most.
- `rename` over an existing file is atomic on the server; `cp` of a whole file is a server-side copy; symbolic links,
  `chmod` and `utimens` work and are stored. Reads are positioned: reading the middle of a large file downloads the
  chunks it covers.
- There is no locking across nodes; concurrent writes to one file from two nodes: last upload wins.
- Files whose name matches the local-file patterns (by default `.*.swp .*.swx *~ 4913`, what vim creates around an
  edit) stay in the mount pod's spool and are never sent: invisible to other nodes and gone at unstage.

### Cache

The listing cache of the mount pods is `memory` by default: one per mount pod. With `cache.store: redis`, every mount
pod of the cluster shares one Redis (the bundled CloudPirates subchart, standalone, or your own, 7.4 or later,
standalone only), so that several nodes mounting the same dataroom fetch a folder once. Keys carry the dataroom ID:
volumes with different TTLs can share it. Without `cache.highConfidentiality`, Redis holds the **decrypted** file and
folder names; with it, only the API's ciphertexts. A Redis that fails at start fails the mount (exit `1`); once mounted,
a failing Redis falls back to a private memory cache.

## Admission policy

The chart ships a `ValidatingAdmissionPolicy` (with its binding, `validationActions: [Deny]`) on the claims of its
StorageClasses. It checks the `csi.retycfs.com/*` annotations with the formats the driver accepts, at creation and at
update, refuses a claim without `csi.retycfs.com/uid` and an update that removes it. On an update, only the annotations
that change are checked. Its reach is the claims whose `storageClassName` is one of the classes the release renders:
nothing else on the cluster, not the claims that get one of those classes later as the cluster default, not a static
PV's claim on another class. The driver still refuses an invalid claim at `CreateVolume` or at stage. See
[configuration.md](configuration.md#admission-policy) for its `failurePolicy`.

## Security

- **Encryption**: files and metadata are encrypted in the mount pod with [AGE](https://github.com/FiloSottile/age)
  post-quantum hybrid keys by `retyc-cli`, before anything leaves the node. Retyc servers only ever see ciphertext.
- **Identity**: a Retyc account per cluster (the driver's Secret) or per namespace (`retycfs-rwx-tenant`). Tokens and
  passphrases live in those Secrets, in the environment of the `retyc` processes and, for every mount pod, in a
  **copy** in the driver's namespace (`retycfs-mount-*` Secrets), including the tenants' credentials. Whoever can read
  Secrets in that namespace has every tenant's identity: keep its RBAC strict (no `get`/`list` on secrets for anyone
  but the node plugin and cluster admins), and enable encryption at rest of Secrets in etcd.
- **Spool**: a file being written is kept in cleartext in `<mount.spoolDir>/<volume-id>` on the node (0700, root)
  until it is sent; local files stay there for the life of the mount.
- **Redis**: without `cache.highConfidentiality`, it holds decrypted names: TLS, password, and no other tenant.
- **Privileges**: the node plugin and the mount pods are privileged, in a namespace with Pod Security `privileged`.
  The mount pods get no service account token.
- **Metrics**: the mount pods serve `/metrics`, `/readyz` and `/healthz` on port 9567 of their pod IP, without
  authentication (numbers, operation names, no file names). Restrict ingress to Prometheus and the node plugin with a
  NetworkPolicy (`extraObjects` in the chart).
- **Pod access**: isolation between workloads is at the claim level: one dataroom per team or application. Inside a
  volume, pods running as the owner UID write, any other UID reads what the stored modes allow.

## Limitations

- **No snapshots, no block volumes.**
- **20 GiB minimum** per dataroom reservation: smaller claims are rounded up.
- **`df` follows an expand within the cache TTL.** The dataroom is enlarged at once; a running mount caches the
  dataroom's stats for the cache TTL (1 min by default, `csi.retycfs.com/cache-ttl`) before reporting the new size.
- **Settings apply at the next mount** of the volume on a node: annotations, chart `mount.*` and `cache.*` values, the
  retyc-cli version, the role of the identity.
- **The finalizer does not protect the mount from a drain.** `kubectl drain` evicts the mount pods like any other pod
  (it needs `--force`, the mount pods having no controller, and `--delete-emptydir-data`): the mount stops after its
  drain of pending writes, and the node plugin recreates it while consumers of the volume remain on the node. A
  PodDisruptionBudget per mount pod is under consideration, not decided.
- **Writes in flight are lost** when a mount process is killed (`kill -9`, OOM kill, grace period exceeded).
- **Many small files take long to drain.** Uploads are bounded across files (`RETYC_API_CONCURRENCY_UPLOAD`, 4 by
  default): thousands of small files written just before an unstage take a minute or more (about 50 a second on a local
  API). Raising it loads the API.
- **The spool has no size limit of its own**: a full disk under `mount.spoolDir` fails the write with `ENOSPC`.
- **A killed client waits for slow operations.** The kernel interrupts a RetycFS request when its caller gets a signal,
  but the mount only honours it for a caller it can see as killed; pods are in another PID namespace, so a process
  killed during a slow operation (a large download, a throttled request) goes when that operation ends. `mount.hostPID`
  is meant to make callers visible, and is not validated.
- **One mount pod per volume and per node**: 256 MiB requested each by default. Many claims on one node add up.
- **The mount pods' logs go with them**: read them while the pod exists (see
  [troubleshooting.md](troubleshooting.md#reading-a-mount-pods-logs)).

## Known gaps and future work

- **Drain and the finalizer.** The finalizer `csi.retycfs.com/in-use` does not stop `kubectl drain` (see
  [Limitations](#limitations)); it also blocks `drain --force` on the object, and only `--disable-eviction` goes around
  it. A PodDisruptionBudget per mount pod with `minAvailable: 1` and a selector on a label of the pod is the lead
  (`maxUnavailable: 0` does not work on a pod without a controller); the decision is postponed.
- **Kubelet's timeout on a long unstage.** `NodeUnstageVolume` returns `Aborted` while the mount pod drains (5 min by
  default) and kubelet retries; how kubelet's own timeout behaves over such a drain is not measured. It is rare: `close`
  waits for the upload, so the spool holds uploads without an open file only when an application was killed during its
  `close`, or in rare cases (deletions queued with `--background-delete`, a mount detached while busy). A test with the
  API throttled to 64 kbit/s never reached it: kubelet was still waiting for the application's container to stop.
- **A slow API blocks the applications.** `close` uploads the file and returns only once it is sent, and creating a file
  can wait up to 2 min for the API. An upload that is slow but progressing has no time limit, by design: an 8 MiB chunk
  must get through a slow link. The application cannot be interrupted meanwhile, not even by `kill -9`: retyc-cli
  honours an interrupt only when the caller is dying, read from `/proc`, and the application's PID, in another pod,
  is not visible from the mount pod. The pod stays `Terminating` until the upload ends: the mount pod is not stopping, so
  its drain timeout does not apply. With an upload that stalls without the connection dropping, it stays blocked
  indefinitely.
  `hostPID: true` on the mount pods would make the interrupts work, but it is rejected: the Pod Security Standards
  forbid it from `baseline` on, and it would expose the `/proc` of every process of the node. The lead is in retyc-cli:
  cancelling an upload that has sent nothing for some time, so that `close` returns an error and the upload is retried
  in the background.
- **Spool journal (retyc-cli).** After a crash, what the spool held is not sent again at the next start (the node plugin
  empties it); a journal of the spools would allow it.
- **Resuming a RetycFS mount (retyc-cli).** Handing the open `/dev/fuse` descriptor to a new process would restart or
  upgrade a mount without a cut. A large change that needs a patch of go-fuse.
