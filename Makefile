MODULE       := github.com/retyc/retycfs-k8s-csi
BINARY       := retycfs-k8s-csi
IMAGE        ?= ghcr.io/retyc/retycfs-k8s-csi:dev
RETYC_CLI_IMAGE ?= ghcr.io/retyc/retyc-cli
# Development build of retyc-cli (no `prod` tag: honours RETYC_INSECURE), published from its master.
CLI_DEV_IMAGE ?= ghcr.io/retyc/retyc-cli-dev
CLI_DEV_TAG  ?= master
VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REVISION     := $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
CREATED      := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
EPOCH        := $(shell date -u +%s)
LDFLAGS      := -s -w -X $(MODULE)/internal/driver.DriverVersion=$(VERSION)

CHART        := charts/retycfs-csi

.PHONY: build test vet lint vulncheck clean image image-dev helm-lint helm-template helm-package \
	vm-load vm-secret vm-helm vm-restart

## Build the driver binary (both controller and node modes)
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

## Run tests with race detector
test:
	go test -race ./...

## Run go vet
vet:
	go vet ./...

## Run golangci-lint
lint:
	golangci-lint run ./...

vulncheck:
	govulncheck ./...

clean:
	rm -f $(BINARY)

## Build the container image (controller + node share one image, see main.go --mode), with OCI
## provenance labels from git. The embedded retyc-cli version is pinned in the Dockerfile;
## `make image RETYC_CLI_VERSION=commit-xxxxxxx` overrides it for a one-off build, `RETYC_CLI_IMAGE`
## the CLI image repository, `VERSION=v1.0.0` the image version. With RETYC_CLI_VERSION, `--pull`
## fetches that CLI tag again: a moving one (master) would otherwise stay at its local copy.
image:
	docker build \
	  $(if $(RETYC_CLI_VERSION),--no-cache --pull --build-arg RETYC_CLI_VERSION=$(RETYC_CLI_VERSION) --build-arg SOURCE_DATE_EPOCH=0) \
	  --build-arg RETYC_CLI_IMAGE=$(RETYC_CLI_IMAGE) \
	  --build-arg VERSION=$(VERSION) --build-arg REVISION=$(REVISION) --build-arg CREATED=$(CREATED) \
	  -t $(IMAGE) .

## The image against the development CLI (CLI_DEV_IMAGE:CLI_DEV_TAG), for the test VMs and the dev
## API: the controller embeds its binary, as the mount pods of examples/helm-values/dev-vagrant-values.yaml
## run it.
image-dev:
	$(MAKE) image RETYC_CLI_IMAGE=$(CLI_DEV_IMAGE) RETYC_CLI_VERSION=$(CLI_DEV_TAG)

## Lint the Helm chart (values schema included), check that it runs the Dockerfile's retyc-cli, and
## render it in its three credential modes.
helm-lint:
	helm dependency build $(CHART)
	helm lint $(CHART)
	.github/scripts/check-cli-version.sh $(CHART)
	helm template x $(CHART) --set credentials.token=t --set credentials.keyPassphrase=p >/dev/null
	helm template x $(CHART) --set credentials.existingSecret=s >/dev/null
	helm template x $(CHART) >/dev/null

## Render the chart with default values to stdout.
helm-template:
	helm template retycfs-csi $(CHART) -n kube-system

## Package the chart into dist/.
helm-package:
	helm package $(CHART) -d dist

## ---------------------------------------------------------------------------------------------
## What runs in the test cluster. The VMs themselves are Vagrant's (Vagrantfile, doc/testing.md:
## vagrant up / halt / destroy / ssh server). Nothing in this Makefile talks to the cluster from
## the host: every vm-* target goes through `vagrant ssh server -c`, a login shell in the server
## where kubectl and helm work without sudo.
##
## The cluster's settings, shared with the Vagrantfile: .env, then .env.local over it (not
## versioned); `make vm-helm CLUSTER_CHART=prod` overrides both for one run, the shell environment
## never does. Read line by line rather than included: make would expand a `$` of the credentials.
##   CLUSTER_CHART  dev (this repo's chart, the image of make image-dev) or prod (the published chart)
##   CLUSTER_MODE   multi (server + agent, Redis cache) or single (server alone, memory cache)
VM_ENV_FILES := .env $(wildcard .env.local)
vm_env = $(shell sed -n 's/^$(1)=//p' $(VM_ENV_FILES) | tail -n 1)
CLUSTER_CHART := $(call vm_env,CLUSTER_CHART)
CLUSTER_MODE := $(call vm_env,CLUSTER_MODE)

VM_NODES := server $(if $(filter multi,$(CLUSTER_MODE)),agent)
VM_CHART_REPO ?= https://retyc.github.io/retycfs-k8s-csi
VM_VALUES ?= examples/helm-values/$(CLUSTER_CHART)-vagrant-values.yaml $(if $(filter multi,$(CLUSTER_MODE)),examples/helm-values/redis-values.yaml)
VM_CHECK = @case "$(CLUSTER_CHART)/$(CLUSTER_MODE)" in dev/multi|dev/single|prod/multi|prod/single) ;; \
	*) echo "CLUSTER_CHART must be dev or prod and CLUSTER_MODE multi or single (.env, .env.local), not '$(CLUSTER_CHART)' and '$(CLUSTER_MODE)'"; exit 1 ;; esac

## Import the locally built image straight into each node's containerd (no registry involved).
vm-load:
	$(VM_CHECK)
	@for m in $(VM_NODES); do echo "$(IMAGE) -> $$m"; docker save $(IMAGE) | vagrant ssh $$m -c "sudo k3s ctr images import -" || exit 1; done

## Create/refresh the credentials Secret from RETYC_TOKEN and RETYC_KEY_PASSPHRASE of .env.local (a
## dev account with CLUSTER_CHART=dev, a production one with prod), not from the shell environment.
## Values travel in a shell variable and over stdin, never on a command line.
vm-secret:
	@creds=$$(awk '/^RETYC_(TOKEN|KEY_PASSPHRASE)=./ { v[substr($$0, 1, index($$0, "=") - 1)] = $$0 } \
	  END { if (!("RETYC_TOKEN" in v) || !("RETYC_KEY_PASSPHRASE" in v)) exit 1; print v["RETYC_TOKEN"]; print v["RETYC_KEY_PASSPHRASE"] }' $(VM_ENV_FILES)) || \
	  { echo "set RETYC_TOKEN and RETYC_KEY_PASSPHRASE in .env.local (see doc/testing.md)"; exit 1; }; \
	printf '%s\n' "$$creds" | \
	  vagrant ssh server -c "kubectl -n kube-system create secret generic retycfs-csi-credentials --from-env-file=/dev/stdin --dry-run=client -o yaml | kubectl apply -f -"

## Install or upgrade the chart as CLUSTER_CHART and CLUSTER_MODE say, with VM_VALUES:
## examples/helm-values/<dev|prod>-vagrant-values.yaml, plus examples/helm-values/redis-values.yaml in
## multi mode. dev installs this repo's chart (its Redis subchart fetched first if missing), prod
## the latest published one. Without --reuse-values: what a scenario --set is dropped.
## Typical loop: make image-dev vm-load vm-helm vm-restart
vm-helm:
	$(VM_CHECK)
ifneq ($(CLUSTER_CHART),prod)
	@ls $(CHART)/charts/redis-*.tgz >/dev/null 2>&1 || helm dependency build $(CHART)
endif
	vagrant rsync server
ifeq ($(CLUSTER_CHART),prod)
	vagrant ssh server -c "helm repo add retycfs $(VM_CHART_REPO) --force-update >/dev/null && helm upgrade --install retycfs-csi retycfs/retycfs-csi -n kube-system $(VM_VALUES:%=-f /vagrant/%) --wait --timeout 5m"
else
	vagrant ssh server -c "helm upgrade --install retycfs-csi /vagrant/$(CHART) -n kube-system $(VM_VALUES:%=-f /vagrant/%) --wait --timeout 5m"
endif

## Restart controller + node plugin so they pick up a freshly loaded :dev image (IfNotPresent +
## unchanged tag = no rollout on its own), and wait for both. Typical loop: make image-dev vm-load vm-restart
vm-restart:
	vagrant ssh server -c "kubectl -n kube-system rollout restart deploy/retycfs-csi-controller ds/retycfs-csi-node && kubectl -n kube-system rollout status deploy/retycfs-csi-controller --timeout=180s && kubectl -n kube-system rollout status ds/retycfs-csi-node --timeout=180s"
