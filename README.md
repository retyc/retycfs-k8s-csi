<p align="center"><img width="200" src=".media/Retyc_Logo_Blue.png" alt="Retyc logo" /></p>

<p align="center">
  <a href="https://github.com/retyc/retycfs-k8s-csi/actions/workflows/main.yml"><img src="https://github.com/retyc/retycfs-k8s-csi/actions/workflows/main.yml/badge.svg" alt="CI" /></a>
  <a href="https://kubernetes-csi.github.io/docs/"><img src="https://img.shields.io/badge/CSI-1.11-326ce5.svg" alt="CSI 1.11" /></a>
  <a href="go.mod"><img src="https://img.shields.io/badge/go-1.26-00ADD8.svg" alt="Go 1.26" /></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="License: MIT" /></a>
</p>

# RetycFS CSI Driver

**RetycFS CSI driver** is a CSI driver that uses your [Retyc](https://retyc.com) account to support dynamic
provisioning of `ReadWriteMany` Kubernetes Persistent Volumes via Persistent Volume Claims. Each Persistent Volume is
a Retyc dataroom, mounted on the nodes with **RetycFS**, the filesystem of the
[`retyc` CLI](https://github.com/retyc/retyc-cli), and encrypted end to end on the node before anything reaches the
Retyc servers.

Each volume mounted on a node runs in its own mount pod, so restarting or upgrading the driver cuts no mount. Volumes
can be expanded, and their versioning switched through a VolumeAttributesClass.

## Requirements

- Kubernetes **1.34** or later;
- nodes that can run RetycFS (see the [mount pods](doc/architecture.md#mount-pods)) and reach `api.retyc.com` and
  `auth.retyc.com`;
- a namespace for the driver that allows privileged pods (Pod Security `privileged`): `retycfs-system`, created below;
- a Retyc organization, its UUID, and an administrator of it who approves the driver's machine account. The driver
  holds no token and no passphrase: it enrolls a [machine account](doc/configuration.md#identities).
- a retyc-cli release with machine accounts (`retyc machine enroll`), in the driver image and as `mount.image.tag`:
  until one is published, build it from its `feat/machine_enroll` branch (see [doc/testing.md](doc/testing.md)).

## How to deploy RetycFS CSI driver to your cluster

### With Helm

Follow the instructions from the helm chart [README](charts/retycfs-csi/README.md).

The tl;dr is

```console
$ kubectl create namespace retycfs-system
$ kubectl label namespace retycfs-system pod-security.kubernetes.io/enforce=privileged
$ helm repo add retyc https://retyc.github.io/retycfs-k8s-csi/
$ helm install retycfs-csi retyc/retycfs-csi --namespace retycfs-system \
    --set identity.organizationID=<Retyc organization UUID>
```

The enroller requests a machine account for the cluster; `helm install` does not wait for it. The pods are ready at
once, and the claims of the cluster-wide classes stay `Pending` until an administrator of the organization approves it,
in Retyc (Organization > Machines) after comparing the fingerprint, or with the CLI:

```console
$ kubectl -n retycfs-system get retycidentity default -o wide      # PHASE Pending, FINGERPRINT XXXX-...
$ retyc machine requests
$ retyc machine approve <enrollment-id> --fingerprint XXXX-...
```

Whether approved or not, the controller pod must be `3/3 Running`, the node pods `2/2` and the enroller `1/1`
(see [doc/troubleshooting.md](doc/troubleshooting.md)):

```console
$ kubectl -n retycfs-system get pods -l app.kubernetes.io/instance=retycfs-csi
```

Without `identity.organizationID`, no cluster-wide identity exists and only the per-namespace ones are served (below).

### Test your environment

Deploy the test resources, a claim with a writer pod and a reader pod:

```console
$ kubectl create -f https://raw.githubusercontent.com/retyc/retycfs-k8s-csi/master/examples/manifests/rwx-test.yaml
$ kubectl logs -f retycfs-reader
```

The reader prints the lines the writer appends. Check `retyc dataroom ls`, or the Retyc web app: the dataroom is
there, named after the volume. The volume's mount pod runs next to the driver:

```console
$ kubectl -n retycfs-system get pods -l app.kubernetes.io/name=retycfs-mount
```

Delete the test resources:

```console
$ kubectl delete -f https://raw.githubusercontent.com/retyc/retycfs-k8s-csi/master/examples/manifests/rwx-test.yaml
```

The dataroom is gone as well.

### Deploying your own PersistentVolumeClaims

Every claim needs the `csi.retycfs.com/uid` annotation: every file of the volume belongs to that UID, and **only pods
running as that UID can write** (others read). Use `accessModes: [ReadWriteMany]` and one of the storage classes:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: shared-docs
  annotations:
    csi.retycfs.com/uid: "1000"
spec:
  accessModes: [ReadWriteMany]
  storageClassName: retycfs-rwx
  resources:
    requests:
      storage: 20Gi
```

- `retycfs-rwx`: deleting the claim deletes the dataroom;
- `retycfs-rwx-retain`: deleting the claim keeps the dataroom;
- `retycfs-rwx-tenant`: same, with the machine account of the claim's namespace instead of the cluster-wide one. The
  namespace applies a `RetycIdentity` named `default` and has it approved first (see
  [doc/configuration.md](doc/configuration.md#identities)). `retycfs-rwx` and `retycfs-rwx-retain` exist only with
  `identity.organizationID`.

The size requested is reserved on your plan, 20 GiB at least. Run the pods with `securityContext.runAsUser` set to the
annotation's UID, and mount the volume with `mountPropagation: HostToContainer`, as in the examples: when a mount pod
dies and is recreated, the pod then sees its volume come back without restarting (see
[doc/architecture.md](doc/architecture.md#recovery)). The other annotations (cache, resources of the mount pod,
read-only...) are in [doc/configuration.md](doc/configuration.md#pvc-annotations).

A dataroom belongs to the machine account that created it. To add people to it at creation, use the
`csi.retycfs.com/initial-members` annotation, and read
[Dataroom lifecycle](doc/configuration.md#dataroom-lifecycle-and-ownership-transfer) before retiring an identity.

Every pod mounting the claim, on any node, shares the same dataroom. It behaves like a network drive: `close` returns
once the file is stored, a change made on one node shows up on the others within the cache TTL (one minute by
default), and concurrent writes to one file are not safe. Good for documents, configuration and artefacts; not for
databases.

## Documentation

- [charts/retycfs-csi/README.md](charts/retycfs-csi/README.md): chart values
- [doc/configuration.md](doc/configuration.md): PVC annotations, static PVs, versioning, identities, dataroom
  ownership transfer, Redis cache, metrics
- [doc/troubleshooting.md](doc/troubleshooting.md): mount pod logs, exit codes, events
- [doc/architecture.md](doc/architecture.md): how it works, mount pods, recovery, consistency, security, limitations
- [doc/testing.md](doc/testing.md): development environment and tests

## Development

```console
$ make build test lint     # driver
$ make helm-lint           # chart
$ make image               # container image
$ vagrant up               # k3s in Vagrant VMs (.env: dev/prod chart, multi/single node), then: make image-dev vm-load vm-helm
```

## License

[MIT](LICENSE) - © Retyc / TripleStack SAS
