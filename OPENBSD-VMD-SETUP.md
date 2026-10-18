# OpenBSD vmd(8) VM Setup for mcwsd

This document describes how to configure an OpenBSD host so that `mcwsd` can
provision workspaces as full virtual machines under vmd(8)
(`-runtime vmd`). It covers the golden image, the network model, privilege
escalation, running the daemon, and troubleshooting.

If you want the design/implementation notes, see [VMD.md](VMD.md).


## David's Notes

1. The document should emphasize creating a non-root user `_wsd` to run the daemon.
2. The document should also emphasize ensuring that all directories and files that the `_wsd` user will access are properly owned and have the correct permissions.
3. The document should also mention that the `_wsd` user needs to have the necessary privileges to run `vmctl` and other required commands,  by configuring an escalation command like `doas` or `sudo`.
4. Master image:  I make mine without the default OpenBSD partitioning scheme.  I use just a single partition `a` with no swap.  Swap is up to the admin.

```text
permit nopass _wsd as root cmd vmctl
permit nopass _wsd as root cmd rm
```

5. Include telling the user to add the `--debug` flag when running the `mcwsd` daemon for troubleshooting purposes even though there is little debugging info in anything but the provisioning process.

---

## 1. How mcwsd uses the host

A VM's filesystem is locked inside a disk image, so `mcwsd` never writes into it
directly. Instead it treats the VM as a black box that exposes SSH:

1. **Clone.** For every workspace, `wsd` creates a **copy-on-write** clone of
   the administrator-built golden image with
   `vmctl create -b disk-master.qcow2 <vm-root>/wsd-<workspace>/disk.qcow2`.
   `-b` (not `-i`) makes this near-instant regardless of the master's size.
2. **Start.** It runs
   `vmctl start -L -B disk -d <disk> -m <memory> wsd-<workspace>`.
   The `-L` flag attaches a **local interface**, so vmd(8) runs DHCP and
   assigns a predictable address derived from the VM ID (see §5).
3. **Wait.** It polls the host ARP table, then TCP port 22, then runs a real
   SSH command, until the guest is reachable (bounded by
   `-openbsd-boot-timeout`, default 5m).
4. **Return a handle.** Commands and file transfers run over SSH
   (`ssh -i <key> user@100.64.<id>.3`).
5. **Destroy.** `vmctl stop -f wsd-<workspace>` and remove the disk directory.
   `-f` is deliberate: the workspace is disposable, so there is nothing worth a
   graceful shutdown.

VM/disk names are `wsd-<workspace-id>` (for example `wsd-ws-71670485b`).

### Paths

| Path | Purpose |
|------|---------|
| `<vm-root>/` | Base directory for all VM disks (default `/var/wsd/vms/openbsd`) |
| `<vm-root>/disk-master.qcow2` | Golden image, cloned per workspace (admin-built) |
| `<vm-root>/wsd-<workspace>/disk.qcow2` | Per-workspace copy-on-write clone |

The master image must be treated as **immutable** once clones exist: every
clone stays dependent on it, so modifying or moving it breaks all workspaces.
**Never boot the master image itself** (`vmctl start -d disk-master.qcow2 ...`):
a running master writes to the backing file that every clone depends on, which
can corrupt clones or capture an inconsistent base. If you need to update the
golden image, build a new file and version it (`disk-master-v2.qcow2`) rather
than editing the old one in place.

---

## 2. Requirements

- OpenBSD with vmd(8) available (`vmm(4)`, `vmd(8)`, `vmctl(8)`).
- vmd(8) enabled and running:
  ```sh
  doas rcctl enable vmd
  doas rcctl start vmd
  doas vmctl status
  ```
- `vmctl(8)`, `ssh(1)`, `tar(1)`, `arp(8)` in `PATH`.
- Either the daemon runs as root, or an escalation command (`doas` preferred,
  `sudo` also supported) is configured and permitted for `vmctl`.
- A golden image and an SSH key pair (§3, §4).

`mcwsd` validates all of this at startup (`vmctl` present, master image
readable and non-empty, VM root writable, `vmctl status` reachable) and
refuses to start if anything is wrong, instead of failing on the first
provision.

---

## Notes::

We need to tell user to create account `_wsd` before running the daemon.
```sh
doas useradd -m _wsd
```
For 3, we need to mkdir the paths for the golden image and the VM root before building the image.
```sh
doas mkdir -p /var/wsd/vms/openbsd
```

## 3. Building the golden image

The golden image is an **administrative artifact**, not something `mcwsd`
builds. It must already contain everything a workspace needs to start working
immediately after boot:

- the login account `wsd` SSHes in as (root, or a dedicated user);
- the daemon's **public** SSH key in that account's
  `~/.ssh/authorized_keys`;
- `sshd(8)` enabled and listening (default port 22);
- **git** (required by the workspace commit flow);
- the tools your users' projects need (TypeScript/node, Python, Go, compilers,
  ...). `wsd` does **not** verify or install these — a missing tool just wastes
  the user's time later.

A practical way to build it:

1. Create a reference disk and install OpenBSD into a VM (or install to a
   physical disk and import it):
```sh
   doas vmctl create -s 20G /var/wsd/vms/openbsd/disk-master.qcow2
   
  # ftp https://cdn.openbsd.org/pub/OpenBSD/7.9/amd64/install79.iso
  vmctl create  /var/wsd/vms/openbsd/disk-master.qcow2
  vmctl create -i /var/wsd/vms/openbsd/disk-master.qcow2 /var/wsd/vms/openbsd/wsd-master.qcow2
```

From `man vmctl` section `LOCAL INTERFACES`:

Add the following rules to your host machine's `/etc/pf.conf`:
Set your `$dns_server` variable to the IP address of the DNS server you want to use for the guest VMs.  I used `192.168.122.1` which is the default DNS server for my virtual network.

```text
  pass out on egress from 100.64.0.0/10 to any nat-to (egress)

  pass in proto { udp tcp } from 100.64.0.0/10 to any port domain \
        rdr-to $dns_server port domain
```

```
Ensure that IP forwarding is enabled by running:
```sh
sysctl net.inet.ip.forwarding=1
```

Make it permanent by adding the following line to `/etc/sysctl.conf`:
```
net.inet.ip.forwarding=1
```


And run `pfctl -f /etc/pf.conf` to apply the new rules.

```sh
   vmctl start -cL  -m2G -r /var/wsd/vms/openbsd/install79.iso -B cdrom -d  /var/wsd/vms/openbsd/wsd-master.qcow2 wsd.master


  # Install OpenBSD into the VM using the installer image.  I recommend following the standard OpenBSD installation procedure except make the whole volume just partition `a` which gives maximum space for packages and the workspace.  Dont forget to ensure the console is serial console as VMD only supports serial console.



```

2. Boot the reference VM, install OpenBSD, create the login user, install the packages, and add the public key:

Host side skip if you shutdown instead of rebooting:

```sh
vmctl start -cL  -m2G  -d  /var/wsd/vms/openbsd/wsd-master.qcow2 wsd.master 
```

Guest side:

```sh
# inside the guest, run syspatch and reboot if needed
  syspatch
  # inside the guest, install your preferred packages noting that git is needed for the workspace to use forge mode.
  pkg_add git node python3
  adduser wsd
 su - wsd
   # paste the contents of the host's /var/wsd/agent_ed25519.pub
   # into the wsd user's `~/.ssh/authorized_keys` file.
  exit
 cp /etc/examples/doas.conf /etc/doas.conf
  # Edit /etc/doas.conf to allow the wsd user to run commands as root
  # For example, add a line like:
  permit nopass wsd
  # test it
  su - wsd
  # You should now be able to run commands as root using doas
  doas id 
  # should report root
  # It is a good idea to check networking
  ping -c 5 1.1.1.1
  ping -c 5 www.google.com
  # Should have network ping to IP and name resolution pinging the domain
```

Host side:

```sh
# Verify SSH Key is good
vmctl show
# Replace X with vmid number 
ssh -i /var/wsd/agent_ed25519 wsd@100.64.X.3
```

3. Shut the guest down **cleanly**, then copy its disk over the master path
   (or build the master first and keep the file you used).

Host side:

Compile and install the daemon.

```sh
pkg_add go git
git clone git@github.com:davidcrumpton/agentic.git
cd agentic
go build -o wsd cmd/mcwsd/main.go 
cp wsd /usr/local/bin/wsd
mkdir /var/wsd/cache
chown _wsd:_wsd /var/wsd/cache
```


---

## 4. Privilege model

Two separate privilege decisions exist:

**Host side (starting/stopping VMs).** `vmctl start`/`stop` need root. Never
run `wsd` as root, but do run it as an unprivileged user `_wsd` and grant a
narrow `doas` rule. For example, in `/etc/doas.conf`:

```text
permit nopass keepenv _wsd as root cmd rm
permit nopass keepenv _wsd as root cmd vmctl
```

Then start the daemon with `-openbsd-escalation-command doas`.

**Guest side (commands the agent runs).** This is configured separately with
`-openbsd-guest-escalation-command`, so the host and guest may use different
escalators (for example `sudo` on the host and `doas` in the guest). If
`-openbsd-login-user` is `root`, no guest escalation is needed (and any guest
escalation setting is ignored). If it is a dedicated user such as `wsd`, the
transport wraps every command as `doas sh -c '<command>'`, so the **guest's own**
`/etc/doas.conf` must permit it — either broadly (full root) or narrowly (for
example only `pkg_add`). This is a *different* `doas.conf` from the host's in
the section above. A typical guest rule granting full root is:

The guest side account isn't a daemon and isn't prefixed with an underscore like `_wsd`.

```text
permit nopass wsd
```

or narrowly, for package installation only:

```text
permit nopass wsd as root cmd pkg_add
```

Narrow permissions means you should update the `provisioning_hints.json` file (§7) to reflect the limited capabilities of the guest account.  You can tell the AI to read `/etc/doas.conf` or execute `sudo -l` to understand what the guest account is allowed to do.  It is great to have AI install needed packages but some things like Postgres require broader permissions so plan well if you lockdown your guest.

If the guest does not permit it, provisioning fails fast during the readiness
probe with `guest login/escalation is misconfigured: doas: Operation not
permitted` rather than hanging.

This is a deliberate trust decision: a full-root rule means the agent
effectively has root inside the VM. Because the VM is isolated, that is usually
acceptable, but the `provisioning_hints.json` file (§7) is how you tell the
agent what it may do.

Using a dedicated user rather than root is recommended: it protects against
catastrophic agent bugs, keeps tools that refuse to run as root working, and
leaves an auditable `doas`/`sudo` trail.

---

## 5. Networking

`wsd` uses vmd(8) **local interfaces** (`-L`). You do not configure guest
addressing, and there is no config drive or cloud-init involved. For the first
local interface, vmd(8) assigns:

| Interface | Address |
|-----------|---------|
| host `tapN` | `100.64.<vm-id>.2` |
| guest `vio0` | `100.64.<vm-id>.3` |

`<vm-id>` is the numeric ID shown by `vmctl status` and printed by
`vmctl start` (`vmctl: started vm 6 successfully`). `wsd` parses it and derives
the guest address automatically. Later local interfaces would be `.4`/`.5`,
`.6`/`.7`, and so on, but `wsd` uses only the first.

Both addresses appear in the host's `arp -a` table on the same `tapN`
interface, which can look like two hosts. They are not: `100.64.<id>.2` is the
OpenBSD **host's own** end of the tap, so SSHing to it reaches the host's
`sshd` (and will report `invalid user` for the guest account). Only
`100.64.<id>.3` is the VM. `wsd` always uses `.3`.

Ensure vmd(8) has a local interface configured (the default `vether0`/`veb0`
setup is sufficient) and that the host is not firewalling the `100.64.0.0/10`
range away from the daemon.

VMs are **always networked** while they run; `wsd`'s `SetNetwork` is a no-op
for VMs (the hypervisor provides the isolation, and vmd(8) has no cheap per-VM
network toggle). Cut network access with `pf(4)` on the host if you need it which requires modifying the wsd daemon source code as it doesn't offer custom hook execution..

---

## 6. Running the daemon

You may with to `su -m _wsd -c sh` to switch to the `_wsd` user before running the daemon.  NOTE:  I need to try `doas -u _wsd` which is cleaner for OpenBSD.


```sh
su -m _wsd -c 'mcwsd daemon \
  --runtime vmd \
  --openbsd-vm-root /var/wsd/vms/openbsd \
  --openbsd-master-image /var/wsd/vms/openbsd/wsd-master.qcow2 \
  --openbsd-memory 1G \
  --openbsd-login-user wsd \
  --openbsd-ssh-key /var/wsd/agent_ed25519 \
  --openbsd-ssh-port 22 \
  --openbsd-guest-os openbsd \
  --openbsd-escalation-command doas \
  --openbsd-guest-escalation-command doas \
  --openbsd-boot-timeout 7m \
  --cache-dir /var/wsd/cache \
  --state-file /var/wsd/state.json'

```

| Flag | Default | Description |
|------|---------|-------------|
| `-runtime vmd` | `docker` | Selects the vmd(8) provisioner (`openbsd` is an alias) |
| `-openbsd-vm-root` | `/var/wsd/vms/openbsd` | Base directory for VM disks |
| `-openbsd-master-image` | `<vm-root>/disk-master.qcow2` | Golden image |
| `-openbsd-memory` | `1G` | `vmctl -m` value per VM |
| `-openbsd-login-user` | `root` | Guest SSH account |
| `-openbsd-ssh-key` | `/var/wsd/agent_ed25519` | Private key for the guest |
| `-openbsd-ssh-port` | `22` | Guest SSH port |
| `-openbsd-guest-os` | `openbsd` | `openbsd`, `freebsd`, or `linux` |
| `-openbsd-escalation-command` | `doas` | Host escalation for `vmctl` (empty = run directly) |
| `-openbsd-guest-escalation-command` | `doas` | Guest escalation for commands run inside the VM (empty = run directly; ignored when the login user is root) |
| `-openbsd-boot-timeout` | `5m` | Total time to wait for SSH readiness (ARP → TCP → SSH) |
| `-openbsd-ssh-probe-timeout` | `20s` | Bound on a single SSH readiness probe |
| `-provisioning-hints-file` | `/etc/wsd/provisioning_hints.json` | Agent environment hints (optional) |

The `-openbsd-*` flags are ignored by the Docker, Podman, and jail runtimes but the app doesn't error to hint you left unusable options in.

---

## 7. Provisioning hints

`wsd` appends the contents of `-provisioning-hints-file` to every workspace's
profile notes, so the agent learns facts it cannot detect on its own: how to
escalate privileges, which tools are preinstalled, and any local conventions.
The file is optional; if it is missing there are no hints. It must be valid
JSON. Example:

```json
{
  "notes": "This OpenBSD VM has git, node, python3 and go preinstalled. Privilege escalation is available via `doas` with full root (permit nopass wsd). Use `pkg_add` to install additional packages. Network is always available."
}
```

This is a daemon-level feature, not vmd-specific: it is equally useful for
jails and containers.

---

## 8. Cleanup and orphans

Because cloning a disk and starting a VM are separate steps, a crash in
between can leave a disk with no VM and no state entry. Run:
You must specify directory options if you don't use the default paths.


```sh
doas -u _wsd -runtime vmd -cleanup-orphans
```

This removes tracked leftovers from a previous run **and** sweeps any
`wsd-*` disk directory under `-openbsd-vm-root` that no live workspace tracks.
It only touches `wsd-*` directories containing a `disk.qcow2`, so the master
image and any unrelated content are never removed.

Normal teardown (`DELETE /workspaces/{id}`), TTL pruning, and the startup
prune all use `vmctl stop -f` plus disk removal.

---

## 9. Troubleshooting

The provisioner logs extensively with a `[DEBUG]` prefix. To watch the exact
`vmctl`/`ssh`/`arp` commands it runs:

```sh
wsd ... 2>&1 | grep '\[DEBUG\]'
```

Common problems:

- **`vmctl status failed`** at startup: vmd(8) is not running
  (`doas rcctl start vmd`), or escalation is not configured/permitted. Note
  that `vmctl status` itself exits non-zero when there are **no** VMs — that is
  normal and `wsd` accepts it as long as vmd answered (its table header is
  printed); only a real connection/escalation failure is fatal.
- **`master image ... is not readable`/`is empty`**: wrong
  `-openbsd-master-image`, or the golden image was never built.
- **Provision times out waiting for ARP/TCP/SSH**: check `vmctl status`, that
  the master image has `sshd` enabled and the public key installed, that
  `-openbsd-login-user` matches the account holding the key, and that
  `100.64.0.0/10` is reachable from the daemon.
- **SSH connects but commands fail**: the login user's `doas`/`sudo` policy
  does not permit the wrapped command; see §4.
- **`provision_failed ... context canceled`**: this is the *client* (the agent
  tool / HTTP request) giving up, not `wsd`'s boot timeout. VM boot plus
  provisioning can exceed a short client timeout. Provisioning can run as a
  background job (`POST /workspaces` with `async: true` or `wait_seconds`),
  which detaches it from the request entirely, so the client polls the job
  instead of holding the connection open. Increase `-openbsd-boot-timeout`
  only if `wsd` itself is timing out (look for `timed out waiting for ...` in
  the log).
- **Clones break after a master change**: the master was modified or moved.
  Treat it as immutable and version a new one.

## 10. OpenBSD Config File Examples

Create the login class file to set the environment for the `wsd` user.

### /etc/login.conf.d/wsd

```text
wsd:\
    :setenv=MCWSD_TOKEN=wsd-XXXXXXXXXXXXXXX:\
    :tc=daemon:
```

### Daemon user _wsd class

Add the run as user `_wsd` to the `wsd` login class.

```sh
doas usermod -L wsd _wsd
doas cap_mkdb /etc/login.conf
```

### RC Configuration /etc/rc.d/wsd

Create the RC configuration file for the `wsd` daemon.

```sh
# Fixup:  Avoid /var
#!/bin/ksh
name="wsd"
daemon="/usr/local/bin/wsd"
daemon_user="_wsd"
daemon_logger="daemon.info"
daemon_flags="daemon \
    --runtime vmd \
    --openbsd-vm-root /var/wsd/vms/openbsd \
    --openbsd-master-image /var/wsd/vms/openbsd/wsd-master.qcow2 \
    --openbsd-memory 1G \
    --openbsd-login-user wsd \
    --openbsd-ssh-key /var/wsd/agent_ed25519 \
    --openbsd-ssh-port 22 \
    --openbsd-guest-os openbsd \
    --openbsd-escalation-command doas \
    --openbsd-guest-escalation-command doas \
    --openbsd-boot-timeout 7m \
    --cache-dir /var/wsd/cache \
    --state-file /var/wsd/state.json"

. /etc/rc.d/rc.subr

rc_bg=YES
rc_cmd $1
```

Enable and start the daemon.

```sh
rcctl enable wsd
rcctl start wsd
```


## 11. Auditing

The privelege executor logs the daemon's actions, including provisioning and command execution, for auditing purposes.  No need for the terse `--debug` output!

```text
$ tail -f /var/log/secure
Oct  1 22:54:20 OpenWSD doas: _wsd ran command vmctl status as root from /home/_wsd
Oct  1 22:54:44 OpenWSD doas: _wsd ran command vmctl create -b /var/wsd/vms/openbsd/wsd-master.qcow2 /var/wsd/vms/openbsd/wsd-ws-77455b88e/disk.qcow2 as root from /home/_wsd
Oct  1 22:54:44 OpenWSD doas: _wsd ran command vmctl start -L -B disk -d /var/wsd/vms/openbsd/wsd-ws-77455b88e/disk.qcow2 -m 1G wsd-ws-77455b88e as root from /home/_wsd
Oct  1 22:58:00 OpenWSD doas: _wsd ran command vmctl create -b /var/wsd/vms/openbsd/wsd-master.qcow2 /var/wsd/vms/openbsd/wsd-ws-ee26fc01d/disk.qcow2 as root from /home/_wsd
Oct  1 22:58:00 OpenWSD doas: _wsd ran command vmctl start -L -B disk -d /var/wsd/vms/openbsd/wsd-ws-ee26fc01d/disk.qcow2 -m 1G wsd-ws-ee26fc01d as root from /home/_wsd
```
### 12. Privelege escalation

You can change your host provisioning escalation command from `doas` to `sudo` or any other escalation command with `-openbsd-escalation-command`. You will need the following commands allowed in your host escalation configuration:

- vmctl
- rm

To set the escalation command used inside the guest VM, use `-openbsd-guest-escalation-command`. It is independent of the host escalator, so the two can differ. The guest escalation command is expected to have root = ALL permissions and is used to run commands inside the guest VM. You can configure OpenBSD syslog to remote log events and capture the commands run by the AI.

### OpenBSD VMD Setup Biggest TIP Ever!

To make things easy, the Workspace Daemon follow's the `LOCAL INTERFACES` section of `man vmctl` as closely as possible so follow the steps to build out the environment accordingly.
