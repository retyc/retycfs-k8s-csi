# The retyc-cli release embedded in the image — the single source of truth for that version.
# Bump it here (Dependabot proposes it); `make image RETYC_CLI_VERSION=...` overrides for a
# one-off build. It must match the mount pods' image in the chart's values.yaml.
ARG RETYC_CLI_VERSION=v1.6.0-beta1
# Registry/repository of the retyc-cli image; `make image RETYC_CLI_IMAGE=...` overrides it (e.g.
# the development CLI, ghcr.io/retyc/retyc-cli-dev: make image-dev).
ARG RETYC_CLI_IMAGE=ghcr.io/retyc/retyc-cli
# Build provenance, set by `make image` (git describe / rev-parse / date -u); also reported by the
# driver's GetPluginInfo through ldflags.
ARG VERSION=dev
ARG REVISION=unknown
ARG CREATED=1970-01-01T00:00:00Z

# Runs on the build host's architecture and cross-compiles (CGO is off, so Go needs nothing else):
# a multi-platform build never runs the compiler under QEMU, and `go mod download` is shared.
FROM --platform=$BUILDPLATFORM golang:1.26.10-trixie AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Declared as late as possible: an ARG is part of the cache key of every RUN that follows it.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w -X github.com/retyc/retycfs-k8s-csi/internal/driver.DriverVersion=${VERSION}" \
    -o /retycfs-k8s-csi .

# Reuse the officially published retyc-cli image instead of vendoring/rebuilding the binary —
# dataroom management is security-sensitive (AGE crypto) and should always run
# the same vetted build the CLI team ships. Pulled from GitHub's registry (same digest as the
# Docker Hub mirror, no anonymous pull limits, provenance attached to the source repository).
FROM ${RETYC_CLI_IMAGE}:${RETYC_CLI_VERSION} AS retyc

FROM debian:trixie-slim

# ca-certificates for the controller's HTTPS calls; the `mount` binary (util-linux, in the base
# image) is what k8s.io/mount-utils calls for the bind mounts.
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY --from=retyc /retyc /usr/local/bin/retyc
COPY --from=builder /retycfs-k8s-csi /usr/local/bin/retycfs-k8s-csi

# Last on purpose: REVISION and CREATED change on every build, and an ARG is part of the cache key
# of every RUN that follows it — declared at the top of the stage, they re-ran the apt-get above
# (under QEMU for the foreign platform) on each build.
ARG RETYC_CLI_VERSION
ARG VERSION
ARG REVISION
ARG CREATED

LABEL org.opencontainers.image.title="RetycFS CSI Driver" \
      org.opencontainers.image.description="Kubernetes CSI driver for Retyc datarooms: ReadWriteMany volumes with end-to-end post-quantum encryption" \
      org.opencontainers.image.vendor="Retyc / TripleStack SAS" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.url="https://retyc.com" \
      org.opencontainers.image.source="https://github.com/retyc/retycfs-k8s-csi" \
      org.opencontainers.image.documentation="https://github.com/retyc/retycfs-k8s-csi/blob/master/README.md" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${CREATED}" \
      org.opencontainers.image.base.name="docker.io/library/debian:trixie-slim" \
      com.retyc.cli.version="${RETYC_CLI_VERSION}"

ENTRYPOINT ["/usr/local/bin/retycfs-k8s-csi"]
