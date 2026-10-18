# retycfs-csi

Helm chart for the [RetycFS CSI driver](https://github.com/retyc/retycfs-k8s-csi): `ReadWriteMany` Kubernetes volumes
backed by Retyc datarooms, with end-to-end post-quantum encryption.

## Install

The driver namespace runs privileged mount pods, so it needs the Pod Security label `privileged`. Use a namespace of
its own: the controller, the enroller and the node plugin read the Secrets of their namespace, which hold the machine accounts' keys
(`kube-system` is discouraged).

```sh
kubectl create namespace retycfs-system
kubectl label namespace retycfs-system pod-security.kubernetes.io/enforce=privileged
helm repo add retyc https://retyc.github.io/retycfs-k8s-csi
helm install retycfs-csi retyc/retycfs-csi \
  --namespace retycfs-system \
  --set identity.organizationID=<Retyc organization UUID>
```

From the OCI registry (`--version` selects a chart version; omit it for the latest):

```sh
helm install retycfs-csi oci://ghcr.io/retyc/charts/retycfs-csi --version <chart version> -n retycfs-system ...
```

There is no token or passphrase to give. With `identity.organizationID`, the enroller creates the `RetycIdentity`
`default` of the release namespace and requests a Retyc machine account for it. An administrator of the organization
approves the request in Retyc (Organization > Machines, or `retyc machine approve`) after comparing the fingerprint:

```sh
kubectl -n retycfs-system get retycidentity default -o wide
```

The pods are ready without waiting for it (`helm install --wait` returns): until then, the `RetycIdentity` is `Pending`
and the claims of the cluster-wide classes wait, with an Event that gives the fingerprint. Without `identity.organizationID`, the driver runs with no
cluster-wide identity: the StorageClasses without `tenant: true` are not installed, and only per-namespace identities
are served. A tenant namespace applies its own `RetycIdentity` (`name: default`, optional `spec.organizationID`) and has
it approved; see [Identities](../../doc/configuration.md#identities).

The chart installs the `CSIDriver`, the `RetycIdentity` CRD (kept on uninstall, see [Uninstall](../../doc/configuration.md#uninstall)), RBAC, the controller `Deployment`, the
enroller `Deployment`, the node `DaemonSet`, the mount pods' configuration, two `ValidatingAdmissionPolicy` (the claims'
`csi.retycfs.com/*` annotations, and the identities' Secrets) and up to three StorageClasses (`retycfs-rwx`,
`retycfs-rwx-retain`, `retycfs-rwx-tenant`). Each staged volume runs in a privileged mount pod
(`retyc fs mount`) in the release namespace. Restarting the node plugin
cuts no mount. Every claim needs the `csi.retycfs.com/uid` annotation: the files belong to that UID, and only pods running
as it can write. See [PVC annotations](../../doc/configuration.md#pvc-annotations) for the others.

Uninstall: delete the PVCs/PVs, then `kubectl delete retycidentity --all -A` and wait for them to disappear, and only then
`helm uninstall`. Each `RetycIdentity` has the finalizer `csi.retycfs.com/identity`, which only the enroller removes: without
it they stay `Terminating`. Full procedure, including the Retyc side and the manual finalizer removal:
[Uninstall](../../doc/configuration.md#uninstall).

**StatefulSets**: put `csi.retycfs.com/uid` in the `volumeClaimTemplates` (`metadata.annotations`). Without it, the
admission policy refuses each claim the StatefulSet controller creates, and the controller retries in a loop: the
StatefulSet's pods stay `Pending` and its events repeat the refusal.

## Values

| Key | Default | Description |
|-----|---------|-------------|
| `image.repository` | `ghcr.io/retyc/retycfs-k8s-csi` | Driver image (Docker Hub mirror: `retyc/retycfs-k8s-csi`) |
| `image.tag` | `""` | Overrides the image tag; defaults to the chart's `appVersion` |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | Pull secrets for the driver image, both components (only for a private mirror) |
| `identity.organizationID` | `""` | UUID of the Retyc organization of the cluster-wide identity. Set, the enroller creates the `RetycIdentity` `default` of the release namespace (Secret `retycfs-identity`) and the classes without `tenant` are installed. Also the organization of tenant `RetycIdentity` without `spec.organizationID` |
| `identity.clusterName` | `kubernetes` | Prefix of the machines' default display name in Retyc, `<clusterName>/<namespace>` |
| `storageClasses` | three classes | `name`, `reclaimPolicy`, `volumeBindingMode`, `isDefault`, `annotations`, `tenant`. All allow expansion; `tenant: true` uses the Secret `retycfs-identity-<claim namespace>` of the release namespace; a class without it is only installed with `identity.organizationID` |
| `rbac.create` | `true` | Create the ClusterRoles/Roles and bindings, and the roles aggregated to `edit`/`admin` (write) and `view` (read) on `RetycIdentity` |
| `serviceAccount.controller.*` / `serviceAccount.node.*` / `serviceAccount.enroller.*` | create | `create`, `name`, `annotations`. The identity Secrets policy lets only the enroller's service account write the identities' Secrets |
| `extraObjects` | `[]` | Extra manifests rendered as templates (see values.yaml for NetworkPolicy examples) |
| `healthPort` | `9808` | `/healthz`, `/readyz` and `/metrics` port of the driver containers |
| `metrics.podMonitor.enabled` | `false` | Create two Prometheus Operator `PodMonitor`s: both driver components, and the mount pods (port `metrics`, 9567) |
| `metrics.podMonitor.namespace` / `labels` | `""` / `{}` | Where to create them, and labels your Prometheus selects on |
| `metrics.podMonitor.interval` / `scrapeTimeout` / `relabelings` / `metricRelabelings` | Prometheus defaults | Scrape settings, shared by both |
| `logLevel` | `2` | klog verbosity |
| `probes.liveness` / `probes.readiness` | see values | Probe timings, shared by both components |
| `controller.replicas` | `1` | Above 1, `--leader-election` is added to csi-provisioner and csi-resizer (with a Role on leases) |
| `controller.resources` | requests `cpu: 10m`, `memory: 64Mi`; limits `memory: 512Mi` | Driver container resources (it runs the `retyc` CLI for each provisioning call) |
| `controller.provisioner.image.*` / `controller.resizer.image.*` | `csi-provisioner:v6.3.0` / `csi-resizer:v2.3.0` | Sidecar images |
| `controller.provisioner.extraArgs` / `controller.resizer.extraArgs` | `[]` | Extra sidecar arguments |
| `controller.extraEnv` | `[]` | Extra environment for the driver container (e.g. `HTTPS_PROXY`, `NO_PROXY`) |
| `controller.extraVolumes` / `controller.extraVolumeMounts` | `[]` | Extra volumes for the driver container (e.g. a private CA bundle) |
| `controller.hostAliases` | `[]` | Extra `/etc/hosts` entries |
| `controller.{podAnnotations,podLabels,nodeSelector,tolerations,affinity,topologySpreadConstraints,priorityClassName}` | | Scheduling (anti-affinity example in values.yaml) |
| `enroller.resources` | requests `cpu: 10m`, `memory: 32Mi`; limits `memory: 256Mi` | Enroller container resources (one `retyc machine enroll` process per pending enrollment) |
| `enroller.extraArgs` / `extraEnv` / `extraVolumes` / `extraVolumeMounts` / `hostAliases` | `[]` | Same as the controller's (egress proxy, private CA bundle...) |
| `enroller.{podAnnotations,podLabels,nodeSelector,tolerations,affinity,priorityClassName}` | | Scheduling; one replica |
| `node.kubeletDir` | `/var/lib/kubelet` | Kubelet root on the nodes |
| `node.resources` | requests `cpu: 10m`, `memory: 32Mi`; limits `memory: 128Mi` | The node plugin runs no retyc process |
| `node.registrar.image.*` | `csi-node-driver-registrar:v2.17.0` | Sidecar image |
| `node.extraArgs` / `node.extraEnv` | `[]` | Extra arguments and environment for the driver container |
| `node.extraVolumes` / `node.extraVolumeMounts` | `[]` | Extra volumes for the driver container |
| `node.updateStrategy` | `RollingUpdate`, `maxUnavailable: 1` | |
| `node.{podAnnotations,podLabels,nodeSelector,tolerations,affinity,priorityClassName}` | tolerate all | Scheduling |
| `mount.image.*` | `ghcr.io/retyc/retyc-cli:v1.5.0` | Mount pods' image; must match the Dockerfile's `RETYC_CLI_VERSION` (checked in CI) |
| `mount.imagePullSecrets` | `[]` | Pull secrets of the retyc-cli image |
| `mount.extraEnv` / `mount.hostAliases` | `[]` | Extra environment (after the driver's) and `/etc/hosts` entries of the mount pods |
| `mount.hostPID` | `false` | Share the host PID namespace (diagnosis only) |
| `mount.resources` | requests `cpu: 50m`, `memory: 96Mi`; limit `memory: 384Mi` | Default resources, overridden per claim by the `csi.retycfs.com/mount-*` annotations |
| `mount.drainTimeout` | `5m` | Time a stopping mount pod gets to send its pending writes |
| `mount.priorityClassName` | `""` | Empty means `system-node-critical` |
| `mount.spoolDir` | `/var/lib/retycfs-csi/spool` | Host directory of the spools, one per volume |
| `cache.store` | `memory` | Listing cache of the mount pods: `memory` (one per pod) or `redis` (shared) |
| `cache.highConfidentiality` | `false` | Keep only ciphertexts in Redis; set it when several tenants share the Redis |
| `cache.redis.subchart.enabled` | `false` | Deploy the CloudPirates Redis chart (standalone, no persistence, `allkeys-lru`; values under `redis`). Only with `cache.store: redis` |
| `cache.redis.subchart.password` | `""` | Redis password; empty generates one and keeps it across upgrades (not in GitOps: see below) |
| `cache.redis.subchart.existingSecret` | `""` | Your own Secret instead, keys `redis-password` and `RETYC_CACHE_REDIS_URL`; set `redis.auth.existingSecret` to the same name |
| `cache.redis.external.url` | `""` | URL of your own Redis (7.4+, standalone), stored in a Secret by the chart |
| `cache.redis.external.existingSecret` / `existingSecretKey` | `""` / `RETYC_CACHE_REDIS_URL` | Existing Secret holding the URL instead |
| `admissionPolicy.enabled` | `true` | `ValidatingAdmissionPolicy` on the annotations of the claims of the chart's StorageClasses, and on the identities' Secrets (type `csi.retycfs.com/machine-identity`) of the release namespace |
| `admissionPolicy.failurePolicy` | `Fail` | `Fail` or `Ignore` when the claims policy cannot be evaluated; only the claims of the chart's classes are matched. The Secrets policy always fails closed |
| `redis.*` | see values | Values of the Redis subchart (`maxmemory`, resources...) |

The Redis URL never goes into the mount pods' ConfigMap: the node plugin reads it from a Secret (`RETYC_CACHE_REDIS_URL`)
and copies it into each mount pod's own Secret. The admission policy only knows the classes this release installs: a
claim given one of them later as the cluster default, or a static PV's claim on another class, is not checked (the
driver still refuses an invalid claim at provisioning or at mount).

**GitOps (Argo CD, `helm template`)**: set `cache.redis.subchart.password`, or use `cache.redis.subchart.existingSecret`.
The generated password relies on Helm's `lookup`, which returns nothing outside `helm install`/`upgrade`: each render
would generate a new one, while the Redis StatefulSet keeps the former one.

## Releasing a chart version

The chart is published by the `Chart` workflow when `Chart.yaml` changes on `master`: bump `version` (the chart)
and, when it should ship a new driver, `appVersion` to an image tag that exists (a `v*` release of this repository).
chart-releaser then creates the `retycfs-csi-<version>` GitHub release with the package, refreshes `index.yaml` on
the `gh-pages` branch, and the same package is pushed to `oci://ghcr.io/retyc/charts`.

## Per-tenant identities

A StorageClass entry with `tenant: true` resolves the Secret `retycfs-identity-<claim namespace>` of the release
namespace for provisioning, expansion and mounting, through the standard `csi.storage.k8s.io/*-secret-*` parameters. That
Secret is written by the enroller from the `RetycIdentity` `default` of the claim's namespace; the tenant cannot read it.
Use `reclaimPolicy: Retain` on such classes. Details: [Identities](../../doc/configuration.md#identities).
