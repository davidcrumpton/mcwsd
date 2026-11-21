# syntax=docker/dockerfile:1.7
#
# mcwsd — Kubernetes-runtime daemon image.
#
# The Kubernetes plugin is a kubectl client: it creates one Pod per workspace
# in a cluster that already exists. The daemon therefore needs no container
# runtime of its own, no Docker socket, and no --privileged. That is why this
# image works on an ordinary container host (including Apple `container`),
# while the docker/podman plugins still need a runtime outside the daemon.
#
# Build:
#   docker build -t mcwsd-kubernetes .
#   podman build -t mcwsd-kubernetes .
#   container build -t mcwsd-kubernetes .
#
# Run (see OCI-CONTAINER-SETUP.md for the full walkthrough):
#   docker run -d --name mcwsd -p 9080:9080 \
#     -e MCWSD_TOKEN="$(docker run --rm mcwsd-kubernetes token)" \
#     -v "$HOME/.kube/config:/etc/mcwsd/kubeconfig:ro" \
#     -e KUBECONFIG=/etc/mcwsd/kubeconfig \
#     mcwsd-kubernetes

ARG GO_VERSION=1.25
ARG DEBIAN_VERSION=bookworm

# ---------------------------------------------------------------------------
# Build the daemon
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-${DEBIAN_VERSION} AS build

ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /src

# Keep the module download in its own layer so source edits do not refetch it.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/mcwsd ./cmd/mcwsd

# ---------------------------------------------------------------------------
# Fetch kubectl for the target architecture, verifying the published checksum
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM debian:${DEBIAN_VERSION}-slim AS kubectl

ARG TARGETARCH
ARG KUBECTL_VERSION=v1.34.1
ARG KUBECTL_BASE=https://dl.k8s.io/release
ARG DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/* \
 && curl -fsSLo /usr/local/bin/kubectl \
      "${KUBECTL_BASE}/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
 && curl -fsSLo /tmp/kubectl.sha256 \
      "${KUBECTL_BASE}/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl.sha256" \
 && echo "$(cat /tmp/kubectl.sha256)  /usr/local/bin/kubectl" | sha256sum -c - \
 && chmod 0755 /usr/local/bin/kubectl

# ---------------------------------------------------------------------------
# Runtime image
# ---------------------------------------------------------------------------
FROM debian:${DEBIAN_VERSION}-slim AS runtime

# mcwsd refuses to run as root (os.Getuid() == 0). The image therefore runs as
# a dedicated unprivileged user. Set these to the host user's uid/gid when your
# container runtime enforces host ownership on a bind-mounted kubeconfig that
# is only readable by its owner:
#   container build --build-arg MCWSD_UID=$(id -u) --build-arg MCWSD_GID=$(id -g) .
ARG MCWSD_UID=1000
ARG MCWSD_GID=1000
ARG DEBIAN_FRONTEND=noninteractive

# git + openssh-client: host-side clone/push, including SSH repo URLs.
# ca-certificates: HTTPS clone and the Kubernetes API.
# tar is part of the Debian base and is used by core's CopyDirIn.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates \
      git \
      openssh-client \
 && rm -rf /var/lib/apt/lists/*

# The GID may already exist in Debian (for example macOS's staff group is GID
# 20, which is dialout here), so only create the group when it is free.
RUN if ! getent group "${MCWSD_GID}" >/dev/null; then \
      groupadd --gid "${MCWSD_GID}" mcwsd; \
    fi \
 && useradd --uid "${MCWSD_UID}" --gid "${MCWSD_GID}" \
      --create-home --home-dir /home/mcwsd --shell /usr/sbin/nologin mcwsd \
 && mkdir -p \
      /home/mcwsd/.kube \
      /home/mcwsd/wsd \
      /home/mcwsd/wsd/cache \
      /home/mcwsd/wsd-workspaces \
 && chown -R "${MCWSD_UID}:${MCWSD_GID}" /home/mcwsd

COPY --from=build /out/mcwsd /usr/local/bin/mcwsd
COPY --from=kubectl /usr/local/bin/kubectl /usr/local/bin/kubectl
COPY plugins/kubernetes.js /usr/local/lib/mcwsd/plugins/kubernetes.js
COPY docker/entrypoint.sh /usr/local/bin/mcwsd-entrypoint
RUN chmod 0755 /usr/local/bin/mcwsd-entrypoint

USER mcwsd
WORKDIR /home/mcwsd
ENV HOME=/home/mcwsd

# Override to run a different plugin (the daemon still supports only one
# plugin per instance; use a separate container for a separate runtime).
ENV MCWSD_PLUGIN=/usr/local/lib/mcwsd/plugins/kubernetes.js

EXPOSE 9080

ENTRYPOINT ["/usr/local/bin/mcwsd-entrypoint"]
