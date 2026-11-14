# OCI Containers

## Kubernetes Setup

The following command will start the Workspace Daemon in Kubernetes mode, assuming your kubeconfig file is properly configured:

```sh
# Will create all directories in home directory and the default namespace.  You must create the namespace first to use an alternate namespace.
/usr/local/bin/mcwsd daemon --plugin plugins/kubernetes.js --bind-address=0.0.0.0 --port 9080 --workspace-auth-mode=scoped
```

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
