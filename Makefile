MODULE       := github.com/retyc/retycfs-k8s-csi
BINARY       := retycfs-k8s-csi
IMAGE        ?= ghcr.io/retyc/retycfs-k8s-csi:dev
RETYC_CLI_IMAGE ?= ghcr.io/retyc/retyc-cli
# Development build of retyc-cli (no `prod` tag: honours RETYC_INSECURE), published from its master.
CLI_DEV_IMAGE ?= ghcr.io/retyc/retyc-cli-dev
CLI_DEV_TAG  ?= master
# 0 builds against the local copy of the CLI image (one built or imported here, not published).
CLI_PULL     ?= 1
VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REVISION     := $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
CREATED      := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
EPOCH        := $(shell date -u +%s)
LDFLAGS      := -s -w -X $(MODULE)/internal/driver.DriverVersion=$(VERSION)

CHART        := charts/retycfs-csi

.PHONY: build test vet lint vulncheck clean image image-dev helm-lint helm-template helm-package \
	vm-stop vm-sync vm-load vm-helm vm-restart shell-server shell-agent

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
## fetches that CLI tag again: a moving one (master) would otherwise stay at its local copy;
## CLI_PULL=0 keeps the local one (e.g. make image-dev CLI_DEV_TAG=<local tag> CLI_PULL=0).
image:
	docker build \
	  $(if $(RETYC_CLI_VERSION),--no-cache $(if $(filter 1,$(CLI_PULL)),--pull) --build-arg RETYC_CLI_VERSION=$(RETYC_CLI_VERSION) --build-arg SOURCE_DATE_EPOCH=0) \
	  --build-arg RETYC_CLI_IMAGE=$(RETYC_CLI_IMAGE) \
	  --build-arg VERSION=$(VERSION) --build-arg REVISION=$(REVISION) --build-arg CREATED=$(CREATED) \
	  -t $(IMAGE) .

## The image against the development CLI (CLI_DEV_IMAGE:CLI_DEV_TAG), for the test VMs and the dev
## API: the controller embeds its binary, as the mount pods of examples/helm-values/dev-vm-values.yaml
## run it.
image-dev:
	$(MAKE) image RETYC_CLI_IMAGE=$(CLI_DEV_IMAGE) RETYC_CLI_VERSION=$(CLI_DEV_TAG)

## Lint the Helm chart (values schema included), check that it runs the Dockerfile's retyc-cli, and
## render it with and without a cluster-wide identity.
helm-lint:
	helm dependency build $(CHART)
	helm lint $(CHART)
	.github/scripts/check-cli-version.sh $(CHART)
	helm template x $(CHART) --set identity.organizationID=0190a7f4-0000-7000-8000-000000000001 >/dev/null
	helm template x $(CHART) >/dev/null

## Render the chart with default values to stdout.
helm-template:
	helm template retycfs-csi $(CHART) -n retycfs-system

## Package the chart into dist/.
helm-package:
	helm package $(CHART) -d dist

## ---------------------------------------------------------------------------------------------
## The test cluster: k3s in Incus VMs, the server retycfs-csi and, in multi mode, the agent
## retycfs-csi-agent. OpenTofu creates and deletes them (vm/main.tf, doc/testing.md); the targets
## below only use them. Nothing here talks to the cluster from the host: every target goes through
## `incus exec` in the server, as root.
##
## The cluster's settings: vm/.env, then vm/.env.local over it (not versioned); `make vm-helm
## CLUSTER_CHART=prod` overrides both for one run, the shell environment never does. Read line by
## line rather than included: make would expand a `$` of the credentials.
##   CLUSTER_CHART  dev (this repo's chart, the image of make image-dev) or prod (the published chart)
##   CLUSTER_MODE   multi (server + agent, Redis cache) or single (server alone, memory cache)
##   RETYC_ORGANIZATION_ID  organization of the cluster-wide identity (empty: tenants only)
VM_ENV_FILES := vm/.env $(wildcard vm/.env.local)
vm_env = $(shell sed -n 's/^$(1)=//p' $(VM_ENV_FILES) | tail -n 1)
CLUSTER_CHART := $(call vm_env,CLUSTER_CHART)
CLUSTER_MODE := $(call vm_env,CLUSTER_MODE)
RETYC_ORGANIZATION_ID := $(call vm_env,RETYC_ORGANIZATION_ID)
VM_NAMESPACE := retycfs-system

VM_SERVER := retycfs-csi
VM_AGENT := retycfs-csi-agent
VM_NODES := $(VM_SERVER) $(if $(filter multi,$(CLUSTER_MODE)),$(VM_AGENT))
# A shell in the server, where kubectl and helm reach k3s.
VM_SH = incus exec $(VM_SERVER) --env KUBECONFIG=/etc/rancher/k3s/k3s.yaml -- bash -c
# Where vm-sync copies this repo in the server.
VM_SRC := /src
VM_CHART_REPO ?= https://retyc.github.io/retycfs-k8s-csi
VM_VALUES ?= examples/helm-values/$(CLUSTER_CHART)-vm-values.yaml $(if $(filter multi,$(CLUSTER_MODE)),examples/helm-values/redis-values.yaml)
VM_CHECK = @case "$(CLUSTER_CHART)/$(CLUSTER_MODE)" in dev/multi|dev/single|prod/multi|prod/single) ;; \
	*) echo "CLUSTER_CHART must be dev or prod and CLUSTER_MODE multi or single (vm/.env, vm/.env.local), not '$(CLUSTER_CHART)' and '$(CLUSTER_MODE)'"; exit 1 ;; esac

## Shut the VMs down, keeping them: OpenTofu applies running=false (vm/main.tf) once its plan is
## confirmed. `cd vm && tofu apply` starts them again.
vm-stop:
	cd vm && tofu apply -var running=false

## A root shell in the server (kubectl and helm ready) or in the agent.
shell-server:
	incus shell $(VM_SERVER)

shell-agent:
	incus shell $(VM_AGENT)

## Copy this repo into the server, at $(VM_SRC), replacing the previous copy: neither .git, nor the
## builds, nor OpenTofu's files, nor the personal settings (vm/.env.local, .env.dev).
vm-sync:
	tar -c --exclude-vcs --exclude=./.idea --exclude=./retycfs-k8s-csi --exclude=./dist \
		--exclude=./vm/.env.local --exclude=./vm/.terraform '--exclude=./vm/terraform.tfstate*' --exclude=./.env.dev . \
		| incus exec $(VM_SERVER) -- sh -c 'rm -rf $(VM_SRC) && mkdir $(VM_SRC) && tar -x -C $(VM_SRC)'

## Import the locally built image straight into each node's containerd (no registry involved).
vm-load:
	$(VM_CHECK)
	@for m in $(VM_NODES); do echo "$(IMAGE) -> $$m"; docker save $(IMAGE) | incus exec $$m -- k3s ctr images import - || exit 1; done

## Install or upgrade the chart as CLUSTER_CHART and CLUSTER_MODE say, with VM_VALUES:
## examples/helm-values/<dev|prod>-vm-values.yaml, plus examples/helm-values/redis-values.yaml in
## multi mode. dev installs this repo's chart (its Redis subchart fetched first if missing), prod
## the latest published one. Without --reuse-values: what a scenario --set is dropped.
## Typical loop: make image-dev vm-load vm-helm vm-restart
vm-helm:
	$(VM_CHECK)
ifneq ($(CLUSTER_CHART),prod)
	@ls $(CHART)/charts/redis-*.tgz >/dev/null 2>&1 || helm dependency build $(CHART)
endif
	$(MAKE) --no-print-directory vm-sync
	$(VM_SH) "kubectl get namespace $(VM_NAMESPACE) >/dev/null 2>&1 || kubectl create namespace $(VM_NAMESPACE); kubectl label namespace $(VM_NAMESPACE) pod-security.kubernetes.io/enforce=privileged --overwrite"
ifeq ($(CLUSTER_CHART),prod)
	$(VM_SH) "helm repo add retycfs $(VM_CHART_REPO) --force-update >/dev/null && helm upgrade --install retycfs-csi retycfs/retycfs-csi -n $(VM_NAMESPACE) $(VM_VALUES:%=-f $(VM_SRC)/%) --set identity.organizationID=$(RETYC_ORGANIZATION_ID) --wait --timeout 5m"
else
	$(VM_SH) "helm upgrade --install retycfs-csi $(VM_SRC)/$(CHART) -n $(VM_NAMESPACE) $(VM_VALUES:%=-f $(VM_SRC)/%) --set identity.organizationID=$(RETYC_ORGANIZATION_ID) --wait --timeout 5m"
endif

## Restart controller, node plugin and enroller so they pick up a freshly loaded :dev image
## (IfNotPresent + unchanged tag = no rollout on its own), and wait for them. Typical loop: make image-dev vm-load vm-restart
vm-restart:
	$(VM_SH) "kubectl -n $(VM_NAMESPACE) rollout restart deploy/retycfs-csi-controller deploy/retycfs-csi-enroller ds/retycfs-csi-node && kubectl -n $(VM_NAMESPACE) rollout status deploy/retycfs-csi-controller --timeout=180s && kubectl -n $(VM_NAMESPACE) rollout status deploy/retycfs-csi-enroller --timeout=180s && kubectl -n $(VM_NAMESPACE) rollout status ds/retycfs-csi-node --timeout=180s"
