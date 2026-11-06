# Testing

## Unit tests

```sh
make lint test helm-lint
```

## Test cluster

k3s in Incus virtual machines, created by OpenTofu, against a real Retyc API: use a test organization. Requires Incus
(initialized with `sudo incus admin init`, its default answers), your user in the `incus-admin` group, OpenTofu
(`tofu`), and an administrator of that organization to approve the machines (the web app, or the `retyc` CLI).

`vm/main.tf` describes the VMs (resources, network) and runs their provisioning, by cloud-init
(`vm/cloud-init-server.yaml`, `vm/cloud-init-agent.yaml`). They run Debian 13 from `images:debian/13/cloud`. Both sit on
their own Incus network, `retycfs`: the server at `192.168.57.10`, the agent at `192.168.57.11`, the host at
`192.168.57.1`.

If Docker runs on the same host, its firewall drops the traffic Incus routes for its VMs: they get no Internet access.
Allow the `retycfs` network through Docker's `DOCKER-USER` chain (see Incus' documentation, "Prevent connectivity issues
with Incus and Docker").

### 1. Configure: `vm/.env` and `vm/.env.local`

Two files in `vm/` say what to deploy. OpenTofu and the Makefile read them.

- **`.env`** is versioned: the team's defaults. Don't put anything personal in it.
- **`.env.local`** is yours: not versioned, never copied into the VMs. Each line overrides the same line of `.env`. It
  also holds your organization.

Create `vm/.env.local` (`chmod 600 vm/.env.local`):

```sh
CLUSTER_CHART=dev                                    # the dev API and this repo's chart (Retyc team only)
RETYC_ORGANIZATION_ID=<UUID of your test organization>
# CLUSTER_MODE=single                                # uncomment to override .env
```

The settings:

| Variable                | Values                                                                                                                                                                               | Default (`.env`)        |
|-------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|
| `CLUSTER_CHART`         | `prod`: the published chart, against the production API. `dev`: this repo's chart and driver, against the development API (Retyc team only: set it in `.env.local`)                  | `prod`                  |
| `CLUSTER_MODE`          | `multi`: two nodes and a shared Redis cache. `single`: one node, memory cache                                                                                                        | `multi`                 |
| `RETYC_ORGANIZATION_ID` | organization of the cluster-wide identity (a dev one with `dev`, a production one with `prod`); `make vm-helm` passes it as `identity.organizationID`. Empty: tenant identities only | none: `.env.local` only |

Format: `KEY=value`, no quotes, no `export`. Variables exported in your shell are ignored. For one command only:
`make vm-helm CLUSTER_CHART=dev`.

### 2. Start

The VMs, from `vm/`:

```sh
cd vm
tofu init                    # once: fetches the Incus provider
tofu apply                   # one or two VMs, depending on CLUSTER_MODE, provisioned (a few minutes)
```

Then, from the repo root:

```sh
make image-dev vm-load       # dev only: build the driver and load it into the VMs
make vm-helm                 # the chart, in the namespace retycfs-system
make shell-server            # then: kubectl -n retycfs-system get pods (make shell-agent: the agent)
```

From the host, on the server's address:

| Service    | URL                                                                         |
|------------|-----------------------------------------------------------------------------|
| Privatebin | http://192.168.57.10 (once `examples/manifests/privatebin.yaml` is applied) |
| Jaeger     | http://192.168.57.10:8081                                                   |
| Prometheus | http://192.168.57.10:8082                                                   |

`make vm-helm` creates `retycfs-system` with the `privileged` Pod Security label and installs the chart with
`identity.organizationID`. The enroller then requests the cluster's machine account; `vm-helm` returns once the pods are
ready, without waiting for the approval. Until it is approved, the claims of `retycfs-rwx*` stay `Pending`. Approve it
from the host:

```sh
incus exec retycfs-csi -- kubectl -n retycfs-system get retycidentity default -o wide   # FINGERPRINT, enrollment
retyc machine requests
retyc machine approve <enrollment-id> --fingerprint XXXX-... -y   # the dev org has no master key: -y confirms
```

The dev values set `image.pullPolicy: IfNotPresent`: the image imported by `make vm-load` is never pulled from a
registry.

The mount pods pull the development CLI (`ghcr.io/retyc/retyc-cli-dev:master`), which `make image-dev` also embeds. To
test an unpublished CLI instead, build it in the CLI repository, build the driver on it without pulling, and import the
CLI image into every node:

```sh
make image-dev CLI_DEV_TAG=<tag> CLI_PULL=0
docker save ghcr.io/retyc/retyc-cli-dev:<tag> | incus exec retycfs-csi -- k3s ctr images import -
docker save ghcr.io/retyc/retyc-cli-dev:<tag> | incus exec retycfs-csi-agent -- k3s ctr images import -   # multi mode
```

`mount.image.tag` of `examples/helm-values/dev-vm-values.yaml` must name the same tag.

After a change: `make image-dev vm-load vm-restart` for the driver, `make vm-helm` for the chart, which first copies
this repo into the server, at `/src` (`make vm-sync`, without `vm/.env.local`). `tofu destroy`, in `vm/`, deletes the
VMs; after a change of `CLUSTER_MODE`, `tofu apply` adds or removes the agent. `make vm-stop` shuts the VMs down,
keeping them; `tofu apply`, in `vm/`, starts them again.

### 3. If something is wrong

- Claims of `retycfs-rwx*` `Pending` with `awaiting approval`: the cluster's `RetycIdentity` is not approved yet (see
  above). `kubectl get retycidentity -A` tells the phase; `Expired` takes the `csi.retycfs.com/retry` annotation
  ([troubleshooting.md](troubleshooting.md#identity-stays-pending-denied-expired-blocked)).
- A machine disabled in Retyc fails the mounts (exit `77`); enable it again.
- A provisioning that failed: `tofu apply` shows the end of its log; all of it with
  `incus exec retycfs-csi -- cat /var/log/cloud-init-output.log`. `tofu apply -replace=incus_instance.server` starts
  again (`incus_instance.agent[0]` for the agent).

## Scenarios to check

In the server (`make shell-server`, `cd /src`):

- `kubectl apply -f examples/manifests/rwx-test.yaml`: the claim is `Bound`; the writer writes, the reader (another UID)
  gets `Permission denied`.
- A claim without the `csi.retycfs.com/uid` annotation is refused.
- Tenant: `kubectl apply -f examples/manifests/tenant.yaml`; the claim stays `Pending` with the fingerprint in its
  events,
  until the namespace's `RetycIdentity` is approved.
- `csi.retycfs.com/initial-members` on a new claim: the people are members of the dataroom, in Retyc.
- Killing a mount process (`sudo pkill -9 -f '^/retyc fs mount'` on its node): the volume comes back within ~20 s,
  without restarting the pods.
- Restarting the node plugin pods cuts no mount.
- Deleting the pods removes their mount pods.
- Two nodes: `kubectl apply -f examples/manifests/privatebin.yaml` runs PrivateBin on each node; a paste created through
  one is readable at once through the other (Redis). With the memory cache, it can take up to a minute.

`kubectl delete -f examples/manifests/rwx-test.yaml` removes the claim and its dataroom.
