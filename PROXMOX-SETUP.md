# Proxmox VE LXC Setup for mcwsd

This document describes how to configure a Proxmox VE host so that `mcwsd` can
provision workspaces as LXC containers (`--plugin plugins/proxmox-lxc.js`). It
covers the template, the API token, the SSH key, the network model, running the
daemon, and troubleshooting.

## How mcwsd uses the host

The daemon drives two separate planes:

1. **Lifecycle over the Proxmox REST API.** Create, start, stop, and delete of
   each workspace container happens through the Proxmox VE API (port `8006`)
   using `curl` and a stateless **API token** (`PVEAPIToken=…`). The token is
   read from an environment variable, never stored in the plugin.
2. **Transport over SSH.** The Proxmox REST API has no "run a command inside a
   container" endpoint, so every command the agent runs is executed by SSHing
   into the container's IP address — the same remote model as the
   `openbsd-vmd` plugin.

For each workspace, `mcwsd` allocates the next free VMID (`GET /cluster/nextid`),
creates the container from a template with the daemon's public key injected via
`ssh-public-keys`, waits for the create task, reads the container's IP from
`/nodes/{node}/lxc/{vmid}/interfaces`, then polls SSH until the container is
ready.

Container names are `wsd-<workspace-id>` (for example `wsd-ws-71670485b`).

## Requirements

- Proxmox VE 9.x with at least one node.
- A container **template** downloaded into a storage (see below).
- A Proxmox **API token** with `VM.Allocate`, `VM.PowerMgmt`, and `VM.Audit`
  privileges (see below).
- An SSH key pair on the daemon host.
- The daemon host can reach the Proxmox API (`<host>:8006`) and the container
  IPs (see Networking).

## 1. Create the API token

In the Proxmox web UI: **Datacenter → Permissions → API Tokens → Add**.

- User: `root@pam` (or a dedicated user).
- Check **Privilege Separation** and grant, at minimum, the following on `/`:

  ```text
  VM.Allocate
  VM.PowerMgmt
  VM.Audit
  VM.Config.Disk
  ```

The resulting secret is shown once and looks like
`PVEAPIToken=root@pam!mcwsd=aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee`. Store it in
an environment variable on the daemon host (default name `PVE_API_TOKEN`):

```sh
export PVE_API_TOKEN="root@pam!mcwsd=aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
```

The plugin never reads the token from a flag; it reads the environment variable
named by `--proxmox-token-env` (default `PVE_API_TOKEN`).

## 2. Download a container template

Proxmox's stock system templates ship `sshd`, so no image building is required.
Download one into the storage the daemon will use:

```sh
# on the Proxmox node
pveam update
pveam available --section system
pveam download local debian-12-standard_12.7-1_amd64.tar.zst
pveam list local
# local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst
```

Pass that full volume identifier as `--proxmox-ostemplate`.

> **OCI images.** Proxmox VE 9 can also create containers from OCI images
> (pulled via *Pull from OCI registry*). This works for the lifecycle, but a
> generic Docker application image has no `sshd`, so the SSH transport cannot
> reach it. Use a system-container OCI image (one with an init and `sshd`), or
> use the stock Debian/Ubuntu template above.

## 3. SSH key pair

Generate a key on the daemon host and note both paths:

```sh
ssh-keygen -t ed25519 -N '' -f /var/wsd/agent_ed25519
# /var/wsd/agent_ed25519      (private, used by the transport)
# /var/wsd/agent_ed25519.pub  (public, injected via ssh-public-keys)
```

The public key is injected into the container's **root** account at creation
time. If you use a non-root login user (`--proxmox-login-user`), you must bake
that user and key into a custom template instead, and set
`--proxmox-guest-escalation-command` to the guest's `sudo`/`doas`.

## 4. Networking

`net0` is attached to the bridge given by `--proxmox-bridge` (default `vmbr0`).
Address selection follows the FreeBSD jail plugin's precedence:

1. a per-workspace address (supplied by the caller);
2. `--proxmox-ip-address` (CIDR, e.g. `192.168.1.50/24`);
3. an address allocated from the shared pool (`--ip-base-network` /
   `--ip-pool-size`), with the gateway defaulting to the pool gateway;
4. otherwise **DHCP** (`ip=dhcp`).

The container's address must be **reachable from the daemon host** on port 22 —
the daemon SSHs directly into it. Use a bridged/routed network, not a NAT-only
veth.

Static example:

```sh
mcwsd daemon --plugin plugins/proxmox-lxc.js \
  --workspace-auth-mode=scoped \
  --proxmox-ip-address 192.168.1.50/24 \
  --proxmox-gateway 192.168.1.1
```

Pool example:

```sh
mcwsd daemon --plugin plugins/proxmox-lxc.js \
  --workspace-auth-mode=scoped \
  --ip-base-network 192.168.1.200/24 --ip-pool-size 50
```

DHCP example (omit both):

```sh
mcwsd daemon --plugin plugins/proxmox-lxc.js --workspace-auth-mode=scoped
```

> **Network on/off.** Like the VM runtime, `SetNetwork` is a no-op: LXC
> containers are always networked while they run, and changing `net0` on a
> running container only takes effect after a restart. Use the Proxmox
> firewall/SDN to restrict traffic if you need per-workspace isolation.

## 5. Running the daemon

```sh
export PVE_API_TOKEN="PVEAPIToken=root@pam!mcwsd=…"

mcwsd daemon \
  --workspace-auth-mode=scoped \
  --plugin plugins/proxmox-lxc.js \
  --proxmox-api-url https://pve.example:8006 \
  --proxmox-node pve1 \
  --proxmox-storage local-lvm \
  --proxmox-ostemplate local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst \
  --proxmox-bridge vmbr0 \
  --proxmox-ssh-key /var/wsd/agent_ed25519 \
  --cache-dir /var/wsd/cache \
  --state-file /var/wsd/proxmox.state.json
```

| Flag | Default | Description |
|------|---------|-------------|
| `--plugin` | *(required)* | Path to the plugin, `plugins/proxmox-lxc.js` |
| `--proxmox-api-url` | *(required)* | Proxmox VE API base URL, e.g. `https://pve.example:8006` |
| `--proxmox-node` | *(required)* | Node name containers are created on |
| `--proxmox-token-env` | `PVE_API_TOKEN` | Env var holding the API token |
| `--proxmox-insecure-tls` | `true` | Accept the self-signed PVE cert (`curl -k`) |
| `--proxmox-storage` | `local` | Storage for the rootfs volume |
| `--proxmox-ostemplate` | *(required)* | Template/OCI volume, e.g. `local:vztmpl/debian-12-standard_….tar.zst` |
| `--proxmox-ostype` | `debian` | `debian`, `ubuntu`, `alpine`, `unmanaged`, … |
| `--proxmox-rootfs-size` | `8` | Rootfs size in GiB |
| `--proxmox-bridge` | `vmbr0` | Bridge `net0` attaches to |
| `--proxmox-ip-address` | `""` | Static CIDR; empty uses DHCP |
| `--proxmox-gateway` | `""` | Gateway for a static address |
| `--proxmox-dns-servers` | `""` | Comma-separated DNS IPs |
| `--ip-base-network` / `--ip-pool-size` | `""` / `0` | Shared IP pool for automatic allocation |
| `--proxmox-memory` / `--proxmox-swap` / `--proxmox-cores` | `512` / `512` / `1` | Container resources |
| `--proxmox-ssh-key` | `/var/wsd/agent_ed25519` | Private key for the transport |
| `--proxmox-ssh-public-key` | `<ssh-key>.pub` | Public key injected via `ssh-public-keys` |
| `--proxmox-login-user` | `root` | Guest SSH account |
| `--proxmox-boot-timeout` | `5m` | Time to wait for SSH readiness |

The daemon's `--help` shows every `--proxmox-*` flag with its current default.

## 6. Cleanup and orphans

Normal teardown (`DELETE /workspaces/{id}`), TTL pruning, and the startup prune
all stop and delete the container via the API and release its pooled IP. To
sweep leftover `wsd-*` containers that no live workspace tracks:

```sh
mcwsd cleanup --plugin plugins/proxmox-lxc.js --proxmox-api-url https://pve02.localdomain:8006 --proxmox-node proxmox --orphans
```

This deletes only containers whose hostname starts with `wsd-`; unrelated
containers are never touched.

## 7. Troubleshooting

- **`proxmox API token not found`**: `PVE_API_TOKEN` is not exported on the
  daemon host, or `--proxmox-token-env` names the wrong variable.
- **`curl … failed` / connection refused**: wrong `--proxmox-api-url`, the node
  is down, or the daemon host cannot reach port `8006`.
- **create task fails**: the token lacks `VM.Allocate`/`VM.Config.Disk`, the
  storage is full, or `--proxmox-ostemplate` is not present on that storage.
- **timed out waiting for a task/container**: watch `mcwsd … 2>&1 | grep
  '\[DEBUG\]'`, or check the Proxmox task log in the web UI.
- **timed out waiting for an IPv4 address** (DHCP): the bridge has no DHCP
  server for that subnet, or the container cannot reach it.
- **timed out waiting for SSH**: the template lacks `sshd`, the public key was
  not injected, or the container IP is not routable from the daemon host.
- **SSH connects but commands fail**: a non-root login user's
  `sudo`/`doas` policy does not permit the wrapped command; see
  `--proxmox-guest-escalation-command`.

## 8. Provisioning hints

Use the daemon-level `--provisioning-hints-file` to tell the agent what the
container provides:

```json
{
  "notes": "This Debian container has apt. git and ca-certificates are installed by the daemon; install additional packages with apt. Network is always available."
}
```
