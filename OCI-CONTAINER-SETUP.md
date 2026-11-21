# OCI Containers

## Kubernetes Setup

The following command will start the Workspace Daemon in Kubernetes mode, assuming your kubeconfig file is properly configured:

```sh
# Will create all directories in home directory and the default namespace.  You must create the namespace first to use an alternate namespace.
/usr/local/bin/mcwsd daemon --plugin plugins/kubernetes.js --bind-address=0.0.0.0 --port 9080 --workspace-auth-mode=scoped
```

## Running the daemon in a container (Kubernetes plugin)

The Kubernetes plugin is a `kubectl` client: it creates one Pod per workspace in
a cluster that already exists. The daemon does not need a container runtime of
its own, so it can run unprivileged in an OCI container. The repository
`Dockerfile` builds exactly that image.

Build it with whichever runtime the workstation has:

```sh
# Docker
docker build -t mcwsd-kubernetes .

# Podman
podman build -t mcwsd-kubernetes .

# Apple container
container build -t mcwsd-kubernetes .
```

The daemon refuses to run as root, so the image creates a dedicated `mcwsd`
user (default uid/gid `1000:1000`). Apple `container`'s virtiofs mount makes a
host kubeconfig readable regardless of its owner, but Docker/Podman bind mounts
on Linux preserve ownership. If your runtime enforces it and the kubeconfig is
mode `0600`, build with the same uid/gid so the container user can read it:

```sh
container build \
  --build-arg MCWSD_UID="$(id -u)" \
  --build-arg MCWSD_GID="$(id -g)" \
  -t mcwsd-kubernetes .
```

Generate an admin token and start the daemon. `MCWSD_TOKEN` is required because
the container listens on `0.0.0.0`:

```sh
export MCWSD_TOKEN="$(container run --rm mcwsd-kubernetes token)"

container run -d --name mcwsd \
  -p 9080:9080 \
  -e MCWSD_TOKEN="$MCWSD_TOKEN" \
  -e KUBECONFIG=/etc/mcwsd/kubeconfig \
  -v "$HOME/.kube/config:/etc/mcwsd/kubeconfig:ro" \
  mcwsd-kubernetes \
  --kubernetes-namespace=mcwsd
```

`--kubernetes-namespace` is optional (default `default`). The plugin's `init`
applies a Namespace manifest at startup, so the kubeconfig identity needs
permission to create or patch that namespace, plus get/list/create/delete pods,
create pods/exec, and create/delete networkpolicies in it.

The image entrypoint defaults to:

```text
mcwsd daemon --plugin /usr/local/lib/mcwsd/plugins/kubernetes.js \
  --bind-address=0.0.0.0 --port=9080 --workspace-auth-mode=scoped
```

Flags passed to the container are appended, so `--port`,
`--workspace-auth-mode`, and the plugin's `--kubernetes-*` flags can be
overridden. Set `MCWSD_PLUGIN` to select a different plugin path. The entrypoint
supplies the plugin for `daemon`, `cleanup`, and `status`; `token` and
`version` run verbatim.

The daemon writes state, jobs, and its cache under `~/wsd`, and clones into
`~/wsd-workspaces`. Mount named volumes at `/home/mcwsd/wsd` and
`/home/mcwsd/wsd-workspaces` if those should survive a container restart:

```sh
container volume create mcwsd-data
container volume create mcwsd-workspaces

container run -d --name mcwsd \
  -p 9080:9080 \
  -e MCWSD_TOKEN="$MCWSD_TOKEN" \
  -e KUBECONFIG=/etc/mcwsd/kubeconfig \
  -v "$HOME/.kube/config:/etc/mcwsd/kubeconfig:ro" \
  -v mcwsd-data:/home/mcwsd/wsd \
  -v mcwsd-workspaces:/home/mcwsd/wsd-workspaces \
  mcwsd-kubernetes
```

Workspace images are pulled by the Kubernetes nodes, not by the daemon
container, so the cluster must be able to reach whatever registry your detected
base images come from.

### Why not Docker-in-Docker for the docker/podman plugins?

Docker-in-Docker would not make this image work better. The Kubernetes plugin
never talks to a local container runtime; it only calls `kubectl`. Running a
nested `dockerd` inside the daemon container would add a privileged daemon and
a second storage/cgroup stack for no benefit to the Kubernetes path.

For the `docker` plugin, the usual pattern is Docker-outside-of-Docker: install
the Docker CLI in the daemon image and mount the host's
`/var/run/docker.sock`. The daemon then creates sibling containers on the host
daemon. Two things are required:

- the container user must be able to use the socket (group mapping), and
- `--workspace-base-path` and `--cache-dir` must be paths that exist with the
  same names on the Docker host, because the host daemon interprets the bind
  mounts the plugin passes.

Podman is different: the plugin runs the `podman` binary, and rootless Podman
inside a container needs user namespaces, `/dev/fuse`, `fuse-overlayfs`, and
subuid/subgid mappings. That is not practical on Apple `container`, which does
not offer a privileged Docker-in-Docker mode. The practical alternatives are to
run mcwsd on the Linux host that owns the Podman storage, or to use
`podman-remote` against a Podman socket and give the plugin a `podman` command
that speaks to that socket. The same host-path parity rule applies.

## Podman Setup

This command will start the Workspace Daemon in Podman mode creating directories as needed in home directory:

```sh
/usr/local/bin/mcwsd daemon --plugin plugins/podman.js --port 9080 --bind-address=0.0.0.0 --debug --workspace-auth-mode=scoped
```

## Docker Setup

This command will start the Workspace Daemon in Docker mode:

```sh
/usr/local/bin/mcwsd daemon --plugin plugins/docker.js --port 9080 --bind-address=0.0.0.0 --debug --workspace-auth-mode=scoped
```

Docker mode assume the user account is in the docker group.  None of the modes require any special permissions like doas/sudo.

## Apple Container Setup

Apple's `container` command is an OCI container runtime for macOS. Make sure the
container services are running and the `container` binary is on `PATH`:

```sh
container system start
/usr/local/bin/mcwsd daemon --plugin plugins/apple-container.js --port 9080 --bind-address=0.0.0.0 --debug --workspace-auth-mode=scoped
```

Apple container fixes a container's network attachment when the container is
created; it has no `network connect` or `network disconnect`. The daemon's
per-exec network toggles therefore cannot add or remove the interface after
provisioning. Choose the attachment at daemon startup with
`--container-network` (default `default`). Set it to `none` to create
network-less workspaces, or to an Apple container network name created with
`container network create`.

```sh
mcwsd daemon --plugin plugins/apple-container.js --container-network none --port 9080 --workspace-auth-mode=scoped
```

## Notes

The `--bind-address` and `--port` flags are optional and can be omitted if you want to use the default values. The default bind address is `127.0.0.1` and the default port is `8080` The `--debug` flag is optional and can be omitted if you do not want to enable debug logging. The `--debug` flag will increase the verbosity of the logs and can be useful for troubleshooting.

All examples above will create a random token on startup so remember to

```sh
export MCWSD_TOKEN=wsd-<token_data>
```

before startup so you have a fixed token.
