# Testing

## Unit tests

```sh
make lint test helm-lint
```

## Test cluster

k3s in Vagrant VMs (libvirt), against a real Retyc API: use a test account. Requires `vagrant`, the `vagrant-libvirt`
plugin, and your user in the `libvirt` group.

### 1. Configure: `.env` and `.env.local`

Two files at the repo root say what to deploy. Both the `Vagrantfile` and the Makefile read them.

- **`.env`** is versioned: the team's defaults. Don't put anything personal in it.
- **`.env.local`** is yours: not versioned, never copied into the VMs. Each line overrides the same line of `.env`. It
  also holds your credentials.

Create `.env.local` (`chmod 600 .env.local`):

```sh
RETYC_TOKEN=<offline token of your test account>     # retyc auth login --offline
RETYC_KEY_PASSPHRASE=<its key passphrase>
# CLUSTER_MODE=single                                # uncomment to override .env
```

The settings:

| Variable | Values | Default (`.env`) |
|----------|--------|------------------|
| `CLUSTER_CHART` | `prod`: the published chart, against the production API. `dev`: this repo's chart and driver, against the development API (Retyc team only: set it in `.env.local`) | `prod` |
| `CLUSTER_MODE` | `multi`: two nodes and a shared Redis cache. `single`: one node, memory cache | `multi` |
| `RETYC_TOKEN`, `RETYC_KEY_PASSPHRASE` | the account the driver uses (a dev one with `dev`, a production one with `prod`) | none: `.env.local` only |

Format: `KEY=value`, no quotes, no `export`. Variables exported in your shell are ignored. For one command only:
`make vm-helm CLUSTER_CHART=dev`.

### 2. Start

```sh
vagrant up                   # one or two VMs, depending on CLUSTER_MODE
make image-dev vm-load       # dev only: build the driver and load it into the VMs
make vm-secret vm-helm       # credentials, then the chart
vagrant ssh server           # then: kubectl -n kube-system get pods
```

After a change: `make image-dev vm-load vm-restart` for the driver, `make vm-helm` for the chart. `vagrant destroy -f`
removes the VMs; recreate them after changing `CLUSTER_MODE`.

### 3. If something is wrong

- Controller stuck at `2/3` with `no_token`: the token expired. Update it in `.env.local`, then
  `make vm-secret vm-restart`.
- Privatebin is on http://localhost:18080, Jaeger on http://localhost:18081.

## Scenarios to check

In the server (`vagrant ssh server`, `cd /vagrant`):

- `kubectl apply -f examples/manifests/rwx-test.yaml`: the claim is `Bound`; the writer writes, the reader (another UID)
  gets `Permission denied`.
- A claim without the `csi.retycfs.com/uid` annotation is refused.
- Killing a mount process (`sudo pkill -9 -f '^/retyc fs mount'` on its node): the volume comes back within ~20 s,
  without restarting the pods.
- Restarting the node plugin pods cuts no mount.
- Deleting the pods removes their mount pods.
- Two nodes: `kubectl apply -f examples/manifests/privatebin.yaml` runs PrivateBin on each node; a paste created through
  one is readable at once through the other (Redis). With the memory cache, it can take up to a minute.

`kubectl delete -f examples/manifests/rwx-test.yaml` removes the claim and its dataroom.
