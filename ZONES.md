# OpenIndiana / illumos Zones runtime setup

This document describes how to configure an OpenIndiana (or other illumos)
host so that `mcwsd` can provision workspaces as native zones
(`--runtime zones`). It covers the required ZFS layout, zone brands, the
network models, privilege escalation with `pfexec`, running the daemon, and
troubleshooting.

The FreeBSD jail runtime is the reference implementation;
[FREEBSD-SETUP.md](FREEBSD-SETUP.md) describes its host setup.

> **Status:** the `zones` runtime is implemented (`--runtime zones`, alias
> `--runtime openindiana`). Flags use the `--zones-` prefix so the same runtime
> can serve OpenIndiana, OmniOS, SmartOS, and other illumos distributions.

---

## 1. How mcwsd uses the host

For every workspace, `mcwsd`:

1. Selects a base: either a zone installed from the `ipkg` brand, or an
   administrator-built **golden zone** that is cloned.
2. Creates the zone configuration with `zonecfg`.
3. Installs (`zoneadm -z <zone> install`) or clones
   (`zoneadm -z <zone> clone <golden>`) the zone.
4. Adds the workspace repository and dependency cache as `lofs` file-system
   resources, and writes DNS configuration into the zone root.
5. Boots the zone with `zoneadm -z <zone> boot`.
6. Runs commands inside it with `zlogin`.
7. On destroy, halts the zone (`zoneadm -z <zone> halt`) and removes it
   (`zoneadm -z <zone> uninstall -F`, then `zonecfg -z <zone> delete -F`).

Zone names are `wsd-<workspace-id>` (for example `wsd-ws-71670485b`).

`mcwsd` manages zones **directly**. It does not enable a zone's `autoboot`
property, does not configure the global zone's networking, and does not build or
patch the operating system image. Host preparation is the administrator's job,
exactly as described in this document.

### Paths

| Path | Purpose |
|------|---------|
| `<zonepath>/` | Parent ZFS dataset for all zones, e.g. `/rpool1/zones` |
| `<zonepath>/wsd-<workspace-id>/` | Per-workspace zone root (created by `zoneadm`) |
| `<zonepath>/wsd-golden/` | Optional golden zone used as a clone source |
| `<workspace-base-path>/<workspace-id>/` | Host-side repo checkout, lofs-mounted into the zone |
| `<state-file>` | Workspace tracking file (default `/var/lib/wsd/state.json`) |
| `<cache-dir>` | Dependency cache, lofs-mounted into the zone (default `/var/lib/wsd/cache`) |

The parent of every zone path **must be a ZFS dataset**. A plain directory on a
ZFS filesystem is not enough; `zoneadm install` creates a child dataset for the
zone, and that requires the parent to be a dataset itself.

---

## 2. Base zones: `ipkg` install or golden clone

OpenIndiana's `ipkg` brand installs a minimal IPS image at
`<zonepath>/root` when `zoneadm install` runs. That works for a single zone,
but it has two drawbacks for a daemon:

- The brand installer (`/usr/lib/brand/ipkg/pkgcreatezone`) must be present on
  the host. A partially installed OpenIndiana image can be missing it; see
  [Troubleshooting](#8-troubleshooting).
- On first boot the zone runs `sysding(1M)`, which asks for system
  configuration on the console and sets root's password to `NP`. Until that
  completes, `zlogin` cannot log in. That is unsuitable for unattended
  provisioning.

For production, build one **golden zone**, complete its first boot and package
installation, then clone it for each workspace. Cloning uses a ZFS snapshot and
is fast even when the source is large.

### 2a. Build the golden zone

Run these commands as root on the OpenIndiana host:

```sh
# Pick a name and a path on the zones dataset.
golden=wsd-golden
zonepath=/rpool1/zones/$golden

zonecfg -z "$golden"
```

At the `zonecfg` prompt:

```text
create
set zonepath=/rpool1/zones/wsd-golden
set brand=ipkg
set autoboot=false
set ip-type=shared
add net
set physical=net0
set address=192.168.1.240/24
set defrouter=192.168.1.1
end
verify
commit
exit
```

Then install and boot it:

```sh
zoneadm -z wsd-golden install
zoneadm -z wsd-golden boot
zlogin -C wsd-golden
```

Complete `sysding` on the console, then install the baseline inside the zone:

```sh
zlogin wsd-golden pkg refresh --full
zlogin wsd-golden pkg install -y git ca-certificates
zlogin wsd-golden git config --system --add safe.directory '*'
```

Install the toolchains your workspaces need. `mcwsd` does not verify or install
project dependencies; a missing tool only wastes the user's time later.

Before using the zone as a template, clean per-instance state:

```sh
zoneadm -z wsd-golden halt
zlogin -C wsd-golden   # optional last check before halting
```

With the zone halted:

- remove `/var/log/*`, `/var/adm/*`, and shell history files;
- remove `/etc/ssh/ssh_host_*` if the zone will run `sshd`, so every clone
  regenerates its own host keys;
- remove any credentials, tokens, host-specific files, and workspace checkouts.

Do **not** boot the golden zone while clones exist, and do not modify it after
cloning. Treat it as immutable. If you need to change it, build a new golden
zone (`wsd-golden-v2`) instead.

### 2b. Clone the golden zone

For each workspace, `mcwsd` creates the clone configuration from the golden
zone and then clones its installed contents:

```sh
zoneadm -z wsd-golden halt
zonecfg -z wsd-ws-71670485b
```

At the `zonecfg` prompt:

```text
create -t wsd-golden
set zonepath=/rpool1/zones/wsd-ws-71670485b
set autoboot=false
remove net
add net
set physical=net0
set address=192.168.1.50/24
set defrouter=192.168.1.1
end
verify
commit
exit
```

```sh
zoneadm -z wsd-ws-71670485b clone wsd-golden
zoneadm -z wsd-ws-71670485b boot
```

The source zone must be halted for the clone to succeed. Every clone needs its
own `zonepath` and its own network address (or VNIC). Do not give two running
zones the same shared-IP address and the same ports.

### 2c. `ipkg` install without a golden zone

If you do not use a golden zone, `mcwsd` runs `zoneadm install` and then
bootstraps the package manager inside the zone. The first boot is still
interactive unless you supply an OpenIndiana `sysding` profile or pre-seed the
zone root. For unattended operation, the golden-zone path above is strongly
recommended.

---

## 3. Requirements

- OpenIndiana Hipster or another illumos distribution with the `ipkg` zone
  brand (`system/zones` installed).
- `zonecfg(1M)`, `zoneadm(1M)`, `zlogin(1)`, `dladm(1M)`, and `zfs(1M)` in
  the host `PATH`.
- The parent of the zone path is a ZFS dataset (`zfs list` shows it).
- Root, or an account that can use `pfexec` with the `Zone Management` profile.
  For exclusive-IP zones, also grant `Network Management` so `dladm` can create
  and delete VNICs.
- A completed golden zone (recommended), or a host whose `ipkg` brand scripts
  are present and a plan for unattended first boot.
- For exclusive-IP zones: a dedicated data link, normally a VNIC created from a
  physical uplink.

> `mcwsd` escalates host commands with `pfexec` by default. It also supports
> `sudo`, `doas`, or any compatible command through `--zones-escalation`.

---

## 4. Filesystem layout

Create the parent dataset once. The pool name is whatever your host uses
(`rpool`, `rpool1`, ...); confirm it with `zpool list`.

```sh
zpool list
zfs create rpool1/zones
zfs list rpool1/zones
```

The resulting layout is:

```
rpool1/zones/                  <- ZFS dataset, <zonepath>
├── wsd-golden/               <- optional golden zone root
│   └── root/
└── wsd-ws-<workspace-id>/    <- per-workspace zone root
    └── root/
```

`mcwsd` does not create the parent dataset, does not create pools, and does not
manage snapshots directly. `zoneadm install` and `zoneadm clone` create and
destroy the per-zone datasets.

### Working directories

The daemon account needs writable directories for checkouts, cache, state, and
logs:

```sh
mkdir -p /var/lib/wsd/cache /var/wsd/workspaces /var/log
chown -R mcwsd:mcwsd /var/lib/wsd /var/wsd /var/log
chmod 700 /var/lib/wsd /var/wsd
```

The workspace directory and the cache are exposed to the zone with `lofs`
resources. The zone root itself is owned by root, so writes there are escalated.

---

## 5. Networking

OpenIndiana zones have two networking models:

- **shared-IP** (`ip-type=shared`): the zone shares the global zone's IP stack,
  routing table, and firewall. The zone gets an address on an existing data
  link. This is the default and the closest analogue to FreeBSD's shared jail
  networking.
- **exclusive-IP** (`ip-type=exclusive`): the zone gets a dedicated data link,
  normally a VNIC, and runs its own IP stack. It can use DHCP or static
  addressing and is not subject to the global zone's shared-stack firewall.

`--zones-network-mode` selects `shared` (default) or `exclusive`.

### Shared-IP, host networking (default)

With no `--zones-ip-address`, the zone uses the global zone's IP stack and its
primary address. The zone shares the host's port space, so two zones cannot bind
the same TCP/UDP port. This is convenient for a single workspace per host but
not for many concurrent zones.

```sh
mcwsd daemon --runtime zones \
  --zones-zonepath /rpool1/zones \
  --zones-network-mode shared \
  --zones-interface net0 \
  --port 9080
```

The equivalent zone configuration uses a `net` resource. If your OpenIndiana
release rejects the global zone's address as a duplicate, assign a dedicated
unused address on the same subnet instead (see below).

### Shared-IP, static address

Give each zone its own unused IPv4 address on the same LAN as the global zone:

```sh
mcwsd daemon --runtime zones \
  --zones-zonepath /rpool1/zones \
  --zones-network-mode shared \
  --zones-interface net0 \
  --zones-ip-address 192.168.1.50/24 \
  --zones-defrouter 192.168.1.1 \
  --port 9080
```

The generated configuration is:

```text
add net
set physical=net0
set address=192.168.1.50/24
set defrouter=192.168.1.1
end
```

Find the real data-link name with `dladm show-phys` or `dladm show-link`.
Reserve the address, or ensure the DHCP server excludes it. Never assign the
global zone's address to a zone that must run its own services on the same
ports.

### Exclusive-IP with DHCP

Exclusive-IP gives the zone its own stack. The daemon creates a VNIC from the
uplink and delegates it to the zone; the zone runs a DHCP client on it.

```sh
mcwsd daemon --runtime zones \
  --zones-zonepath /rpool1/zones \
  --zones-network-mode exclusive \
  --zones-interface e1000g0 \
  --port 9080
```

Host-side setup per zone:

```sh
dladm create-vnic -l e1000g0 wsd-ws-71670485b0
```

Zone configuration:

```text
create
set zonepath=/rpool1/zones/wsd-ws-71670485b
set brand=ipkg
set autoboot=false
set ip-type=exclusive
add net
set physical=wsd-ws-71670485b0
end
verify
commit
exit
```

After boot, configure DHCP inside the zone:

```sh
zlogin wsd-ws-71670485b ipadm create-ip net0
zlogin wsd-ws-71670485b ipadm create-addr -T dhcp net0/v4
```

### Exclusive-IP with a static address

Pass an address and router; the daemon configures the zone's interface after
boot:

```sh
mcwsd daemon --runtime zones \
  --zones-zonepath /rpool1/zones \
  --zones-network-mode exclusive \
  --zones-interface e1000g0 \
  --zones-ip-address 192.168.1.50/24 \
  --zones-defrouter 192.168.1.1 \
  --zones-dns-servers 192.168.1.1 \
  --port 9080
```

Inside the zone:

```sh
ipadm create-ip net0
ipadm create-addr -T static -a 192.168.1.50/24 net0/v4
route -p add default 192.168.1.1
```

The address is the zone's own; it must not collide with the global zone or any
other zone.

### Automatic IP allocation

For a shared-IP or exclusive-IP static deployment, the daemon can allocate
addresses from a pool, mirroring the FreeBSD runtime's
`--freebsd-ip-base-network` / `--freebsd-ip-pool-size`:

```sh
mcwsd daemon --runtime zones \
  --zones-zonepath /rpool1/zones \
  --zones-network-mode shared \
  --zones-interface net0 \
  --zones-ip-base-network 192.168.1.200/24 \
  --zones-ip-pool-size 50 \
  --zones-defrouter 192.168.1.1 \
  --zones-dns-servers 192.168.1.1 \
  --port 9080
```

Addresses start at `192.168.1.200` and are tracked per workspace in the state
file. Reserve the pool range on the DHCP server. The automatic allocation is
persisted across daemon restarts as long as the state file survives.

### DNS inside zones

An `ipkg` zone is installed with a minimal image and may not have a useful
`/etc/resolv.conf`. By default `mcwsd` copies the global zone's
`/etc/resolv.conf` into `<zonepath>/root/etc/resolv.conf` before boot. With
`--zones-dns-servers`, it writes the supplied servers instead:

```sh
--zones-dns-servers 192.168.1.1,1.1.1.1
```

Static exclusive-IP requires explicit DNS servers; DHCP supplies them. DNS
writes are confined to the zone root and escalated with the configured command.

### Per-workspace override

`interface` and `ip4_address` may also be supplied in the `POST /workspaces`
body and take precedence over the daemon flags:

```json
{
  "repo_url": "git@example.com:team/repo.git",
  "interface": "net0",
  "ip4_address": "192.168.1.101/24"
}
```

### Network-off behavior

Exclusive-IP zones can be isolated at runtime by bringing the delegate VNIC
down. Shared-IP zones share the global zone's stack; there is no per-zone link
to take down, so the daemon does not firewall them. Use exclusive-IP if you
need per-workspace network isolation.

### Repository access inside zones

OpenIndiana can proxy the host's IPS publishers into zones. For environments
where zones have no direct Internet access, install the proxy packages in the
global zone and the client in the golden zone:

```sh
pkg install pkg:/package/pkg/system-repository pkg:/package/pkg/zones-proxy
svcadm enable svc:/application/pkg/system-repository:default
svcadm enable svc:/application/pkg/zones-proxyd:default

zlogin wsd-golden pkg install pkg:/package/pkg/zones-proxy
zlogin wsd-golden svcadm enable svc:/application/pkg/zones-proxy-client:default
zlogin wsd-golden pkg set-property use-system-repo True
```

This is optional; direct network access works as usual.

---

## 6. Running wsd

Build and install:

```sh
cd /path/to/mcwsd
go build -o /usr/local/bin/mcwsd cmd/mcwsd/main.go
```

### `--zones-*` flags

These mirror the FreeBSD jail flags.

| Flag | Description | Default |
|------|-------------|---------|
| `--runtime` | `zones` (alias `openindiana`) for the Zones runtime | `docker` |
| `--zones-zonepath` | Parent ZFS dataset for zone roots | *(required)* |
| `--zones-brand` | Zone brand; `ipkg` is the documented brand | `ipkg` |
| `--zones-base-zone` | Installed, halted golden zone to clone instead of `zoneadm install` | *(empty)* |
| `--zones-network-mode` | `shared` or `exclusive` | `shared` |
| `--zones-interface` | Global-zone data link (shared) or VNIC uplink (exclusive) | *(empty)* |
| `--zones-ip-address` | Shared: IPv4/prefix. Exclusive: static IPv4/prefix, empty for DHCP | *(empty)* |
| `--zones-defrouter` | Default router | *(empty)* |
| `--zones-ip-base-network` | CIDR pool for automatic address allocation | *(empty)* |
| `--zones-ip-pool-size` | Number of addresses to allocate | `0` |
| `--zones-dns-servers` | Comma-separated DNS server IPs; default is the host resolver | *(empty)* |
| `--zones-escalation` | Privilege escalation command: `pfexec`, `sudo`, `doas`, or empty | `pfexec` |
| `--zones-boot-timeout` | How long to wait for a booted zone to accept `zlogin` | `5m` |
| `--workspace-base-path` | Host dir for per-workspace checkouts | `~/wsd-workspaces` |
| `--cache-dir` | Dependency cache directory | `/var/lib/wsd/cache` |
| `--state-file` | Workspace tracking file | `/var/lib/wsd/state.json` |
| `--bind-address` | HTTP bind address | `127.0.0.1` |
| `--port` | HTTP listen port | `8080` |

> **Use absolute paths.** `~` is not expanded. Pass an absolute
> `--workspace-base-path`, such as `/var/wsd/workspaces`.

### Example: shared-IP with automatic addresses

```sh
MCWSD_TOKEN="$(openssl rand -hex 16)" \
mcwsd daemon --runtime zones \
  --zones-zonepath /rpool1/zones \
  --zones-network-mode shared \
  --zones-interface net0 \
  --zones-ip-base-network 192.168.1.200/24 \
  --zones-ip-pool-size 50 \
  --zones-defrouter 192.168.1.1 \
  --zones-dns-servers 192.168.1.1 \
  --zones-escalation pfexec \
  --workspace-base-path /var/wsd/workspaces \
  --cache-dir /var/lib/wsd/cache \
  --state-file /var/lib/wsd/state.json \
  --port 9080
```

### Example: golden zone and exclusive-IP DHCP

```sh
mcwsd daemon --runtime zones \
  --zones-zonepath /rpool1/zones \
  --zones-base-zone wsd-golden \
  --zones-network-mode exclusive \
  --zones-interface e1000g0 \
  --zones-escalation pfexec \
  --workspace-base-path /var/wsd/workspaces \
  --cache-dir /var/lib/wsd/cache \
  --state-file /var/lib/wsd/state.json \
  --port 9080
```

### Run as an SMF service

OpenIndiana uses SMF, not rc(8). Install a start wrapper that loads secrets
from a root-only file:

`/usr/local/lib/wsd/start.sh`:

```sh
#!/bin/sh
set -a
[ -f /etc/mcwsd/env ] && . /etc/mcwsd/env
set +a
exec /usr/local/bin/mcwsd daemon \
  --runtime zones \
  --zones-zonepath /rpool1/zones \
  --zones-network-mode shared \
  --zones-interface net0 \
  --zones-ip-base-network 192.168.1.200/24 \
  --zones-ip-pool-size 50 \
  --zones-defrouter 192.168.1.1 \
  --zones-dns-servers 192.168.1.1 \
  --zones-escalation pfexec \
  --workspace-base-path /var/wsd/workspaces \
  --cache-dir /var/lib/wsd/cache \
  --state-file /var/lib/wsd/state.json \
  --port 9080
```

Make it executable and create the secrets file:

```sh
chmod 0755 /usr/local/lib/wsd/start.sh
install -m 600 /dev/null /etc/mcwsd/env
printf 'MCWSD_TOKEN=%s\n' "$(openssl rand -hex 16)" > /etc/mcwsd/env
```

SMF manifest, `/var/svc/manifest/site/mcwsd.xml`:

```xml
<?xml version="1.0"?>
<!DOCTYPE service_bundle SYSTEM "/usr/share/lib/xml/dtd/service_bundle.dtd.1">
<service_bundle type="manifest" name="mcwsd">
  <service name="site/mcwsd" type="service" version="1">
    <create_default_instance enabled="false"/>
    <single_instance/>

    <dependency name="network" grouping="require_all" restart_on="error" type="service">
      <service_fmri value="svc:/milestone/network:default"/>
    </dependency>
    <dependency name="filesystem" grouping="require_all" restart_on="none" type="service">
      <service_fmri value="svc:/system/filesystem/local"/>
    </dependency>

    <exec_method type="method" name="start"
      exec="/usr/local/lib/wsd/start.sh" timeout="60">
      <method_context>
        <method_credential user="mcwsd" group="mcwsd" />
      </method_context>
    </exec_method>
    <exec_method type="method" name="stop" exec=":kill" timeout="60"/>

    <property_group name="startd" type="framework">
      <propval name="duration" type="astring" value="contract"/>
    </property_group>

    <stability value="Unstable"/>
    <template>
      <common_name>
        <loctext xml:lang="C">Workspace Daemon</loctext>
      </common_name>
    </template>
  </service>
</service_bundle>
```

Import and enable it:

```sh
svccfg validate /var/svc/manifest/site/mcwsd.xml
svccfg import /var/svc/manifest/site/mcwsd.xml
svcadm enable site/mcwsd
svcs site/mcwsd
```

Logs go to the SMF log:

```sh
svcs -L site/mcwsd
tail -f /var/svc/log/site-mcwsd:default.log
```

---

## 7. Verifying and operating

After a workspace is provisioned:

```sh
zoneadm list -cv                 # list configured/running zones
zlogin wsd-ws-xxxx               # open a shell in a zone
zfs list -r rpool1/zones         # see per-zone datasets
dladm show-vnic                  # see delegated VNICs
```

Stopping and removing a zone by hand (normally `mcwsd` does this on destroy):

```sh
zoneadm -z wsd-ws-xxxx halt
zoneadm -z wsd-ws-xxxx uninstall -F
zonecfg -z wsd-ws-xxxx delete -F
```

`uninstall -F` skips the confirmation prompt; it does **not** force-uninstall a
running zone. Halt first.

Daemon housekeeping:

```sh
mcwsd cleanup --runtime zones --zones-zonepath /rpool1/zones \
  --zones-network-mode shared --zones-escalation pfexec --prune
mcwsd cleanup --runtime zones --zones-zonepath /rpool1/zones \
  --zones-base-zone wsd-golden \
  --zones-network-mode shared --zones-escalation pfexec --orphans
```

### Debug logging

```sh
mcwsd daemon --runtime zones ... --debug 2>&1 | grep -v '\[DEBUG\]'
```

---

## 8. Troubleshooting

### `/usr/lib/brand/ipkg/pkgcreatezone: not found`

`zoneadm install` cannot find the brand installer scripts. Check the host:

```sh
ls -l /usr/lib/brand/ipkg
pkg list system/zones
pkg contents -m system/zones | grep 'usr/lib/brand/ipkg'
```

If only `config.xml` and `platform.xml` are present, the installed `system/zones`
package is incomplete for this image. Refresh and update the image, or reinstall
the package:

```sh
pkg refresh --full
pkg update
# If the image is pinned, install the package explicitly:
pkg install system/zones
```

Then confirm that `pkgcreatezone`, `uninstall`, `clone`, and the other scripts
are present:

```sh
ls -l /usr/lib/brand/ipkg
```

Do **not** copy brand scripts from another Solaris or illumos release and do not
hand-create them; they perform package-image creation and removal.

### `zone is incomplete; uninstall required`

A failed `zoneadm install` left the zone in `incomplete` state. After fixing the
brand scripts, remove the configuration and try again:

```sh
zoneadm -z wsd-ws-xxxx uninstall
zonecfg -z wsd-ws-xxxx delete -F
```

Then recreating the zone and running `zoneadm install` again.

### `ERROR: the zonepath must be a ZFS dataset`

The parent of the zone path is a plain directory, not a ZFS dataset. Create the
parent dataset:

```sh
zpool list
zfs create rpool1/zones
zfs list rpool1/zones
```

Then set `--zones-zonepath` to the dataset's mountpoint. Do not point it at
`/zones` when the dataset is mounted at `/rpool1/zones`, and vice versa.

### `ping: sendto: No route to host`

The zone was created with shared-IP networking but has no `net` resource, or the
address is not on the host's subnet. Check the configuration:

```sh
zonecfg -z wsd-ws-xxxx info
dladm show-phys
ipadm show-addr
```

Add a `net` resource with the correct data link, address, and router, or use
exclusive-IP with a VNIC.

### `pfexec` does not escalate / `Permission denied`

The daemon account needs an RBAC profile. On OpenIndiana the reliable choice is
the built-in `Zone Management` profile:

```sh
usermod -P "Zone Management" mcwsd
```

For exclusive-IP zones, add `Network Management` as well:

```sh
usermod -P "Zone Management,Network Management" mcwsd
```

Log the account out and back in after changing profiles; `usermod` warns that
changes may not take effect until the next login. Verify from the service
account:

```sh
su - mcwsd
pfexec id
pfexec zoneadm list -cv
```

Do not rely on `solaris.zone.manage/<zone>` or `solaris.zone.login/<zone>` names
on OpenIndiana; they are not consistently defined. Use the profile instead.

### `zlogin` cannot log in after first boot

An `ipkg` zone runs `sysding(1M)` on first boot and sets root's password to
`NP` until configuration completes. Use the console once:

```sh
zlogin -C wsd-ws-xxxx
```

For unattended operation, complete this in the golden zone before cloning (see
§2).

### Clone fails because the source is running

Halt the source first:

```sh
zoneadm -z wsd-golden halt
zoneadm -z wsd-ws-xxxx clone wsd-golden
```

Do not boot the golden zone after clones exist.

### `zoneadm uninstall -F` refuses on a running zone

`-F` only suppresses the confirmation prompt. Halt first:

```sh
zoneadm -z wsd-ws-xxxx halt
zoneadm -z wsd-ws-xxxx uninstall -F
```

### A zone will not boot

Read the daemon log; it records the full `zonecfg`, `zoneadm`, and `zlogin`
command lines. Useful host checks:

```sh
zoneadm -z wsd-ws-xxxx list -v
zoneadm list -cv
zonecfg -z wsd-ws-xxxx info
zfs list -r rpool1/zones
dladm show-vnic
```

### VNIC creation fails

The uplink may not exist, or the VNIC name may collide:

```sh
dladm show-phys
dladm show-vnic
dladm create-vnic -l <uplink> <unique-name>
```

VNIC names are limited by the data-link name length. `mcwsd` derives a short,
stable name from the workspace ID; do not reuse one.

---

## 9. What mcwsd does not do

- No `zpool create`, no parent `zones` dataset creation, and no ZFS quota or
  snapshot management beyond what `zoneadm` performs.
- No OpenIndiana image update, package installation on the host, or repair of
  `system/zones` / brand scripts.
- No global-zone network configuration (`ipadm`, NWAM, DHCP server, VLANs).
- No NAT, firewall, or routing configuration.
- No interactive `sysding` / first-boot configuration.
- No zone `autoboot` management; the daemon owns the zone lifecycle.
- No direct manipulation of zone roots for `ipkg` installs; it uses `zoneadm`.

---

## 10. Host configuration checklist

1. Create the service account:

   ```sh
   useradd -m -s /usr/bin/bash mcwsd
   ```

2. Grant RBAC profiles and re-login:

   ```sh
   usermod -P "Zone Management" mcwsd
   # Exclusive-IP only:
   usermod -P "Zone Management,Network Management" mcwsd
   ```

3. Create the zones dataset:

   ```sh
   zpool list
   zfs create rpool1/zones
   ```

4. Confirm the `ipkg` brand scripts exist:

   ```sh
   ls -l /usr/lib/brand/ipkg
   ```

   If they are missing, update the image or reinstall `system/zones` (§8).

5. Create working directories and the secrets file:

   ```sh
   mkdir -p /var/lib/wsd/cache /var/wsd/workspaces /usr/local/lib/wsd
   chown -R mcwsd:mcwsd /var/lib/wsd /var/wsd
   chmod 700 /var/lib/wsd /var/wsd
   printf 'MCWSD_TOKEN=%s\n' "$(openssl rand -hex 16)" > /etc/mcwsd/env
   chmod 600 /etc/mcwsd/env
   ```

6. Build the golden zone (§2), or accept an interactive first boot.

7. Verify `pfexec` from the service account:

   ```sh
   su - mcwsd
   pfexec zoneadm list -cv
   pfexec zlogin <zone> id
   ```

8. Install and enable the SMF service (§6).

9. Provision a test workspace and confirm networking:

   ```sh
   zoneadm list -cv
   zlogin wsd-ws-xxxx ping -c 3 1.1.1.1
   zlogin wsd-ws-xxxx pkg refresh
   ```

### Escalation commands

`--zones-escalation` defaults to `pfexec`. If you switch to `sudo` or `doas`,
that command must be permitted to run at least:

- `zonecfg`
- `zoneadm`
- `zlogin`
- `dladm` (exclusive-IP only)
- `chroot` (mount-point and resolver setup confined to the zone root)
- `zfs` (read-only dataset validation; may be run without escalation)

With `pfexec`, the daemon user's RBAC profile must permit these commands as
well. If `Zone Management` alone does not include `chroot`, add a profile that
does, or use `sudo`/`doas` for the daemon's escalation. The escalation command
must be non-interactive: `pfexec`/RBAC does not prompt, and `sudo` needs a
`NOPASSWD` rule for the daemon account.

The daemon does not escalate Go filesystem syscalls; only spawned commands are
escalated. The zone parent dataset and the daemon's working directories must
remain writable by the daemon account.

### Biggest tip

Follow the [OpenIndiana Systems Administration handbook](https://docs.openindiana.org/handbook/systems-administration/)
for the host side. `mcwsd` deliberately delegates zone creation, cloning, and
removal to `zonecfg` and `zoneadm` rather than reimplementing them.
