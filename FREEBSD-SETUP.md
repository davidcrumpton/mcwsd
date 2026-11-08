# FreeBSD jail runtime setup

This document describes how to configure a FreeBSD server so that `mcwsd` can
provision workspaces as FreeBSD jails. The jail runtime is shipped as a script
plugin (`plugins/freebsd-jail.js`); run the daemon with
`--plugin /usr/local/lib/mcwsd/plugins/freebsd-jail.js`. It covers the
required layout, networking, running the daemon, and troubleshooting.

If you are looking for the design/implementation notes for the jail
provisioner itself, see [FREEBSD.md](FREEBSD.md).


---

## 1. How mcwsd uses the host

For every workspace, `mcwsd`:

1. Downloads (once per release) `base.txz` and caches it at
   `/usr/local/jails/media/<release>/base.txz`, or downloads a configured
   administrator-built template archive (see below).
2. Creates a **per-workspace** jail root at
   `/usr/local/jails/containers/<jail-name>/` and extracts `base.txz` into it.
3. Writes a jail config to `/etc/jail.conf.d/<jail-name>.conf`.
4. Starts the jail with `jail -c -f /etc/jail.conf.d/<jail-name>.conf`.
5. Runs commands inside it with `jexec`.
6. On destroy, stops the jail (`jail -r`), removes its config, and removes its
   root.

Jail names are `wsd-<workspace-id>` (for example `wsd-ws-71670485b`).

`wsd` manages jails **directly**. It does not use the `jail` rc(8) service,
`jail_list`, `bsdinstall`, or ZFS cloning. VNET mode uses the base-system
`/usr/local/sbin/jib` helper for per-jail epair and bridge plumbing.

### Paths (fixed in the current build)

| Path | Purpose |
|------|---------|
| `/etc/jail.conf.d/` | Generated per-jail config files |
| `/usr/local/jails/` | Base jail directory |
| `/usr/local/jails/media/<release>/base.txz` | Cached release archive |
| `/usr/local/jails/containers/<jail-name>/` | Per-workspace jail root |
| `<workspace-base-path>/<workspace-id>/` | Host-side repo checkout, nullfs-mounted into the jail |

The `base.txz` URL is
`https://download.freebsd.org/releases/<arch>/<machine>/<release>/base.txz`
(for example `.../amd64/amd64/15.1-RELEASE/base.txz`). The architecture is
auto-detected from the host.

---

## Custom jail archive templates

A sysadmin can prepare a FreeBSD jail with the organization's packages and
configuration, then distribute its complete root filesystem as a `.tgz`.
Every workspace gets a separate writable extraction; this does not require
ZFS and is not a shared-base thin jail.

### 1. Build a dedicated source jail

Run these commands as root **on the FreeBSD host**, not inside an existing
workspace. A dedicated source jail avoids accidentally packaging a workspace
checkout, its cache, or the host's mounted `/tmp`.

Use the host architecture and a compatible FreeBSD release. This example
uses amd64 and 15.1-RELEASE; adjust them for your host:

```sh
template_root=/usr/local/jails/templates/organization-15.1
mkdir -p "$template_root" /usr/local/jails/media
fetch -o /usr/local/jails/media/15.1-base.txz \
  https://download.freebsd.org/releases/amd64/amd64/15.1-RELEASE/base.txz
tar -xf /usr/local/jails/media/15.1-base.txz -C "$template_root"
cp /etc/resolv.conf "$template_root/etc/resolv.conf"

jail -c name=mcwsd-template \
  path="$template_root" host.hostname=mcwsd-template \
  ip4=inherit mount.devfs persist
jexec mcwsd-template env ASSUME_ALWAYS_YES=yes pkg bootstrap -f
jexec mcwsd-template pkg install -y rust go python3

jexec mcwsd-template rustc --version
jexec mcwsd-template go version
jexec mcwsd-template python3 --version
```

Use a new, empty `template_root`; do not extract over another jail's root.
The temporary jail above shares the host network stack for package downloads.
It does not boot services automatically. Install other packages and customize
files under `$template_root/etc` and `$template_root/usr/local/etc` as needed.
Keep credentials, host-specific secrets, and workspace checkouts out of the
template.

If you prefer to customize an existing mcwsd workspace jail, its root is
`/usr/local/jails/containers/<jail-name>`. Use that path and jail name in the
next step, and account for all its workspace, cache, and `/tmp` mounts.
**Do not destroy the workspace through mcwsd before archiving it:** destruction
removes its root. Stop the source jail directly with `jail -r` instead.

### 2. Stop the jail and check for mounts

On the host:

```sh
jail -r mcwsd-template
jls
mount | grep -F " on $template_root/"
```

Confirm the source jail is no longer listed by `jls`. The mount check must
show **no mounts below the source root**. A ZFS mount at the root itself is
normal; mounted devfs, nullfs, or child filesystems below it need attention.
If the command reports leftovers, unmount each exact target, deepest first,
then repeat the check. For example, only if these paths are reported:

```sh
umount "$template_root/dev"
# For a source workspace, also unmount its reported workspace/cache/tmp targets.
```

Do not blindly unmount host paths or archive a running jail. In particular,
`/dev` in a running jail is devfs, not part of the reusable root filesystem.
Its device nodes must never be packaged into the template. mcwsd mounts a
fresh devfs when it starts each new jail.

### 3. Create and inspect the archive

Choose a **new filename for every published build**, for example `v2` rather
than replacing a previously downloaded `v1`. mcwsd caches templates by URL.
Create the archive outside the source root:

```sh
cd $template_root
archive=/usr/local/jails/media/organization-15.1-v13.tgz
mkdir -p "$template_root/dev" "$template_root/tmp"
chmod 1777 "$template_root/tmp"
tar -czf "$archive" --one-file-system \
  --exclude './dev/?*' \
  --exclude './tmp/?*' \
  --exclude './workspace/?*' \
  --exclude './cache/?*' \
  -C "$template_root" .
```

Keep the standard system directories, including `/dev` and `/tmp`; omit
only their runtime contents. The `?*` patterns require at least one character
after the slash, so they exclude children without matching the directory's
own trailing-slash entry. Do not use `--exclude './tmp'` or
`--exclude './tmp/*'`: the latter can also match `./tmp/` and omit the
directory itself. `/tmp` should be mode `1777` (world-writable with the sticky bit).
mcwsd also ensures `/tmp` exists before mounting the host's `/tmp` there,
but the archive should still contain a complete root filesystem.
`--one-file-system` is an additional guard against descending into mounted
filesystems, not a substitute for stopping the jail and checking mounts.
If your source intentionally uses separate filesystems for installed packages
or configuration, stage a complete, unmounted root first; this command would
otherwise omit those filesystems.

Check the archive before publishing:

```sh
tar -tf "$archive" ./bin/sh
tar -tf "$archive" | grep -E '^\./(dev|tmp)/$'
tar -tf "$archive" | grep -E '^\./usr/local/bin/(rustc|cargo|go|python[0-9.]*)$'
tar -tvf "$archive" | awk '$1 ~ /^[cb]/ { print; bad=1 } END { exit bad }'
tar -tf "$archive" | grep -E '^\./(dev|tmp|workspace|cache)/.+'
```

The first commands should show the shell, both `./dev/` and `./tmp/`, and installed tools. The device-node
check must print nothing and exit successfully. The last check must print
nothing (grep exits 1 when it finds no matches). An empty `./dev/` entry is
fine. If device nodes or runtime contents appear, **do not publish**:
check the mounts and rebuild the archive.

The archive must contain `./bin/`, `./etc/`, `./usr/`, etc. at its top level,
not a wrapping directory such as `organization-15.1/bin/`. If `./tmp/`
or `./dev/` is missing, correct the exclusions and rebuild before publishing.

### 4. Sign and publish

Sign the finished archive on your signing machine; if your private key is
on a YubiKey, use it through GnuPG as usual. Do not change the archive after
signing it:

```sh
gpg --local-user YOUR_SIGNING_KEY_FINGERPRINT \
  --detach-sign --output organization-15.1-v2.tgz.sig organization-15.1-v2.tgz
gpg --armor --export YOUR_SIGNING_KEY_FINGERPRINT > organization-template.pub
gpg --verify organization-15.1-v2.tgz.sig organization-15.1-v2.tgz
```

```sh
gpg --local-user mcwsd@mcwsd.io \
gpg --local-user mcwsd@mcwsd.io \
--detach-sign --output organization-15.1-v2.tgz.sig $archive

gpg --armor --export mcwsd@mcwsd.io > organization-template.pub

gpg --verify organization-15.1-v2.tgz.sig $archive
```

Your private key, including a YubiKey-backed key, stays on the signing
machine. Distribute the public key to the daemon through a trusted channel
and confirm its fingerprint independently. The local public-key file is
the trust anchor; mcwsd does not fetch keys or use the GnuPG trust database.
All public keys in that file are trusted for templates. Keep it
administrator-controlled and update it for key rotations and revocations.

Upload the archive and its adjacent `.sig` file to the same directory on
the template server. For example, nginx could serve these files from
`/usr/local/www/<www-base>/freebsd/`:

```text
organization-15.1-v2.tgz
organization-15.1-v2.tgz.sig
```

Use the HTTP URL that actually serves that directory, not its host filesystem
path. Configure the rc service as shown below, or run the daemon directly:

```sh
mcwsd daemon --plugin /usr/local/lib/mcwsd/plugins/freebsd-jail.js \
  --base-template-url https://templates.example.org/freebsd \
  --freebsd-release organization-15.1-v2.tgz \
  --verify-signature \
  --template-public-key /usr/local/etc/wsd/organization-template.pub
```

`--base-template-url` is an HTTP(S) directory URL.
`--freebsd-release` becomes the archive filename in this mode. Without
`--base-template-url`, the existing official FreeBSD release download is
unchanged. Select a template built for the host architecture and a FreeBSD
userland compatible with the host kernel.

### Authentication

When using the supplied rc service, configure `/etc/rc.conf` instead of
building a daemon command:

```sh
mcwsd_base_template_url="https://templates.example.org/freebsd"
mcwsd_freebsd_release="organization-15.1-v2.tgz"
mcwsd_verify_signature="YES"
mcwsd_template_public_key="/usr/local/etc/wsd/organization-template.pub"
```

The [rc script](examples/freebsd/rc.d/mcwsd) forwards these settings for the
FreeBSD jail plugin. For Basic authentication, set
`mcwsd_base_template_user` and `mcwsd_base_template_password_env`. For header
authentication, set `mcwsd_base_template_header` and
`mcwsd_base_template_header_env` instead. Export the corresponding secret in
the protected file selected by `mcwsd_source_env_file` (default `/usr/local/etc/wsd/env`);
only the environment variable's name goes into the daemon arguments.
Leave the template settings unset to retain official-release downloads.
See the [rc.conf example](examples/freebsd/etc/rc.conf) for all template settings.
After changing `/etc/rc.conf`, restart the service with `service mcwsd restart`
and provision a new workspace. Existing workspace roots are not rebuilt.

Anonymous downloads need no extra options. For HTTP Basic authentication:

```sh
export MCWSD_TEMPLATE_PASSWORD='your-password'
# Add to the daemon command:
# --base-template-user mcwsd --base-template-password-env MCWSD_TEMPLATE_PASSWORD
```

For a custom authentication header:

```sh
export MCWSD_TEMPLATE_AUTH='Bearer your-token'
# Add to the daemon command:
# --base-template-header Authorization --base-template-header-env MCWSD_TEMPLATE_AUTH
```

Choose one authentication method. It applies to both the archive and its
signature. Credentials are taken from environment variables, not URL
credentials or command-line password arguments. URLs must not contain a
query or fragment. Redirects are restricted to the same host, port, and
scheme to avoid forwarding credentials elsewhere. Use HTTPS; HTTP is
available for internal servers but sends credentials without encryption.

### Verification and caching

`--verify-signature` requires `<archive>.sig` and
`--template-public-key`. Verification uses native Go OpenPGP support; the
daemon does not need GnuPG, `shasum`, or `md5sum`. Binary and ASCII-armored
detached signatures and public keys are supported. This is actual
public-key signature verification, not a checksum sidecar.

Missing, malformed, expired, untrusted, or invalid signatures fail
provisioning before extraction. The verifier also checks signing-key
revocation and OpenPGP metadata using the configured public-key file and
the library's verification rules. There is no automatic revocation lookup;
the sysadmin must keep the trusted key file current.

Templates are cached under `/usr/local/jails/media/templates/<URL-hash>/`.
Downloads are staged before publication and requests for a shared template
are serialized within the provisioner. Cached archives are reverified on
every use when verification is enabled. Use immutable, versioned filenames:
changing the server's archive at the same URL does not refresh the cache.
If the cached archive or signature is damaged, or verification is enabled
after downloading an unsigned template, remove that specific cached archive
and its `.sig` to download a fresh pair. Workspace deletion preserves the cache.

#### Troubleshooting: `./dev/...: Can't create ...: Operation not permitted`

This usually means the archive was made while devfs was mounted and contains
device nodes. Archive extraction uses the configured escalator to preserve
ownership, but templates must still exclude devfs: the jail mounts a fresh
devfs at startup. Rebuild from an unmounted root using the exclusions above.

If the rebuilt archive on the web server has no device nodes but provisioning
still reports them, check the **cached archive**, not just the published one:

```sh
find /usr/local/jails/media/templates -type f -name 'organization-15.1-v1.tgz'
# Substitute the exact path printed above:
tar -tvf /usr/local/jails/media/templates/URL_HASH/organization-15.1-v1.tgz \
  | awk '$1 ~ /^[cb]/ { print }'
```

Replacing `v1` on nginx does not replace mcwsd's cached `v1`. Prefer publishing
`v2` and updating `mcwsd_freebsd_release`, then restarting the service. If you
must reuse the filename, stop mcwsd and remove only that exact cached archive
and its adjacent `.sig`, then restart. Do not remove the entire media cache.

### DNS and per-workspace settings

Explicit `--freebsd-dns-servers` replaces the template's resolver configuration.
Without it, mcwsd copies the host's local `/etc/resolv.conf`, as for official
release jails. Static VNET (including automatically allocated static IPs)
requires explicit DNS servers; VNET DHCP and shared networking do not.
For custom templates, failure to install DNS configuration aborts provisioning.
The resolver path must be writable and must not be a symlink outside the jail root.

The template supplies packages and in-jail configuration. mcwsd still
generates the jail hostname, network parameters, and workspace/cache mounts
for each workspace; host-side jail configuration is not imported from the archive.

---

## 2. Requirements

- FreeBSD **14.3-RELEASE or later** (including 15.x). The exec transport uses
  `jexec -d`, which was added in 14.3. Verified on 15.1-RELEASE.
- `amd64` or `arm64`.
- **Root privileges.** Creating jails, mounting filesystems, and writing to
  `/etc/jail.conf.d` and `/usr/local/jails` all require root.
- Base-system tools, all present by default: `jail`, `jexec`, `jls`, `mount`,
  `umount`, `fetch`, `tar`.
- Shared mode does not require VIMAGE/VNET kernel support. VNET mode requires
  a kernel with VIMAGE/VNET support, the `jib` helper, and the `bpf` kernel
  module loaded (`kldload bpf` or `bpf_load="YES"` in `/boot/loader.conf`).

> The `mcwsd` daemon escalates privileges using `mdo` when creating and managing jails and should be run in a service account.

---

## 3. Filesystem layout

### 3a. Plain directories (ZFS not required)

ZFS is **optional**. Plain directories work fine:

```sh
mkdir -p /etc/jail.conf.d
mkdir -p /usr/local/jails/media
mkdir -p /usr/local/jails/containers
```

### 3b. ZFS (optional, what the FreeBSD Handbook uses)

If `/usr/local/jails` is on ZFS, the conventional layout maps cleanly onto
what `wsd` uses. `wsd` only needs the `media` and `containers` datasets; the
Handbook's `templates` dataset is **not** used by this tool.

```sh
zfs create -o mountpoint=/usr/local/jails zroot/jails
zfs create -o mountpoint=/usr/local/jails/media zroot/jails/media
zfs create -o mountpoint=/usr/local/jails/containers zroot/jails/containers

chown -R mcwsd:mcwsd
chmod 700 /usr/local/jails
```

Either way, the end result is the same:

```
/usr/local/jails/
├── media/
│   └── 15.1-RELEASE/
│       └── base.txz
└── containers/
    └── wsd-ws-<workspace-id>/
```

`wsd` never creates or destroys ZFS datasets. It creates plain directories
inside `containers/`, so no `zfs` commands are needed to operate or clean up
workspaces. (If you want per-jail quotas or snapshots, you can put each jail
root on its own dataset, but that is not required and not automated.)

---

## 4. Networking

A non-VNET jail shares the host's network stack. There are two modes.

### DHCP / inherit (default, recommended)

Start `mcwsd` **without** `--freebsd-ip4-address`. Each jail is configured with:

```
ip4 = inherit;
```

This makes the jail inherit the host's addresses (including the one `vtnet0`
obtained via DHCP). No `interface` line is needed.

Example host interface:

```
vtnet0: flags=... 
        inet 192.168.122.250 netmask 0xffffff00 broadcast 192.168.122.255
```

Every jail then uses `192.168.122.250`.

**Caveat:** because the jail shares the host's IP, it also shares its port
space. Two jails (or a jail and the host) cannot bind the same TCP/UDP port.
For stronger isolation, use fixed per-jail addresses (below).

### Fixed IP

To pin an address, pass the interface and address:

```sh
mcwsd daemon --plugin plugins/freebsd-jail.js \
    --freebsd-interface vtnet0 \
    --freebsd-ip4-address 192.168.122.100
```

The generated config becomes:

```
interface = "vtnet0";
ip4.addr += "192.168.122.100";
```

`jail(8)` adds the alias to `vtnet0` when the jail starts and removes it when
the jail is destroyed. Use a distinct address per concurrently running jail.
The subnet should already be reachable from the host (e.g. the DHCP subnet).

### VNET

VNET gives each jail its own network stack and interface. It is opt-in and
requires the host interface that should carry the jail traffic. You can use
either DHCP (simpler) or a static IP configuration.

#### VNET with DHCP (recommended)

For DHCP configuration, only the interface and network mode are required:

```sh
mcwsd daemon --plugin plugins/freebsd-jail.js \
  --freebsd-network-mode vnet \
  --freebsd-interface em0 \
  --port 9080
```

Each jail will obtain its IP address via DHCP on the jail-side vnet interface.

#### VNET with Static IP

For static IP configuration, you must also provide a CIDR address, gateway,
and explicit DNS servers:

```sh
mcwsd daemon --plugin plugins/freebsd-jail.js \
  --freebsd-network-mode vnet \
  --freebsd-interface em0 \
  --freebsd-ip4-address 192.168.122.100/24 \
  --freebsd-vnet-gateway 192.168.122.1 \
  --freebsd-dns-servers 192.168.122.1 \
  --port 9080
```

#### VNET with Automatic IP Allocation (Recommended for Static)

For automatic IP allocation from a pool, configure a base network and pool size:

```sh
mcwsd daemon --plugin plugins/freebsd-jail.js \
  --freebsd-network-mode vnet \
  --freebsd-interface em0 \
  --ip-base-network 192.168.1.200/24 \
  --ip-pool-size 50 \
  --freebsd-dns-servers 192.168.1.1 \
  --port 9080
```

This will automatically assign IPs starting from 192.168.1.200, then 192.168.1.201,
192.168.1.202, etc., up to 50 IPs total. The gateway will be auto-calculated as
192.168.1.1. WSD tracks which IPs are assigned to which workspaces.

You can also configure custom DNS servers for all jails:

```sh
mcwsd daemon --plugin plugins/freebsd-jail.js \
  --freebsd-network-mode vnet \
  --freebsd-interface em0 \
  --ip-base-network 192.168.122.200/24 \
  --ip-pool-size 50 \
  --freebsd-dns-servers "192.168.122.1,1.1.1.1" \
  --freebsd-vnet-gateway 192.168.122.1 \
  --port 9080 --debug
```

Note: When using automatic IP allocation, WSD manages the IP assignments. Each
workspace gets one IP from the pool, and IPs are tracked across daemon restarts
(as long as the state file is preserved).

The host must have the `jib` helper available and a bridge-capable uplink.
wsd runs `jib addm <jib-name> <uplink-interface>` before the jail starts, then
`jib destroy <jib-name>` after it stops. The uplink is the host-side interface,
commonly `em0`. `jib` creates or reuses a derived bridge (by default
`em0bridge`) and an epair. It prefixes the supplied name with `e0b_` for the
jail-side interface, so the name must be short enough to fit FreeBSD's
15-character interface-name limit. wsd derives a short, stable jib name from
the jail name; for example, jail `wsd-ws-ca4c1bebd` uses jib name
`wsca4c1bebd` and jail-side interface `e0b_wsca4c1bebd`. Its optional `-b`
argument changes the bridge-name suffix; it does not select an existing bridge
by its full name. The current wsd integration uses `jib`'s default bridge
naming and does not attach epairs to a separately managed bridge such as
`bridge0`.

For static IP configuration: use an unused host address in the CIDR, not the
subnet address: for example, `192.168.122.100/24`, not `192.168.122.0/24`. The
gateway must be the actual router address and reachable on the network.

Network-off behavior remains active in VNET mode: wsd brings the jail-side
interface down when the workspace is not explicitly allowed network access,
and brings it back up for an allowed operation.

### Per-workspace override

`interface` and `ip4_address` can also be supplied per request in the
`POST /workspaces` body and take precedence over the daemon flags:

```json
{
  "repo_url": "git@example.com:team/repo.git",
  "interface": "vtnet0",
  "ip4_address": "192.168.122.101"
}
```

### DNS inside jails

The jail root is extracted fresh from `base.txz` on every provision, and that
archive does not ship a usable `/etc/resolv.conf`. `wsd` copies the host's
`/etc/resolv.conf` into each jail root automatically as part of provisioning,
so name resolution works inside workspaces (`pkg`, `npm`, `go mod download`,
...) without any manual step. With `ip4 = inherit` the jail shares the host's
network stack, so the host resolver config is the right one to use.

If you need a resolver other than the host's, bake a `resolv.conf` into a
custom base archive; the automatic copy will overwrite it on each provision.

### Base tooling and package manager

A jail populated from `base.txz` ships neither `pkg(8)` nor `git(1)`. Before
any project install command runs, `wsd` bootstraps the package manager and
installs a small baseline into every jail:

```sh
certctl rehash
env ASSUME_ALWAYS_YES=yes pkg bootstrap -y
pkg install -y git ca_root_nss
git config --system --add safe.directory '*'
```

`certctl rehash` builds the OpenSSL trust store (`/etc/ssl/cert.pem` and
`/etc/ssl/certs`) from the Mozilla certificates in `/usr/share/certs/trusted`.
A jail untarred from `base.txz` has the certificates but not the store, because
`rehash` normally runs during a real install; without it `pkg` fails HTTPS
certificate verification and cannot bootstrap. `git` is required by the
workspace's commit flow (`CommitAndPR` runs git inside the jail) and
`ca_root_nss` supplies the CA bundle `git`/`curl` need for HTTPS. This step
needs network access and runs while the workspace still has it; if it fails
(for example an unreachable mirror) it is logged and provisioning continues,
leaving the agent to install what it needs.

The `safe.directory` entry is needed because the workspace repo is a nullfs
mount of a host directory owned by the daemon's unprivileged user, while git
runs as root inside the jail; without it git refuses with "detected dubious
ownership".

### Ownership and escalation

`pkg(8)` refuses to run unless its database directory (`/var/db/pkg`) is owned
by UID/GID `0:0` (`root:wheel` on FreeBSD). When `wsd` runs as an
unprivileged user via an escalation command (`--escalation-command mdo`), the
base or template archive is extracted with `<escalation> tar -xpf` so the
entire filesystem retains its archived ownership and permissions. Without
an escalator, the daemon must run as root. Provisioning also ensures
`/var/db/pkg` exists and is owned by `0:0`, including for templates that omit it.

DNS writes and mount-point setup also use escalation, confined with `chroot`
to the jail root. The archive must contain the standard FreeBSD `/bin/sh`,
`mkdir`, and `chmod` utilities. Cache, configuration, and workspace host
directories must remain writable by the daemon account.

The extracted tree contains root-owned files, so cleanup is escalated too:
destroying a jail removes its root with
`<escalation> rm -rf <jail-root>` rather than a Go syscall, which the
unprivileged daemon could not perform.

Existing jails are not repaired automatically. Reprovision affected workspaces
to restore the archive's ownership; recursively assigning every file to root
would destroy intentional non-root ownership in the archive.

Every command run in a jail is executed with an explicit
`PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin:/usr/local/bin`. `jexec(8)`
does not start a login shell, so without this a daemon started from rc(8)
inherits its minimal `PATH`, and tools installed by `pkg` into `/usr/local/bin`
(such as `git`) are not found even though they are installed.

### Mounts

The generated jail config mounts:

- `devfs` at the jail's `/dev` (`mount.devfs`).
- The host's `/tmp` at the jail's `/tmp` (`nullfs`). This is **required**:
  `wsd` transfers the repo by writing a tar into the host's `/tmp`, which the
  jail then reads.
- The per-workspace host checkout (`<workspace-base-path>/<workspace-id>`)
  at the jail's `/workspace/repo`.
- An optional dependency cache at the jail's `/cache`, when a cache is in use.

---

## 5. Running wsd

Build and install:

```sh
cd /path/to/mcwsd
go build -o /usr/local/bin/mcwsd cmd/mcwsd/main.go
```

### Relevant flags

| Flag | Description | Default |
|------|-------------|---------|
| `--plugin` | Path to the FreeBSD jail plugin (e.g. `plugins/freebsd-jail.js`) | *(required)* |
| `--freebsd-release` | Release to fetch/cache and boot jails from | `15.1-RELEASE` |
| `--freebsd-arch` | Architecture used to build the base.txz download URL | `amd64` |
| `--freebsd-network-mode` | `shared` or `vnet` | `shared` |
| `--freebsd-interface` | Host interface; required for fixed shared IPs and VNET | *(empty)* |
| `--freebsd-ip4-address` | Shared: plain IPv4 or empty for inherit. VNET: IPv4 CIDR (optional, omit for DHCP) | *(empty)* |
| `--freebsd-vnet-gateway` | VNET default gateway (required only with static IP in VNET mode) | *(empty)* |
| `--ip-base-network` | Base network in CIDR for automatic IP allocation | *(empty)* |
| `--ip-pool-size` | Number of IPs to allocate from `--ip-base-network` | `0` |
| `--freebsd-dns-servers` | Comma-separated DNS server IPs; required for static VNET | *(empty)* |
| `--escalation-command` | Host escalator for jail(8)/jexec(8)/tar (`mdo`, `sudo`, `doas`, or empty) | `mdo` |
| `--workspace-base-path` | Host dir for per-workspace checkouts | `~/wsd-workspaces` |
| `--cache-dir` | Dependency cache directory | `~/wsd/cache` |
| `--state-file` | Workspace tracking file | `~/wsd/<plugin>.state.json` |
| `--bind-address` | HTTP bind address | `127.0.0.1` |
| `--port` | HTTP listen port | `8080` |

> **Use absolute paths.** `~` is **not** expanded. The default
> `~/wsd-workspaces` creates a literal `~` directory relative to the working
> directory. Always pass an absolute `-workspace-base-path`, for example
> `/usr/local/wsd/workspaces` or `/root/.wsd`.

### Example

DHCP, default release, no fixed IP:

```sh
MCWSD_TOKEN="$(openssl rand -hex 16)" \
mcwsd daemon --plugin plugins/freebsd-jail.js --freebsd-release 15.1-RELEASE \
 --workspace-base-path /home/wsd --cache-dir /var/lib/wsd/cache  \
 --state-file /var/lib/wsd/state.json --port 9080
```

The first provision downloads `base.txz` (~165 MB) once and reuses it for
subsequent workspaces of the same release.

### Run as an rc(8) service

Create `/usr/local/etc/rc.d/mcwsd`:

```sh
#!/bin/sh
#
# PROVIDE: mcwsd
# REQUIRE: NETWORKING
# KEYWORD: shutdown

. /etc/rc.subr

name="mcwsd"
rcvar="mcwsd_enable"

load_rc_config $name

: ${wsd_enable:="NO"}
: ${wsd_flags:="-plugin /usr/local/lib/mcwsd/plugins/freebsd-jail.js -freebsd-release 15.1-RELEASE -workspace-base-path /usr/local/wsd/workspaces -cache-dir /var/lib/wsd/cache -state-file /var/lib/wsd/state.json -port 9080"}

command="/usr/local/bin/mcwsd"
pidfile="/var/run/${name}.pid"

mcwsd_start()
{
    # Load secrets from a root-only env file, if present.
    if [ -f /usr/local/etc/mcwsd.env ]; then
        set -a
        . /usr/local/etc/mcwsd.env
        set +a
    fi
    echo "Starting ${name}."
    /usr/sbin/daemon -p ${pidfile} -f ${command} ${wsd_flags}
}

mcwsd_stop()
{
    if [ -f ${pidfile} ]; then
        kill "$(cat ${pidfile})"
    fi
}

start_cmd="mcwsd_start"
stop_cmd="mcwsd_stop"

run_rc_command "$1"
```

Then:

```sh
chmod 755 /usr/local/etc/rc.d/mcwsd
install -m 600 /dev/null /usr/local/etc/mcwsd.env
printf 'MCWSD_TOKEN=%s\n' "$(openssl rand -hex 16)" > /usr/local/etc/mcwsd.env
sysrc mcwsd_enable=YES
service mcwsd start
```

`mcwsd` runs in the foreground and writes logs to stderr; `daemon(8)` detaches
and captures them to syslog. The service must run as root.

---

## 6. Verifying and operating

After a workspace is provisioned:

```sh
jls                          # list running jails
jexec wsd-ws-xxxx sh         # open a shell in a jail
mount | grep /usr/local/jails/containers   # see jail mounts
```

Stopping/removing a jail by hand (normally `wsd` does this on destroy):

```sh
jail -r wsd-ws-xxxx
rm -f /etc/jail.conf.d/wsd-ws-xxxx.conf
umount -f /usr/local/jails/containers/wsd-ws-xxxx/dev
rm -rf /usr/local/jails/containers/wsd-ws-xxxx
```

Daemon housekeeping:

```sh
mcwsd cleanup --prune                  # destroy workspaces older than the TTL, then exit
mcwsd cleanup --orphans                # remove jails left by a previous crashed run, then exit
```

### Debug logging

2026-09-30T18:20:57-06:00 [DEBUG]

```sh
mcwsd daemon --plugin plugins/freebsd-jail.js ... 2>&1 | grep -v '\[DEBUG\]'
```

---

## 7. Troubleshooting

### Stale jail root: "does not look like a FreeBSD jail root"

A failed `jail -c` can leave `devfs`/`nullfs` mounts behind; `rm -rf` cannot
remove a mount point, so the directory survives. The current build unmounts
and rebuilds automatically, but to clean up manually:

```sh
mount | grep /usr/local/jails/containers/<jail-name>
umount -f /usr/local/jails/containers/<jail-name>/dev
umount -f /usr/local/jails/containers/<jail-name>/tmp
umount -f /usr/local/jails/containers/<jail-name>/workspace/repo
rm -rf /usr/local/jails/containers/<jail-name>
```

### "unknown parameter: allow.sysvshm"

Older builds emitted `allow.sysvshm`, which modern FreeBSD removed. Upgrade
`mcwsd`; the config now uses `allow.sysvipc`.

### "mount: /tmp /tmp: missing information"

`jail.conf` `mount` entries are `fstab(5)` lines and need the filesystem type
(`nullfs rw 0 0`). Upgrade `mcwsd`.

### "mount: /workspace: No such file or directory"

`jail.conf` mount targets are **host** paths under the jail root, not in-jail
paths. Upgrade `mcwsd`.

### "jexec: jail \"jexec\" not found"

The exec transport previously passed `jexec` twice. Upgrade `mcwsd`.

### ZFS datasets look like leftover mounts

`zroot/jails`, `zroot/jails/media`, and `zroot/jails/containers` are your
normal layout — do **not** `zfs destroy` them. Only `devfs`/`nullfs` entries
under `containers/<jail-name>/` are leftovers.

### A jail won't start

Read the command output in the logs. `wsd` logs the full command line before
running it and includes the command's stderr in the error. Useful checks:

```sh
jail -c -f /etc/jail.conf.d/<jail-name>.conf   # run manually
ifconfig <interface>                           # confirm the IP/subnet
mount | grep /usr/local/jails                  # confirm no stale mounts
```

---

## 8. What mcwsd does not do

- No NAT configuration. VNET mode delegates epair/bridge setup to the
  base-system `jib` helper; mcwsd does not create persistent host NAT rules.
- No `bsdinstall` (non-interactive provisioning only).
- No `jail_list` / `jail` rc(8) service management.
- No ZFS dataset creation, cloning, snapshots, or quotas.
- No `templates` dataset usage.

## Jail Host Configutation Tips

### /etc/jail.conf.d

```sh
chown -R mcwsd:mcwsd
```

### DEVFS RULES

Add the devfs rules `devfsrules_jail` to your `/etc/devfs.conf` file. Host only Jails don't need this but currently the code is not configured to handle that case.

```text
root@vnetbsd:/home/bear/Workspace/agentic # tail /etc/devfs.conf 
#own	speaker	root:operator
#perm	speaker	0660


[devfsrules_jail=11]
add include $devfsrules_hide_all
add include $devfsrules_unhide_basic
add include $devfsrules_unhide_login
add path 'bpf*' unhide
```

### JIB

The code is compiled to expect `jib` at `/usr/local/sbin/jib`.

```sh
install -o root -g wheel -m 0555 \
    /usr/share/examples/jails/jib \
    /usr/local/sbin/jib
```

### MDO

Setup MAC/DO for the `mcwsd` user ensuring your uid matches the user.

```sh
kldload mac_do
sysctl security.mac.do
id mcwsd
sysctl 'security.mac.do.rules=uid=1002>uid=0,gid=0,+gid=0,+gid=5'
```

### MAC/DO Rules

Load the `mac_do` module and set the rules to make permanent.

```text
echo 'mac_do_load="YES"' >> /boot/loader.conf
# OR
sysrc kld_list+="mac_do"

echo 'security.mac.do.rules=uid=1002>uid=0,gid=0,+gid=0,+gid=5' >> /etc/sysctl.conf
```

Verification: Run `sysctl security.mac.do.rules` after rebooting to confirm the rule is active.

### WSD Working Directories

We must create the working directories for the `mcwsd` user and ensure they have the correct permissions.

```sh
mkdir -p /var/lib/mcwsd
chown mcwsd:mcwsd /var/lib/mcwsd
chmod 700 /var/lib/mcwsd
```

Validate MAC/DO setup for the `mcwsd` user.

```sh
su - mcwsd
mdo -u root id
mdo -u root whoami
```

We use strict host key checking on Forges so ensure your known hosts file is up to date

```sh
su - mcwsd
ssh git@github.com   # replace github.com with your Git server
```

### RC File

For the maintained service script with settings for the daemon's runtime,
capacity, job, TLS, and lifecycle options, install
[`mcwsd`](./examples/freebsd/rc.d/mcwsd). The inline
script below is an older minimal example retained for reference.

```sh
#!/bin/sh
# This script is condensed from the maintained version in examples/freebsd/rc.d/mcwsd.

# PROVIDE: mcwsd
# REQUIRE: DAEMON NETWORKING
# BEFORE:  LOGIN
# KEYWORD: shutdown

#
# Add the following lines to /etc/rc.conf to enable mcwsd:
#
# mcwsd_enable="YES"
#

. /etc/rc.subr

name="mcwsd"
rcvar="mcwsd_enable"
desc="Workspace Daemon"

# Add /usr/local/bin and /usr/local/sbin
PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin:/usr/local/bin
export PATH

load_rc_config $name

# Defaults
: ${mcwsd_enable:="NO"}
: ${mcwsd_user:="mcwsd"}
: ${mcwsd_group:="mcwsd"}
: ${mcwsd_pidfile:="/var/run/${name}.pid"}
: ${mcwsd_port:="9080"}
: ${mcwsd_workspace_base_path:="/home/mcwsd"}
: ${mcwsd_cache_dir:="/home/mcwsd/cache"}
: ${mcwsd_state_file:="/home/mcwsd/state.json"}
: ${mcwsd_bind_address:="0.0.0.0"}
: ${mcwsd_runtime:="jail"}
: ${mcwsd_forge_type:="github"}
: ${mcwsd_git_auth_mode:="ssh"}
: ${mcwsd_debug:="YES"}
: ${mcwsd_freebsd_network_mode:="vnet"} 
: ${mcwsd_freebsd_interface:="em0"}


# Required secrets - set these in /etc/rc.conf (or better, in a
# protected file sourced by rc.conf) if not already in the environment:
# Remember to use the correct forge variables (i.e. GITLAB_TOKEN, GITHUB_TOKEN, etc.)
mcwsd_token="wsd-<token>"
mcwsd_github_token="github_pat_<token>"

pidfile="${mcwsd_pidfile}"
procname="/usr/local/bin/mcwsd"

command="/usr/sbin/daemon"
command_args="-f -p ${pidfile} -o /var/log/${name}.log /usr/local/bin/mcwsd daemon"
command_args="${command_args} --port ${mcwsd_port}"
command_args="${command_args} --workspace-base-path ${mcwsd_workspace_base_path}"
command_args="${command_args} --cache-dir ${mcwsd_cache_dir}"
command_args="${command_args} --plugin=${mcwsd_plugin}"
command_args="${command_args} --state-file ${mcwsd_state_file}"
command_args="${command_args} --forge-type=${mcwsd_forge_type}"
command_args="${command_args} --github-token-env-var=GITHUB_TOKEN"
command_args="${command_args} --git-auth-mode=${mcwsd_git_auth_mode}"
command_args="${command_args} --git-ssh-key-path-env-var=SSH_KEY_PATH"
command_args="${command_args} --bind-address=${mcwsd_bind_address}"
command_args="${command_args} --freebsd-network-mode=${mcwsd_freebsd_network_mode}"
command_args="${command_args} --freebsd-interface=${mcwsd_freebsd_interface}"

if checkyesno mcwsd_debug; then
    command_args="${command_args} --debug"
fi

start_precmd="mcwsd_prestart"
stop_postcmd="mcwsd_poststop"

mcwsd_prestart()
{
    # Export environment variables consumed by mcwsd.
    if [ -n "${mcwsd_token}" ]; then
        MCWSD_TOKEN="${mcwsd_token}"
        export MCWSD_TOKEN
    fi
    if [ -n "${mcwsd_github_token}" ]; then
        GITHUB_TOKEN="${mcwsd_github_token}"
        export GITHUB_TOKEN
    fi
    MCWSD_GIT_SSH_KNOWN_HOSTS="${mcwsd_known_hosts}"
    SSH_KEY_PATH="${mcwsd_ssh_key_path}"
    export MCWSD_GIT_SSH_KNOWN_HOSTS SSH_KEY_PATH

    # Make sure the runtime directories exist.
    for d in "${mcwsd_workspace_base_path}" "${mcwsd_cache_dir}" \
             "$(dirname "${mcwsd_state_file}")" /var/log; do
        [ -d "$d" ] || mkdir -p "$d" || chown -R mcwsd:mcwsd
    done

    # Make sure the runtime files exist.
    for f in "/var/log/mcwsd.log" "/var/run/mcwsd.pid" 
    do
        [ -f "$f" ] || touch "$f" && chown ${mcwsd_user}:${mcwsd_group} $f
    done
}

mcwsd_poststop()
{
    if [ -f "${pidfile}" ]; then
        rm -f "${pidfile}"
    fi
}

run_rc_command "$1"
```

Install the maintained script from the repository root:

```sh
install -m 0755 examples/freebsd/rc.d/mcwsd /usr/local/etc/rc.d/mcwsd
```

Control the service with the following commands:

```sh
service mcwsd start
service mcwsd stop
service mcwsd restart
```

### Privelege escalation

You can change from `mdo` to `sudo` or any other escalation command by changing the `mcwsd_freebsd_escalation` variable in `/etc/rc.conf`. You will need the following commands allowed in your escalation configuration:

- jexec
- jls
- tar
- mkdir
- chown
- chroot

The FreeBSD host can be configured to ship syslog entries to a remote syslog server or to a local file. The `mcwsd` daemon logs to stderr, which is captured by the `daemon(8)` wrapper and sent to syslog. 

### BIGGEST TIP EVER!

To make things easy, the Workspace Daemon follows the [FreeBSD Jail Guide for 15.1](https://docs.freebsd.org/en/books/handbook/jails/) as closely as possible so follow the steps to build out the environment accordingly.
