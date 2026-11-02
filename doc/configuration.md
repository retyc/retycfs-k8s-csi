# Configuration

Everything below is exposed by the Helm chart (`charts/retycfs-csi`); the value names are in its
[README](../charts/retycfs-csi/README.md) and in `values.yaml`. Kubernetes 1.34 or later is required
(VolumeAttributesClass, `ValidatingAdmissionPolicy` v1).

## PVC annotations

A claim on a Retyc StorageClass is configured by annotations. All of them but `initial-members` are read **at stage**,
when a node mounts the volume: a change applies at the next mount of the volume on each node, never to a running mount
pod (see [architecture.md](architecture.md#settings-read-at-stage)). `csi.retycfs.com/uid` is also checked by
`CreateVolume`, and `initial-members` is read there only.

| Annotation                             | Required                | Value                                                                                                                                                                | Effect                                                                                                                                                                                                                               |
|----------------------------------------|-------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `csi.retycfs.com/uid`                  | **yes**                 | integer, 0 to 4294967295 (`"0"` for root, written explicitly)                                                                                                        | owner UID of every file (`--uid`): the UID the pods must run as to write                                                                                                                                                             |
| `csi.retycfs.com/gid`                  | no, defaults to the UID | integer                                                                                                                                                              | owner GID (`--gid`)                                                                                                                                                                                                                  |
| `csi.retycfs.com/cache-ttl`            | no                      | positive Go duration (`30s`, `5m`, `1h30m`)                                                                                                                          | how long listings and names are cached (`--cache-ttl`); without it, the CLI's default (1 min)                                                                                                                                        |
| `csi.retycfs.com/background-delete`    | no                      | `true` / `false`                                                                                                                                                     | `unlink` returns at once and the delete is sent afterwards (`--background-delete`): much faster `rm -r`, but a successful `rm` no longer proves the file is gone on the server, and deletes not sent at unstage count as lost writes |
| `csi.retycfs.com/unsafe-write`         | no                      | `true` / `false`                                                                                                                                                     | the API acknowledges an upload before the chunk is stored (`--unsafe-write`): faster `close`, but a store that fails afterwards is reported to nobody                                                                                |
| `csi.retycfs.com/read-only`            | no                      | `true` / `false`                                                                                                                                                     | mounts read-only whatever the identity's role (`--read-only`)                                                                                                                                                                        |
| `csi.retycfs.com/local-file`           | no                      | patterns separated by spaces, or `none` alone                                                                                                                        | names of files kept in the mount pod's spool and never sent (`--local-file`); replaces the CLI's default `.*.swp .*.swx *~ 4913`; `none` sends every file, and cannot be combined with patterns                                      |
| `csi.retycfs.com/mount-cpu-request`    | no                      | positive Kubernetes quantity (`100m`)                                                                                                                                | CPU request of the volume's mount pods                                                                                                                                                                                               |
| `csi.retycfs.com/mount-cpu-limit`      | no                      | quantity                                                                                                                                                             | CPU limit (none by default)                                                                                                                                                                                                          |
| `csi.retycfs.com/mount-memory-request` | no                      | quantity (`512Mi`)                                                                                                                                                   | memory request                                                                                                                                                                                                                       |
| `csi.retycfs.com/mount-memory-limit`   | no                      | quantity                                                                                                                                                             | memory limit                                                                                                                                                                                                                         |
| `csi.retycfs.com/initial-members`      | no                      | `alice@acme.fr:admin,bob@acme.fr:editor,carol@acme.fr`: comma-separated e-mail addresses, each optionally followed by `:admin`, `:editor` or `:viewer` (the default) | people added to the dataroom when the driver creates it. See [Initial members](#initial-members)                                                                                                                                     |

Example:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: shared-docs
  annotations:
    csi.retycfs.com/uid: "1000"
    csi.retycfs.com/cache-ttl: 30s
    csi.retycfs.com/mount-memory-limit: 1Gi
spec:
  accessModes: [ ReadWriteMany ]
  storageClassName: retycfs-rwx
  resources:
    requests:
      storage: 20Gi
---
# ... in the pods using it:
securityContext:
  runAsUser: 1000          # the claim's csi.retycfs.com/uid, or no write
  runAsGroup: 1000
containers:
  - volumeMounts:
      - name: data
        mountPath: /data
        mountPropagation: HostToContainer   # sees the volume again after a mount pod died
```

What to know:

- **The pod must run with the UID of the annotation**, or it cannot write: it reads, and gets `Permission denied` on
  every write. Nothing checks it beforehand.
- An identity with the **viewer** role on the dataroom gets a read-only mount. A role changed since the mount needs a
  remount of the volume on each node.
- With the **contributor** role, replacing a file by `rename` fails (`EACCES`): the "temporary file, then rename"
  pattern of editors and of many tools does not work.
- Every `close` of a written file stores a version (see [VolumeAttributesClass](#volumeattributesclass-versioning)).
- **StatefulSets**: put the annotation in the `volumeClaimTemplates`, which is where the claims get it:

  ```yaml
  volumeClaimTemplates:
    - metadata:
        name: data
        annotations:
          csi.retycfs.com/uid: "1000"
      spec:
        accessModes: [ReadWriteMany]
        storageClassName: retycfs-rwx
        resources:
          requests:
            storage: 20Gi
  ```

- <a id="initial-members"></a>**Initial members.** Retyc is the source of truth for who can open a dataroom. The
  annotation is read **only when the driver creates the dataroom** and is never applied again: changing it later, or
  adopting an existing dataroom, changes nothing. After that, manage the members in Retyc. An entry the driver cannot
  add (a refusal of the API) does not fail the volume: the claim gets a `Warning` event
  (`InitialMembers`) and the driver does not retry. A claim whose list has no `admin` gets a `NoHumanAdmin` warning,
  see [Dataroom lifecycle](#dataroom-lifecycle-and-ownership-transfer). A malformed value is refused at creation.
  The members are added in the background, right after the creation (5 minutes at most): a controller that stops
  finishes them first, which its `terminationGracePeriodSeconds` of 330 s allows.
- `--mirror-locks` is not exposed: after a mount pod is recreated, the locks of the dead process would keep the new one
  from opening the files (`EBUSY`) until their lease ends.

## Static PVs

A dataroom that already exists (retained from a deleted claim, or created with `retyc dataroom create`) becomes a
volume through a static PV whose `volumeHandle` is the dataroom ID:

```sh
retyc --json dataroom ls          # id + title
```

The mount settings of a static PV are read from its `volumeAttributes`, with the **same keys as the annotations**
(`csi.retycfs.com/uid` is required there too). Add `pvcName` and `pvcNamespace` to have the claim's annotations read as
well: they then win over `volumeAttributes`. Without them, only `volumeAttributes` count (on a tenant class they are
required, see below). The chart's admission policy
requires `csi.retycfs.com/uid` on the claim of a static PV on its classes, so put it on both. Fill in and apply
[`examples/manifests/static-pv.yaml`](../examples/manifests/static-pv.yaml), with
`persistentVolumeReclaimPolicy: Retain`: the provisioner
never deletes volumes it did not create.

**A static PV of a tenant class needs its own `nodeStageSecretRef`.** kubelet takes the node-stage secret from the PV,
not from the StorageClass: the provisioner copies it there for the volumes it creates, but a static PV only has what
you write. Without it, kubelet stages the volume with no credentials, and the driver would fall back to its
cluster-wide identity, another Retyc machine than the tenant's. So when a stage comes without secrets, the node plugin
reads every PV of the driver with the volume handle (the claim's own PV first) and their StorageClasses, and refuses
with `FailedPrecondition` if the class declares
`csi.storage.k8s.io/node-stage-secret-name`, or if the PV itself names a node-stage secret kubelet did not send. The
consumer pod then stays in `ContainerCreating` with that message. Name the tenant's identity Secret in the PV, which
lives in the driver's namespace, and the claim in
`volumeAttributes`: the node plugin checks that the identity was enrolled for the claim's namespace, and refuses a
tenant identity with no claim (`FailedPrecondition ... names no claim namespace`):

```yaml
  csi:
    driver: csi.retycfs.com
    volumeHandle: <dataroom ID>
    volumeAttributes:
      csi.retycfs.com/uid: "1000"
      pvcName: <claim>
      pvcNamespace: <the claim's namespace>
    nodeStageSecretRef:
      name: retycfs-identity-<the claim's namespace>
      namespace: retycfs-system
```

The tenant's machine also needs access to the dataroom: a dataroom created by another machine (or by a person) is
not open to it until an administrator of the dataroom adds it in Retyc.

Every PV of the handle must pass: a global PV written on the same dataroom does not let a tenant one through. A
handle with no PV at all is refused too (kubelet only stages a volume that has one). A PV whose class no longer
exists, or that has none, is not refused. The node plugin needs `get` and `list` on
PersistentVolumes and `get` on StorageClasses (in the chart's ClusterRole).

## VolumeAttributesClass (versioning)

The `versioning` parameter of a VolumeAttributesClass (`true` or `false`) sets the dataroom's versioning, at creation
and whenever the claim switches classes (`ControllerModifyVolume`). It is the only parameter: any other key is refused.

```yaml
apiVersion: storage.k8s.io/v1
kind: VolumeAttributesClass
metadata:
  name: retycfs-versioned
driverName: csi.retycfs.com
parameters:
  versioning: "true"
```

```sh
kubectl patch pvc shared-docs -p '{"spec":{"volumeAttributesClassName":"retycfs-unversioned"}}'
```

**Disabling versioning is destructive**: the driver confirms it without asking, and the next completed upload of each
file deletes its older versions. For a tenant class, the resizer authenticates with the controller-expand secret. See
[`examples/manifests/volume-attributes-class.yaml`](../examples/manifests/volume-attributes-class.yaml).

## StorageClasses

The chart ships three classes (`storageClasses` value), all with `allowVolumeExpansion: true`:

| Name                 | Identity              | `reclaimPolicy` | `volumeBindingMode` |
|----------------------|-----------------------|-----------------|---------------------|
| `retycfs-rwx`        | cluster-wide          | `Delete`        | `Immediate`         |
| `retycfs-rwx-retain` | cluster-wide          | `Retain`        | `Immediate`         |
| `retycfs-rwx-tenant` | the claim's namespace | `Retain`        | `Immediate`         |

The classes without `tenant: true` are only rendered when a cluster-wide identity is configured
(`identity.organizationID`). The driver
has no StorageClass parameter of its own: only the sidecars' `csi.storage.k8s.io/*` keys are accepted, anything else is
refused at `CreateVolume`. Mount settings belong to the claims' annotations.

The size requested is reserved on the owner's plan, with a 20 GiB floor: a `1Gi` claim gets a `20Gi` PV. Raising the
request enlarges the reservation (`ControllerExpandVolume`); shrinking is not possible. A running mount reports the new
size in `df` within the cache TTL (1 min by default). The filesystem access modes `ReadWriteOnce`, `ReadOnlyMany` and
`ReadWriteMany` are accepted, as is `ReadWriteOncePod` (not block volumes); a read-only publish is bind-mounted
read-only.

`ReadWriteOncePod` is supported; Kubernetes enforces the single pod, the driver mounts it like any other volume;
exclusivity only holds inside the cluster: the dataroom stays reachable from the Retyc web app or another cluster (same
as `ReadWriteOnce`).

## Identities

The driver holds no token and no passphrase. Each identity is a Retyc **machine account**, obtained by enrollment: the
`enroller` (a separate Deployment, with its own service account) runs `retyc machine enroll` for every `RetycIdentity`,
keeps the keys in a Secret of the driver's namespace and checks the machine regularly. No secret travels through the
chart, the values or the cluster's users.

There are two kinds, the same object:

|                             | Cluster-wide                                                                                        | Tenant                                                      |
|-----------------------------|-----------------------------------------------------------------------------------------------------|-------------------------------------------------------------|
| `RetycIdentity`             | `default`, in the driver's namespace, created by the enroller when `identity.organizationID` is set | `default`, in the tenant's namespace, applied by the tenant |
| Secret (driver's namespace) | `retycfs-identity`                                                                                  | `retycfs-identity-<namespace>`                              |
| Used by                     | `retycfs-rwx`, `retycfs-rwx-retain`                                                                 | `retycfs-rwx-tenant`                                        |

### The RetycIdentity

```yaml
apiVersion: csi.retycfs.com/v1alpha1
kind: RetycIdentity
metadata:
  name: default          # imposed: one per namespace
  namespace: team-a
spec: # optional
  organizationID: <Retyc organization UUID>
  displayName: team-a    # name of the machine in Retyc, default <identity.clusterName>/<namespace>; no "/"
```

- The name must be `default`, one per namespace.
- `spec.organizationID` defaults to `identity.organizationID` of the chart, and is **immutable**: to change it, delete
  and recreate the object (a new machine). Without either, the phase is `Rejected`.
- Deleting the object never deletes the machine on the Retyc side: disable and remove it there (see
  [Dataroom lifecycle](#dataroom-lifecycle-and-ownership-transfer)).

### Enrollment and approval

1. The enroller requests a machine account. The `RetycIdentity` shows phase `Pending`, the request's fingerprint, and
   the approval link (`-o wide`):

   ```sh
   kubectl get retycidentity -A
   kubectl -n team-a get retycidentity default -o wide
   ```

   | Column | |
      |---|---|
   | `PHASE` | `Pending`, `Approved`, `Denied`, `Expired`, `Rejected` or `Blocked` |
   | `FINGERPRINT` | the request's fingerprint, to compare before approving |
   | `ACTIVE` | once approved, whether Retyc still accepts the machine (`False`: disabled or deleted there, checked every 5 minutes) |
   | `REASON` (`-o wide`) | why it is not active (`MachineRefused`: disabled or deleted in Retyc) |
   | `MACHINE` | the machine ID in Retyc, once approved |
   | `APPROVAL` (`-o wide`) | where an administrator approves |

2. An **administrator of the Retyc organization** compares the fingerprint with the one the cluster shows, then
   approves,
   in Retyc (Organization > Machines) or with the CLI:

   ```sh
   retyc machine requests
   retyc machine approve <enrollment-id> --fingerprint XXXX-XXXX-XXXX
   ```

   When the organization has no master key, Retyc asks the administrator to acknowledge that the machine's datarooms
   are recoverable only through the members it adds (the CLI asks for a confirmation, which `-y` gives): see
   [Initial members](#initial-members) and [Dataroom lifecycle](#dataroom-lifecycle-and-ownership-transfer).

3. The enroller stores the keys in the identity's Secret. The phase becomes `Approved`.

| Phase      | Meaning                                                                                                                          | What to do                                                                                                                                                                                                                            |
|------------|----------------------------------------------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `Pending`  | request awaiting approval (it lives 15 minutes; the enroller sends a new one when it expires)                                    | approve it                                                                                                                                                                                                                            |
| `Approved` | the machine is enrolled; the condition `Active` tells whether Retyc still accepts it                                             |                                                                                                                                                                                                                                       |
| `Expired`  | four requests in a row, 15 minutes each, went unanswered                                                                         | `kubectl -n <ns> annotate retycidentity default csi.retycfs.com/retry=$(date +%s) --overwrite` starts a new series                                                                                                                    |
| `Denied`   | an administrator refused the request. Terminal                                                                                   | delete and recreate the `RetycIdentity`                                                                                                                                                                                               |
| `Rejected` | no organization (`spec.organizationID` and `identity.organizationID` both empty), or Retyc refused the configuration (exit `78`) | no organization: set `identity.organizationID` on the chart (the next pass enrolls) or recreate the `RetycIdentity` with `spec.organizationID` (it cannot be added later). Configuration refused: fix it, then the `retry` annotation |
| `Blocked`  | the Secret of an older identity of that namespace, still used by PVs, is in the way (see below)                                  | see [Namespace deleted and recreated](#namespace-deleted-and-recreated)                                                                                                                                                               |

`kubectl describe retycidentity default` and the namespace's Events carry the details. A tenant's PVC created before the
approval stays `Pending`, with an Event giving the fingerprint to compare.

The `Active` condition comes from `retyc auth status` run as the machine every 5 minutes. A machine **disabled or
deleted in Retyc** sets `Active=False` (reason `MachineRefused`): its mounts fail with exit `77`. The machine cannot
tell the two
apart; an administrator can (Organization > Machines, or `retyc machine ls`). Disabled: once it is enabled again, the
mount pods try again by themselves within 15 minutes. Deleted: delete the `RetycIdentity` to enroll a new one. Exit `78`
is a configuration error
(see [troubleshooting.md](troubleshooting.md#exit-codes-of-a-mount-pod)).

### Cluster-wide identity

```sh
kubectl create namespace retycfs-system
kubectl label namespace retycfs-system pod-security.kubernetes.io/enforce=privileged
helm install retycfs-csi retyc/retycfs-csi --namespace retycfs-system --set identity.organizationID=<uuid>
```

The enroller creates the `RetycIdentity` `default` of `retycfs-system` itself. Until it is approved, the claims of
`retycfs-rwx*` wait (`Pending`, with an Event that gives the fingerprint); the pods of the driver are ready all the
same,
so that `helm install --wait` does not wait for the approval. Deleting the object enrolls a new machine
(a new request); the former one stays in Retyc until it is disabled there. Changing `identity.organizationID` later
does not move the existing object (`organizationID` is immutable): delete it, and the enroller recreates it in the new
organization.

### Per-tenant identities

`retycfs-rwx-tenant` uses the standard CSI secret parameters, templated on the claim's namespace and pointing into the
**driver's** namespace:

```yaml
parameters:
  csi.storage.k8s.io/provisioner-secret-name: retycfs-identity-${pvc.namespace}
  csi.storage.k8s.io/provisioner-secret-namespace: retycfs-system
  csi.storage.k8s.io/node-stage-secret-name: retycfs-identity-${pvc.namespace}
  csi.storage.k8s.io/node-stage-secret-namespace: retycfs-system
  csi.storage.k8s.io/controller-expand-secret-name: retycfs-identity-${pvc.namespace}
  csi.storage.k8s.io/controller-expand-secret-namespace: retycfs-system
```

A tenant applies a `RetycIdentity` `default` in its namespace (with or without `spec.organizationID`, depending on
`identity.organizationID`), has it approved, and its claims on the class are provisioned by its own machine. The
provisioner reads the Secret for `CreateVolume` and `DeleteVolume`, the resizer for expansion and VolumeAttributesClass
changes, kubelet for `NodeStageVolume`. The tenant never reads the Secret, which sits in a namespace it has no access
to.

What to know before rolling it out:

- the rights on `RetycIdentity` are aggregated to the built-in roles: `edit` and `admin` get write access, `view` read
  access (`rbac.create`). Nobody gets write access to the object's status;
- a claim created before the approval stays `Pending`, with the fingerprint in its Events;
- the class is `Retain` on purpose: a tenant that deletes its `RetycIdentity` before its claims would otherwise leave
  PVs stuck on a `DeleteVolume` that can no longer authenticate. Copy the class with `Delete` if that trade-off is
  acceptable;
- each mount pod gets a **copy** of the identity in a Secret of the driver's namespace: keep the RBAC of that namespace
  strict and Secrets encrypted at rest in etcd (see [architecture.md](architecture.md#security));
- an identity's change (a recreated `RetycIdentity`) applies to the mount pods at the next mount of each volume
  (another config hash);
- with a shared Redis, set `cache.highConfidentiality: true` (NOTES warns otherwise).

#### Namespace deleted and recreated

The Secret records the UID of the namespace it was enrolled for. A recreated namespace does not inherit the former
identity: its `RetycIdentity` enrolls a new machine. The former Secret stays as long as PVs use it; while it does, the
new object is `Blocked`. Delete those PVs (or their claims) to release it.

### Admission policy on the identities' Secrets

The `<release>-identity-secrets` policy (`admissionPolicy.enabled`) lets only the enroller's service account create,
update or delete Secrets of type `csi.retycfs.com/machine-identity` in the driver's namespace, and the enroller write
no other Secret than `retycfs-identity` and `retycfs-identity-*`. Members of `system:masters` and the namespace
controller (deleting a namespace) pass. A cluster administrator outside `system:masters` who must repair a Secret by
hand disables `admissionPolicy` for the time of the repair. This policy always fails closed.

Examples, with a namespace, its `RetycIdentity`, a claim and two pods: [
`examples/manifests/tenant.yaml`](../examples/manifests/tenant.yaml).

## Dataroom lifecycle and ownership transfer

A dataroom **belongs to the machine account that created it**: the one of the claim's namespace for a tenant class, the
cluster's for the other classes. A machine cannot be removed from Retyc while it owns datarooms, and a recreated
`RetycIdentity` is another machine, with no access to the former volumes. Plan the ownership before the first claim.

1. **Have a human administrator of the dataroom.** Put people in `csi.retycfs.com/initial-members` when the claim is
   created (`alice@acme.fr:admin`), or add them in Retyc **while the machine is still active**. Without an `admin` in
   the list, the claim gets a `NoHumanAdmin` warning: only the machine could then transfer the dataroom.
2. **Transfer the ownership.** An administrator of the Retyc organization runs, for each dataroom:

   ```sh
   retyc admin dataroom chown <dataroom-id> <user-id>
   ```

3. **Delete the claims and PVs.** With `Retain` the dataroom stays in Retyc. Then delete the `RetycIdentity`
   (`kubectl -n <ns> delete retycidentity default`): the enroller deletes the Secret at once when no PV uses it;
   otherwise it keeps it, and its sweep (at startup, then
   every hour) removes it once the last PV is gone.
4. **Remove the machine** in Retyc:

   ```sh
   retyc machine disable <machine-id>
   retyc machine rm <machine-id>      # refused while the machine still owns datarooms
   ```

**Do not disable the machine before step 1.** A disabled machine can no longer be used to add members, which blocks the
transfer: enable it again for the time of step 1.

To keep using a dataroom after recreating a `RetycIdentity`, a human administrator of that dataroom adds the new machine
to it, then a static PV adopts the dataroom (see [Static PVs](#static-pvs)).

## Uninstall

`helm uninstall` removes the enroller, but every `RetycIdentity` carries the finalizer `csi.retycfs.com/identity` and
the
CRD is kept (`helm.sh/resource-policy: keep`). Without an enroller, deleting a `RetycIdentity` or a tenant namespace
stays `Terminating`. Remove the identities first:

1. **Handle the datarooms to keep**:
   see [Dataroom lifecycle and ownership transfer](#dataroom-lifecycle-and-ownership-transfer).
2. **Delete the PVCs and PVs.**
3. **Delete the identities** and wait until they are gone (the enroller deletes the Secrets no PV references any more,
   then removes the finalizer; a Secret still referenced waits for the sweep, at startup then every hour, after its PVs
   are gone, or step 5):

   ```sh
   kubectl delete retycidentity --all -A
   kubectl get retycidentity -A
   ```

4. **Uninstall the chart**: `helm uninstall <release> -n <driver-namespace>`.
5. **Clean the driver namespace**: delete the remaining `retycfs-identity*` Secrets and, if wanted, the CRD
   (`kubectl delete crd retycidentities.csi.retycfs.com`).
6. **In Retyc**, disable and remove the machines (`retyc machine disable <machine-id>`,
   `retyc machine rm <machine-id>`).

If an identity is stuck (enroller already gone), remove the finalizer by hand. The Secret is then not cleaned up:

```sh
kubectl -n <ns> patch retycidentity default --type=merge -p '{"metadata":{"finalizers":null}}'
```

## Chart values

The main ones; the full list is in the chart's README.

### Identity

| Value                       | Default      | Description                                                                                                                                                                                                                                                                                 |
|-----------------------------|--------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `identity.organizationID`   | `""`         | UUID of the Retyc organization of the cluster-wide identity. Set, the enroller creates the `RetycIdentity` `default` of the release namespace, and `retycfs-rwx` and `retycfs-rwx-retain` are rendered. Also the organization of the tenants' `RetycIdentity` without `spec.organizationID` |
| `identity.clusterName`      | `kubernetes` | prefix of the machines' default display name in Retyc, `<clusterName>/<namespace>`                                                                                                                                                                                                          |
| `enroller.*`                |              | the enroller's resources (32 MiB requested, 256 MiB limit), scheduling, `extraEnv`, `extraVolumes`, `hostAliases` and `extraArgs`                                                                                                                                                           |
| `serviceAccount.enroller.*` | create       | the identity policy only lets this service account write the identities' Secrets                                                                                                                                                                                                            |

Both optional: without `identity.organizationID`, the StorageClasses without `tenant: true` are not installed, only
per-namespace identities are served, and NOTES says so. See [Identities](#identities).

The enroller runs one replica, replaced rather than rolled (`Recreate`), non-root, with a read-only root filesystem: the
keys of the enrollments under way only touch its in-memory `/tmp`. It needs HTTPS to `api.retyc.com` and
`auth.retyc.com`, like the controller.

Never mount a `token.json` (from `retyc auth login`) into the driver pods: the CLI would use the token stored in its
config directory.

### Mount pods

| Value                                           | Default                                                         | Description                                                                                                                                                                      |
|-------------------------------------------------|-----------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `mount.image.repository` / `tag` / `pullPolicy` | `ghcr.io/retyc/retyc-cli`, the Dockerfile's `RETYC_CLI_VERSION` | image of the mount pods; CI checks it matches the controller's embedded CLI                                                                                                      |
| `mount.imagePullSecrets`                        | `[]`                                                            | in the driver's namespace                                                                                                                                                        |
| `mount.resources`                               | 50m and 96Mi requested, 384Mi limit                             | default resources, overridden per claim by the `csi.retycfs.com/mount-*` annotations; raise the memory limit for large trees (see [architecture.md](architecture.md#mount-pods)) |
| `mount.drainTimeout`                            | `5m`                                                            | `--drain-timeout`: how long a stopping mount pod may take to send its pending writes; the pod's grace period is 10 s more                                                        |
| `mount.spoolDir`                                | `/var/lib/retycfs-csi/spool`                                    | host directory of the spools, one subdirectory per volume, files in cleartext until sent                                                                                         |
| `mount.priorityClassName`                       | `""` (`system-node-critical`)                                   |                                                                                                                                                                                  |
| `mount.extraEnv`                                | `[]`                                                            | extra variables of the CLI, after the driver's: they can override `RETYC_*` settings such as `RETYC_API_BASE_URL` or `RETYC_API_CONCURRENCY_UPLOAD`                              |
| `mount.hostAliases`                             | `[]`                                                            | extra `/etc/hosts` entries                                                                                                                                                       |
| `mount.hostPID`                                 | `false`                                                         | host PID namespace; not needed (kept for diagnosis)                                                                                                                              |

The chart writes these settings into the `<release>-mount-config` ConfigMap (`mount.json`), which the node plugin reads
at start; a change rolls the node plugin, cuts no mount, and applies to the mount pods created from then on.

### Cache

| Value                                                               | Default  | Description                                                                                                                                                                                           |
|---------------------------------------------------------------------|----------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `cache.store`                                                       | `memory` | `memory`: one cache per mount pod; `redis`: one Redis shared by every mount pod of the cluster                                                                                                        |
| `cache.highConfidentiality`                                         | `false`  | `--cache-redis-high-confidentiality`: Redis holds only ciphertexts (about 19 MB per 10,000 files instead of 2.3 MB, plus a decryption per name on each pod). Set it with several tenants on one Redis |
| `cache.redis.subchart.enabled`                                      | `false`  | deploy the CloudPirates Redis chart (standalone, no persistence, `maxmemory` with `allkeys-lru`, values under `redis`); only with `cache.store: redis`                                                |
| `cache.redis.subchart.password`                                     | `""`     | Redis password (letters, digits, `-._~`); empty generates one at install and keeps it across upgrades                                                                                                 |
| `cache.redis.subchart.existingSecret`                               | `""`     | a Secret you manage instead, with the keys `redis-password` (the password) and `RETYC_CACHE_REDIS_URL` (`redis://:<password>@<release>-redis:6379`); the chart then creates no Redis Secret           |
| `cache.redis.external.url` / `existingSecret` / `existingSecretKey` |          | your own Redis (7.4 or later, standalone, `rediss://` for TLS), as a URL the chart stores in a Secret, or an existing Secret                                                                          |

The Redis URL never goes into the ConfigMap: the node plugin reads it from a Secret (`RETYC_CACHE_REDIS_URL`) and copies
it into each mount pod's own Secret.

**GitOps (Argo CD and other `helm template` renderers): set `cache.redis.subchart.password`, or use
`cache.redis.subchart.existingSecret`.** The generated password is kept across upgrades with Helm's `lookup`, which
returns nothing outside `helm install`/`upgrade`: each render would then produce a new password and change the Redis
Secret, while the Redis StatefulSet keeps the former one. The mount pods would then fail to authenticate, and get
another config than the running ones.

A complete Redis setup (subchart, high confidentiality, sizing, or your own Redis):
[`examples/helm-values/redis-values.yaml`](../examples/helm-values/redis-values.yaml).

### Admission policy

| Value                           | Default | Description                                                                         |
|---------------------------------|---------|-------------------------------------------------------------------------------------|
| `admissionPolicy.enabled`       | `true`  | `ValidatingAdmissionPolicy` and binding on the claims of the chart's StorageClasses |
| `admissionPolicy.failurePolicy` | `Fail`  | `Fail` or `Ignore`: what the API server does when the policy cannot be evaluated    |

The policy refuses, with the reason, a claim without `csi.retycfs.com/uid`, an update that removes it, and any
`csi.retycfs.com/*` annotation whose value the driver would refuse. It is evaluated inside the API server (no webhook,
nothing to be unavailable), with `failurePolicy: Fail` by default: a policy that cannot be evaluated (a CEL error, a
missing parameter) refuses the request instead of letting it through. `Ignore` lets such a request through, unchecked
(the driver still refuses an invalid claim, later). Its reach is limited by a match condition on
`storageClassName`: only the claims of the classes this release renders, at `CREATE` and `UPDATE` (not their status).
A broken policy therefore blocks the Retyc claims only, never other storage. It does not see a claim that gets one of
these classes later as the cluster default, nor a static PV's claim on another class: the driver still refuses an
invalid claim at `CreateVolume` (`InvalidArgument`) or at stage.

### Controller

| Value                                                  | Default     | Description                                                                                                                          |
|--------------------------------------------------------|-------------|--------------------------------------------------------------------------------------------------------------------------------------|
| `controller.replicas`                                  | `1`         | above 1, `--leader-election` is added to csi-provisioner and csi-resizer (with a Role on leases): one replica works, the others wait |
| `controller.affinity` / `topologySpreadConstraints`    | `{}` / `[]` | spread the replicas (an anti-affinity example is in `values.yaml`)                                                                   |
| `controller.extraEnv` / `extraVolumes` / `hostAliases` |             | egress proxy, private CA bundle, extra hosts                                                                                         |

### Namespace

The node plugin and the mount pods are privileged: the release namespace must allow it with Pod Security
`privileged`. Use a namespace of its own, `retycfs-system`:

```sh
kubectl create namespace retycfs-system
kubectl label namespace retycfs-system pod-security.kubernetes.io/enforce=privileged
```

Avoid `kube-system`: the controller, the enroller and the node plugin read the Secrets of their namespace, which hold
every machine
account's keys, and `kube-system` usually has many other readers (NOTES warns).

## Driver flags

| Flag                        | Default                                     | Description                                                                                        |
|-----------------------------|---------------------------------------------|----------------------------------------------------------------------------------------------------|
| `--mode`                    |                                             | `controller`, `node` or `enroller` (required)                                                      |
| `--endpoint`                | `unix:///csi/csi.sock`                      | CSI gRPC endpoint; only `unix://` is supported                                                     |
| `--retyc-bin`               | `retyc`                                     | path to the `retyc` CLI binary (controller)                                                        |
| `--http-endpoint`           | `:9808`                                     | address of the `/healthz`, `/readyz` and `/metrics` endpoints; empty disables them                 |
| `--node-id`                 | `$NODE_ID`, `$NODE_NAME`, then the hostname | Kubernetes name of the node, reported by `NodeGetInfo`; the mount pods run there (node mode)       |
| `--mount-config`            | `/etc/retycfs-csi/mount.json`               | JSON configuration of the mount pods (node mode)                                                   |
| `--namespace`               | `$POD_NAMESPACE`                            | namespace of the mount pods and of the identities' Secrets, the driver's (node and enroller modes) |
| `--identity-dir`            | `/var/run/retycfs-csi/identity`             | where the cluster-wide identity's Secret is mounted (optional volume), read at each use            |
| `--default-organization-id` |                                             | organization of the `RetycIdentity` without `spec.organizationID` (enroller mode)                  |
| `--cluster-name`            | `kubernetes`                                | prefix of the machines' display name (enroller mode)                                               |

`klog` flags (`-v`, `--logtostderr`, ...) are accepted as well. The three modes need an in-cluster Kubernetes client:
the
controller reads the claims, the node plugin the claims and the mount pods, the enroller the `RetycIdentity` objects.

### `mount.json`

Written by the chart from the `mount.*` and `cache.*` values. Unknown keys fail the start.

| Key                                                                                              | Default                           | Description                                                                 |
|--------------------------------------------------------------------------------------------------|-----------------------------------|-----------------------------------------------------------------------------|
| `image`                                                                                          | (required)                        | retyc-cli image of the mount pods                                           |
| `cliVersion`                                                                                     | the image tag                     | version recorded on the pods, compared for `retycfs_csi_mount_pod_outdated` |
| `imagePullPolicy`, `imagePullSecrets`, `extraEnv`, `hostAliases`, `hostPID`, `priorityClassName` |                                   | copied into every mount pod                                                 |
| `resources`                                                                                      | 50m / 96Mi requested, 384Mi limit | default resources of the mount container                                    |
| `kubeletDir`                                                                                     | `/var/lib/kubelet`                | staging paths must be under `<kubeletDir>/plugins`                          |
| `spoolDir`                                                                                       | `/var/lib/retycfs-csi/spool`      |                                                                             |
| `drainTimeout`                                                                                   | `5m`                              | Go duration                                                                 |
| `metricsPort`                                                                                    | `9567`                            | port of the CLI's `/healthz`, `/readyz` and `/metrics`                      |
| `redis.url`, `redis.highConfidentiality`                                                         |                                   | Redis cache; the node plugin's `RETYC_CACHE_REDIS_URL` overrides the URL    |

### Environment

| Variable                     | Component | Value                                                      |
|------------------------------|-----------|------------------------------------------------------------|
| `NODE_ID`, `POD_NAMESPACE`   | node      | downward API                                               |
| `POD_NAMESPACE`, `HOME=/tmp` | enroller  | downward API; the CLI's home, in memory                    |
| `RETYC_CACHE_REDIS_URL`      | node      | from the Redis Secret, copied into the mount pods' Secrets |

The controller passes its environment to every `retyc` subprocess, with
`RETYC_MACHINE_DIR` set to a `0700` temporary directory (in memory) holding the identity of the request. Every mount pod
projects the files of its own Secret (the cluster-wide identity, or a copy of the tenant's) and gets `RETYC_MACHINE_DIR`
on them, `RETYC_CONFIG_DIR`, `RETYC_CACHE_REDIS_URL` with Redis, then `mount.extraEnv`.

## Health endpoints, probes and resources

The three components serve `/healthz` (liveness) and `/readyz` (readiness) on `--http-endpoint` (port `9808`, container
port `health`), and the CSI `Probe` RPC reports the same readiness.

All three are ready as soon as they serve. The state of the identities shows on the `RetycIdentity` objects (phase,
`Active` condition), not on the pods: a readiness that waited for an approval would hold `helm install --wait`, Flux or
Argo CD until a person approves. The node plugin's readiness depends on no mount pod either: a volume that cannot mount
shows on its claim and its pods.

| Probe     | Path       | Period           | Meaning                                                           |
|-----------|------------|------------------|-------------------------------------------------------------------|
| liveness  | `/healthz` | 30 s, 5 failures | the process serves HTTP at all                                    |
| readiness | `/readyz`  | 10 s, 2 failures | the process serves (the identities show on their `RetycIdentity`) |

The node plugin runs no `retyc` process: 32 MiB requested, 128 MiB limit. The controller: 64 MiB requested, 512 MiB
limit. The enroller: 32 MiB requested, 256 MiB limit (one `retyc machine enroll` process per pending enrollment). The
mount pods' resources are under `mount.resources`.

## Metrics

Every driver container (controller, node plugin, enroller) serves Prometheus metrics on `/metrics`, port `9808` (
container port `health`). Every mount pod
serves the CLI's on port `9567` (container port `metrics`). With the Prometheus Operator,
`metrics.podMonitor.enabled=true` creates two `PodMonitor`s, one for the driver components and one for the mount pods
(selected by `app.kubernetes.io/name=retycfs-mount`); add the labels your Prometheus selects on in
`metrics.podMonitor.labels`.

| Metric                                                                                                                                                                                                               | Where       | Meaning                                                                                                |
|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------|--------------------------------------------------------------------------------------------------------|
| `go_*`, `process_*`                                                                                                                                                                                                  | driver      | runtime of the driver process                                                                          |
| `retycfs_csi_build_info{version, mode}`                                                                                                                                                                              | driver      | constant 1                                                                                             |
| `retycfs_csi_mount_pods{state}`                                                                                                                                                                                      | node plugin | mount pods of the node by state: `pending`, `running` (not ready), `ready`, `terminating`, `exited`    |
| `retycfs_csi_mount_pod_outdated{reason="cli_version"}`                                                                                                                                                               | node plugin | mount pods running another retyc-cli version than the configured one; they take it at their next mount |
| `retycfs_ops_total{op, result}`, `retycfs_op_duration_seconds{op}`, `retycfs_bytes_total{direction}`, `retycfs_open_handles{kind}`                                                                                   | mount pod   | filesystem operations                                                                                  |
| `retycfs_cache_lookups_total`, `retycfs_dataroom_list_refreshes_total`, `retycfs_spool_bytes`, `retycfs_uploads_total{result}`, `retycfs_locks_held`, and with Redis `retycfs_store_op_duration_seconds{op, result}` | mount pod   | cache, spool and uploads                                                                               |
| `retycfs_background_deletes_total{result}`                                                                                                                                                                           | mount pod   | with `csi.retycfs.com/background-delete`                                                               |
| `retycfs_api_*`, `retycfs_token_*`, `retycfs_build_info`, `go_*`, `process_*`                                                                                                                                        | mount pod   | Retyc API calls and latency, token expiry, runtime                                                     |

The node plugin's metrics are updated by every reconciliation round (every 30 s). The mount pods' series carry the
pod's labels through the PodMonitor (`csi.retycfs.com/volume`, `/node`, `/pvc`, `/namespace` are on the pod; add a
`podTargetLabels` or relabeling to keep them).

Useful starting points:

```promql
sum by (op) (rate(retycfs_ops_total{result!="ok"}[5m])) > 0           # filesystem errors seen by the pods
retycfs_csi_mount_pods{state="exited"} > 0                              # a mount pod waiting for its restart, or given up
retycfs_csi_mount_pod_outdated > 0                                      # mount pods on an older retyc-cli
retycfs_spool_bytes                                                   # data written, not sent yet
histogram_quantile(0.95, sum by (le, op) (rate(retycfs_op_duration_seconds_bucket[5m])))
retycfs_token_expiry_seconds < 300                                    # access token about to expire
```

Alert on `retycfs_ops_total{result!="ok"}`: it is what a pod sees as an I/O error.

`/metrics` listens on the pod IPs without authentication: no file names, no dataroom content. On a cluster that
restricts pod traffic, open ports `9808` and `9567` to Prometheus, and `9567` to the node plugin (it probes the mount
pods' `/readyz`), e.g. with a NetworkPolicy in `extraObjects`.

## Image

The `ghcr.io/retyc/retyc-cli` release embedded in the controller image is pinned by `ARG RETYC_CLI_VERSION` in the
`Dockerfile`, and the same version must be `mount.image.tag` in the chart (`make helm-lint` checks it). For a one-off
build against another release, or the development CLI (`make image-dev`, see [testing.md](testing.md)):

```sh
make image RETYC_CLI_VERSION=v1.5.0-beta1
make image RETYC_CLI_IMAGE=ghcr.io/retyc/retyc-cli-dev RETYC_CLI_VERSION=master
```

The image is based on `debian:trixie-slim`, for the `mount` binary the bind mounts use, with CA certificates.

## Installing without Helm

`helm template` renders the same resources as plain YAML, for GitOps pipelines or a `kubectl apply`:

```sh
helm template retycfs-csi retyc/retycfs-csi -n retycfs-system --set identity.organizationID=<uuid>
```

With the Redis subchart, set `cache.redis.subchart.password` (see [Cache](#cache)). The chart is also available as an
OCI artifact: `helm install retycfs-csi oci://ghcr.io/retyc/charts/retycfs-csi`.
