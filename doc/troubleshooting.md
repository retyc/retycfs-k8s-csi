# Troubleshooting

The commands below use `retycfs-system`, the namespace of the README's install; replace it with yours. Start with the
driver pods, the identities, the mount pods and the claim's events:

```sh
kubectl get retycidentity -A                     # PHASE, FINGERPRINT, ACTIVE, MACHINE of every identity
kubectl -n retycfs-system get pods -l app.kubernetes.io/name=retycfs-csi -o wide
kubectl -n retycfs-system get pods -l app.kubernetes.io/name=retycfs-mount -o wide -L csi.retycfs.com/pvc,csi.retycfs.com/node
kubectl describe pvc <claim>                     # provisioning, expansion and mount events
kubectl describe pod <pod using the claim>       # FailedMount: the stage's own message
```

The controller pods must be `3/3` (csi-provisioner, csi-resizer, driver), the node pods `2/2`, the enroller `1/1`. Logs:

```sh
kubectl -n retycfs-system logs deploy/retycfs-csi-controller -c retycfs-csi-controller
kubectl -n retycfs-system logs deploy/retycfs-csi-enroller                    # enrollments, approvals, machine checks
kubectl -n retycfs-system logs deploy/retycfs-csi-controller -c csi-provisioner
kubectl -n retycfs-system logs ds/retycfs-csi-node -c retycfs-csi-node        # one node's; pick the pod of the node with -o wide
kubectl -n retycfs-system logs <mount pod>                                # the retyc fs mount of one volume on one node
```

A mount pod is `retycfs-mount-<hash>`, one per volume and per node; find the one of a claim on a node with
`-l csi.retycfs.com/pvc=<claim>,csi.retycfs.com/namespace=<namespace>` (label values that are not valid labels are
hashed; the annotations of the same names hold the exact values).

## Reading a mount pod's logs

The CLI logs to the mount pod's output, which **disappears with the pod**: once a mount pod is deleted (unstage,
recreation), `kubectl logs` has nothing left. While it exists:

- `kubectl -n retycfs-system logs <mount pod>`, also after its container exited: the finalizer `csi.retycfs.com/in-use`
  keeps the pod object until the node plugin has read its exit code;
- `kubectl -n retycfs-system get pod <mount pod> -o yaml`: `status.containerStatuses[0].state.terminated` holds the exit
  code and, thanks to `terminationMessagePolicy: FallbackToLogsOnError`, the end of the log of a failed CLI. The node
  plugin copies that message into its own errors and into the claim's events, so it outlives the pod there;
- the node plugin's log keeps its view of every pod (`mount pod ... exited with code ...`).

For a live view of what the filesystem does, read `.retycfs/accesslog` at the root of the mount, from any pod using the
claim (it is not listed by `ls`):

```sh
kubectl exec <pod> -- tail -f /data/.retycfs/accesslog
```

It is a live log of the operations, numbers only: no file names.

## Exit codes of a mount pod

| Code | Meaning |
|------|---------|
| `0` | clean stop: unmounted, everything was sent |
| `1` | any other error, a failure at start included: read the message (Redis unreachable or too old, mount point left dead, API refused at start). A retyc-cli before v1.5.0-beta1 also exits `1` on lost writes: the driver then reads the loss in the output |
| `75` | writes lost: files being written, or background deletes, not sent before the filesystem went away (drain timed out or cut short, API refused them). The mount worked: the pod is recreated with the backoff, never given up on |
| `77` | the machine account is disabled or deleted in Retyc. A login lost while writes were lost keeps `77`; the output then also reports the lost files. The mount pods try again by themselves, at most 15 minutes after the machine is enabled again |
| `78` | the machine account's configuration is refused (a configuration problem, not an approval one) |
| `137` | killed: `kill -9`, OOM kill (`reason: OOMKilled`, or `Error` when containerd misses it), or still draining at the end of its grace period |

## Events on the claim

| Reason | Meaning | What to do |
|--------|---------|------------|
| `MountGaveUp` | the first exit with `77` or `78` on a node: the machine account is refused. Stages of the volume on that node fail at once with `FailedPrecondition`, without a new pod | for a `77`, enable the machine again in Retyc (`kubectl get retycidentity` shows `ACTIVE` `False`, reason `MachineRefused`; if it was deleted, recreate the `RetycIdentity`): the volume is tried again within 15 minutes. A recreated `RetycIdentity` (another machine, another config hash) gives a fresh try; a `78` is final until the configuration changes. The exited mount pod is released within 30 s (the volume was never staged), but the give-up stays for 30 minutes after the last stage that found it. After fixing something the hash does not cover (the API side), restart the node plugin pod of that node (it cuts no mount) to start a full series of tries there; deleting the exited mount pod by hand before it is released, or an unstage of the volume, does the same |
| `WritesLost` | a mount pod exited with code `75` when the node plugin released it, or with `1` or `77` and an output that reports lost files (`... not sent before the filesystem was unmounted`: a CLI older than `75`, or a login lost at the same time): files a pod believed written that never reached Retyc (the drain timed out, the API refused) | check the message; find out why the drain did not finish (`mount.drainTimeout`, API errors, a pod killed by a drain or a delete) |
| `MountFailed` | a mount pod exited with code `1` and its output reports no lost file. Before it was ever ready (`failed before mounting it`), a failure at start, nothing was written (Redis unreachable or too old, a mount point left dead, the API refused at start); after (`failed after mounting it`), the mount stopped on an error but reported no lost write | read the output in the event; see [Mount pods exit `1` at start with Redis](#mount-pods-exit-1-at-start-with-redis) |
| `MountOOMKilled` | a mount pod was killed at its memory limit (`137`, `reason: OOMKilled`), or killed (`137`) while no deletion asked it to stop, which containerd does not always report as `OOMKilled` ("most likely at its memory limit"; a `kill -9` gives the same), and is recreated; files being written then may not have reached Retyc. A walk of a large dataroom (an indexer, a backup, `ls -lR`) holds memory for every entry until the kernel forgets it, which a mount pod's own limit does not make it do | raise `csi.retycfs.com/mount-memory-limit` on the claim, or `mount.resources`; see [architecture.md](architecture.md#mount-pods) for the sizing |
| `MountConfigChanged` | the volume's settings changed (annotations, identity, `mount.*` or `cache.*` values, image) while the volume is in use on a node: its mount pod keeps the former ones | remount the volume on that node: move or restart the pods using it there, all of them, so that it is unstaged and staged again |

`ProvisioningFailed`, `VolumeResizeFailed` and `VolumeModifyFailed` come from the sidecars and carry the controller's
CSI error (see [architecture.md](architecture.md#errors-of-the-controller)).

## Claim refused at creation: `denied request: ValidatingAdmissionPolicy ... csi.retycfs.com/uid is required`

The chart's admission policy refuses a claim of its StorageClasses without `csi.retycfs.com/uid`, an update that removes
it, and any `csi.retycfs.com/*` annotation with a value the driver would refuse; the message names the annotation and the
expected format. Add the annotation (for a StatefulSet, in the `volumeClaimTemplates`). See
[configuration.md](configuration.md#pvc-annotations).

## Claim stays `Pending`

`kubectl describe pvc` shows the provisioner's event:

- `InvalidArgument ... annotation csi.retycfs.com/uid is required`: the claim has no owner annotation and the policy did
  not see it (disabled, or the class came later as the cluster default). Nothing was created on the Retyc side; add the
  annotation;
- `InvalidArgument ... annotation csi.retycfs.com/initial-members`: the list is malformed (`email[:admin|editor|viewer]`,
  comma-separated, no duplicate);
- `OutOfRange ... below the smallest dataroom reservation`: the claim sets a limit below 20 GiB;
- `ResourceExhausted`: the account is read-only, its dataroom count is reached, or its plan has no storage left;
- `Unavailable ... awaiting approval`: the identity is enrolled but not approved yet, see [the next
  section](#identity-stays-pending-denied-expired-blocked); the event gives the fingerprint to compare;
- `FailedPrecondition ... no credentials`: a class without secrets while the driver has no cluster-wide identity;
- `FailedPrecondition ... identity enrolled for another namespace`: the tenant Secret was enrolled for a former
  namespace of the same name;
- `FailedPrecondition ... the machine ... disabled`: Retyc refuses the machine of the class (or the driver's own):
  enable it again in Retyc. `... refuses the retyc CLI of the driver image as too old`: upgrade the driver;
- `PermissionDenied`: the API answered 403 (the identity is only a viewer, or the account is over its plan);
- `Unavailable` with an `API error 429`: the API's rate limit held after the CLI's own retries; the provisioner retries with its backoff;
- `secret "retycfs-identity-<namespace>" not found`: a tenant class in a namespace that has no `RetycIdentity` (or the
  enroller has not written the Secret yet). The driver's own identity is never used for that class: apply the
  `RetycIdentity` `default` in the claim's namespace.

After a successful creation, `Warning` events on the claim:

- `InitialMembers`: some people of `csi.retycfs.com/initial-members` could not be added; the driver does not retry,
  add them in Retyc;
- `NoHumanAdmin`: no `admin` among them. See [configuration.md](configuration.md#dataroom-lifecycle-and-ownership-transfer).

## Identity stays `Pending`, `Denied`, `Expired`, `Blocked`

Everything starts from the `RetycIdentity` of the namespace (the driver's namespace for the cluster-wide one):

```sh
kubectl -n <namespace> get retycidentity default -o wide
kubectl -n <namespace> describe retycidentity default       # conditions and events
kubectl -n retycfs-system logs deploy/retycfs-csi-enroller
```

| Phase | Cause | What to do |
|-------|-------|------------|
| `Pending` | the request waits for an administrator of the Retyc organization | compare the `FINGERPRINT` with the request in Retyc (Organization > Machines, or `retyc machine requests`), then approve it, in the web app or with `retyc machine approve <enrollment-id> --fingerprint XXXX-...`. A request lives 15 minutes; the enroller sends a new one |
| `Expired` | four requests in a row went unanswered | approve, then ask again: `kubectl -n <namespace> annotate retycidentity default csi.retycfs.com/retry=$(date +%s) --overwrite` |
| `Denied` | an administrator refused the request. Terminal | delete and recreate the `RetycIdentity` |
| `Rejected` | no organization (`spec.organizationID` empty and no `identity.organizationID` on the chart), or Retyc refused the configuration | set it, then the `retry` annotation. `organizationID` cannot be changed once set: delete and recreate |
| `Blocked` | the Secret of an older identity of that namespace, still used by PVs, is in the way (the namespace was deleted and recreated) | delete the PVs and claims that use the former identity, or keep them and use another namespace |

The pods of the driver stay ready meanwhile: an identity awaiting approval shows on its `RetycIdentity` and on the
claims' Events, not on the pods.

`Approved` with `ACTIVE` `False` (reason `MachineRefused`): Retyc refuses the machine, disabled or deleted there. The
machine itself cannot tell the two apart; an administrator can, with `retyc machine ls` (status `inactive` or
`deleted`) or in Organization > Machines. Disabled: enable it again, mounts retry within 15 minutes. Deleted: it cannot come
back; delete the `RetycIdentity` (`kubectl -n <ns> delete retycidentity default`, then recreate it for a tenant; the
enroller recreates the cluster-wide one by itself) and approve the new request. Retyc refuses to delete a machine that
still owns datarooms, so none is lost; transfer them first otherwise (see
[Dataroom lifecycle](configuration.md#dataroom-lifecycle-and-ownership-transfer)). The `RetycIdentity` is refused with `only one RetycIdentity per namespace, named 'default'` for any other name.

## Pod stuck in `ContainerCreating`

`kubectl describe pod` shows `MountVolume.MountDevice failed` with the stage's message. kubelet retries by itself; the
message says what it waits for.

| Message | Cause |
|---------|-------|
| `mount pod retycfs-mount-... is starting: ErrImagePull` / `ImagePullBackOff` | the retyc-cli image cannot be pulled: `mount.image`, `mount.imagePullSecrets` |
| `volume ... is not mounted yet` | the CLI is logging in and unlocking the key (a few seconds), or waits for the API; read the mount pod's log |
| `mount pod ... exited (code N, ...), K failures in a row: restarting at ...` | the mount fails; the code and output say why (table above); the backoff goes up to 5 min |
| `mount pod gave up; last output: ...` (`FailedPrecondition`) | machine refused: see `MountGaveUp` |
| `mount pod of volume ...: pods "retycfs-mount-..." is forbidden: violates PodSecurity ...` | the driver's namespace does not allow privileged pods: label it `pod-security.kubernetes.io/enforce=privileged` |
| `annotation csi.retycfs.com/... : want ...` (`InvalidArgument`) | an annotation the driver refuses (the policy was bypassed), such as `local-file` with `none` among other patterns |
| `identity enrolled for namespace "...", but the volume names no claim namespace` (`FailedPrecondition`) | a static PV of a tenant class without `pvcName` and `pvcNamespace` in its `volumeAttributes`: add them (see [configuration.md](configuration.md#static-pvs)) |
| `claim .../... not found` | the PV's `pvcName`/`pvcNamespace` point to a claim that no longer exists |
| `no credentials: the StorageClass has no node-stage secret ...` | no cluster-wide identity (`identity.organizationID` unset), or an incomplete one |
| `... machine enrollment awaiting approval` (`Unavailable`) | the identity is not approved yet: see [Identity stays Pending](#identity-stays-pending-denied-expired-blocked) |
| `PV ... has no nodeStageSecretRef, but its StorageClass ... is a tenant class` (`FailedPrecondition`) | a static PV of a tenant class without `spec.csi.nodeStageSecretRef`: the driver's identity is not used in its place. Add the reference (see [configuration.md](configuration.md#static-pvs)) |
| `the mount pod is ready but ... is not mounted in the node plugin` | the node plugin does not see the mount pod's mount: its `<kubeletDir>/plugins` mount must be `Bidirectional`, and `node.kubeletDir` must be the nodes' real kubelet directory |
| `is still mounted ... at a staging path of a former kubelet layout` | a mount pod of an older layout still serves pods on that node: drain them, or delete that mount pod |

## `Permission denied` on a write

- The pod does not run as the claim's `csi.retycfs.com/uid`: every file belongs to that UID (and to the GID, by
  default the same), and another UID only reads. Set `securityContext.runAsUser` (and `runAsGroup`), or change the
  annotation and remount. `fsGroup` changes nothing.
- `rename` over an existing file fails with `EACCES` while other writes work: the identity is a **contributor** on the
  dataroom, which cannot replace a file. Tools that write a temporary file and rename it over the original fail for
  that role.

`Read-only file system` (`EROFS`): the identity only **views** the dataroom, its plan refuses writes, the claim has
`csi.retycfs.com/read-only: "true"`, or the pod mounts it read-only. The role is read at mount time: after a role change,
remount the volume on each node.

## `Transport endpoint is not connected` inside a pod

The volume's mount pod on that node died (OOM kill, `kill -9`, eviction, `kubectl delete`) and the pod still holds the
dead mount. The node plugin recreates the mount pod and stacks the new mount on the pods' target paths (about ten
seconds after a `kill -9`, more within the restart backoff); its log shows `recovery: volume ... now ...` and
`recovery: rebinding ...`.

- The error lasts a few seconds: nothing to do.
- It persists in a pod while the node plugin logged `rebinding` for it: the pod does not mount the volume with
  `mountPropagation: HostToContainer`. Restart its container, and add the setting or, where a policy forbids it, a
  liveness probe that writes to the volume (see [architecture.md](architecture.md#recovery)).
- The node plugin logs `is mounted at ... but has no mount pod on this node`: the mount pod was deleted with its
  finalizer removed by hand. Reschedule the pods using the volume on that node.

For an OOM kill (`137`, `reason: OOMKilled` in the mount pod's status), raise `csi.retycfs.com/mount-memory-limit` on the
claim, or `mount.resources`.

## Mount pod stays `Terminating`

It is draining: after `SIGTERM` the CLI sends what is still being written, for up to `mount.drainTimeout` (5 min by
default), and `NodeUnstageVolume` returns `Aborted` (`mount pod ... is draining`) until it is done; its log says how
many files are left. A pod past its grace period on a node that is gone or hung is taken as finished a minute later.

Do not remove the finalizer by hand while the node is alive: the exit code would be lost, and a volume still in use
would be left without a mount pod to repair it.

## Dead mount pods are not cleaned up, node plugin logs `cannot read kubelet's directory of the driver`

The node plugin cannot list `<kubeletDir>/plugins/kubernetes.io/csi/csi.retycfs.com`: `node.kubeletDir` is not kubelet's
`--root-dir` (some distributions move it), or its plugins directory is not mounted in the node plugin. Kubelet's
`vol_data.json` files are then out of sight, so the node plugin deletes no mount pod as an orphan until it can read the
directory again. Set `node.kubeletDir` to the nodes' real kubelet directory.

## Settings or a new CLI version not applied

Annotations, chart `mount.*` and `cache.*` values, the retyc-cli image and a new identity are read when a node
mounts the volume. A running mount pod keeps its own: the claim gets `MountConfigChanged`, and
`retycfs_csi_mount_pod_outdated` counts the pods on an older CLI. Remount the volume on the node (every pod using it there
gone, then back).

## `df` shows the old size after an expand

The dataroom was enlarged (the PV and `retyc dataroom ls` show it), but a running mount caches the dataroom's stats for
the cache TTL (1 min by default, `csi.retycfs.com/cache-ttl`). Wait that long. If `df` still shows the old size after
several TTLs, it is a bug: report it with the claim, its annotations and the mount pod's logs.

## Writes from one node take long to appear on another

Expected up to the cache TTL (1 min by default, `csi.retycfs.com/cache-ttl`): a node sees another node's upload when its
own listing cache expires. Changes made outside the cluster too. See
[architecture.md](architecture.md#consistency).

## Mount pods exit `1` at start with Redis

`cache.store: redis` and the Redis does not answer, is too old (7.4 or later is required) or is a Cluster or Sentinel
deployment: the mount fails. Check the mount pod's output and the Redis pod. Once mounted, a failing Redis does not
fail the mount.

After a GitOps sync, if every new mount pod gets another config (`MountConfigChanged` everywhere) and the Redis password
changed: the subchart's password was generated at render time, and the Redis StatefulSet kept the former one. Set
`cache.redis.subchart.password`, or use `cache.redis.subchart.existingSecret` (see
[configuration.md](configuration.md#cache)).

## `kubectl drain` stops the volumes of the node

The mount pods are evicted like any pod (the drain needs `--force`, since they have no controller, and
`--delete-emptydir-data`). The finalizer does not prevent it: each mount pod drains its pending writes within its grace
period and stops; the node plugin may recreate it while pods using the volume are still on the node. Drain the
consumers first, or accept the interruption.

## PV stays `Released` with `Delete` policy, provisioner logs `cannot patch resource "persistentvolumes"`

The provisioner's ClusterRole lacks `patch` on `persistentvolumes`, which it needs to remove its finalizer. Upgrade to
the current chart (`rbac.create: true`), or add the verb to your own ClusterRole.

## Controller pod is `2/3`, node pods `1/2`

The driver container does not answer `/readyz`: it is starting, or it exits (`kubectl logs` of the `retycfs-csi-*`
container, then `--previous`). Its readiness does not depend on the identities: an identity not approved yet, or a
machine disabled in Retyc, shows on its `RetycIdentity` (`kubectl get retycidentity -A`, phase and `Active` condition),
see [above](#identity-stays-pending-denied-expired-blocked).
