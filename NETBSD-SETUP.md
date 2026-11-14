# NetBSD Account Setup for mcwsd

This document describes how to configure a NetBSD host so that `mcwsd` can
provision workspaces as local user **accounts** on the host
(`--plugin plugins/netbsd-account.js`). Unlike jails, zones, or VMs, this
runtime provides **process isolation only** — a workspace is an ordinary
unprivileged account, nothing more. Read the Security briefing in §6 before
relying on it.

NetBSD has no jails and ships no privilege escalator, but a non-interactive
daemon cannot use `su`. This runtime therefore requires an escalator from
pkgsrc: **sudo** (recommended) or **doas**.

---

## 1. How mcwsd uses the host

1. **Provision.** For each workspace it creates a group and an account named
   after a random **instance id** core assigns to the runtime, *not* the public
   workspace id (`ac-XXXXXXXXXXXX`, 15 characters), e.g. `groupadd
   ac-abc123456789` then `useradd -d /home/ac-abc123456789 -m -M 700 -g
   ac-abc123456789 -G wsd -s /sbin/nologin ac-abc123456789`. The account's home
   directory *is* the workspace: core's fixed in-runtime path `/workspace` is
   remapped onto it, so the repo lives at `/home/ac-abc123456789/repo` and is
   owned by the account.
2. **Exec.** Every command — user commands and core's own file I/O
   (copy-in, read, write) — runs as the account via
   `sudo -u ac-abc123456789 sh -c 'cd <home>/repo && …'`. The account never gets
   its own escalation.
3. **Destroy.** `userdel -r ac-abc123456789` and `groupdel ac-abc123456789`; the
   home (and any workspace tree beneath it) is removed.

Account names are opaque instance ids (15 chars, at NetBSD's 16-character
`LOGIN_NAME_MAX` limit). Because they are not derived from the public workspace
id, listing `/home` or `/etc/passwd` does not reveal other workspaces' API ids.
New accounts match `^ac-[0-9a-f]{12}$`; legacy accounts from releases that
named accounts after the workspace id match `^ws-[0-9a-f]{9,12}$`. Orphan
cleanup only touches these shapes, so unrelated accounts are never touched.

### Paths

| Path | Purpose |
|------|---------|
| `/home/ac-XXXXXXXXXXXX/` | Workspace root (the account's home, mode 0700) |
| `/home/ac-XXXXXXXXXXXX/repo/` | The workspace repository (`/workspace/repo` remapped) |

---

## 2. Requirements

- NetBSD (tested on 11.0).
- pkgsrc `sudo` (recommended) or `doas` — neither is in base.
- The account-management tools are in base (`/usr/sbin/useradd`, `userdel`,
  `groupadd`, `groupdel`, `userinfo`, `chown`).
- A shared group that workspace accounts join (default `wsd`).

---

## 3. Setup

Run these as root.

### 3.1 Install an escalator

```sh
PKG_PATH="https://cdn.netbsd.org/pub/pkgsrc/packages/NetBSD/$(uname -m)/$(uname -r)/All"
export PKG_PATH
pkg_add sudo        # or: pkg_add doas
```

> `sudo` is the recommended default because a single static sudoers rule can
> authorize run-as-any-workspace-account by group; see below.

### 3.2 Create the daemon account and shared group

```sh
useradd -m -s /bin/sh _wsd
groupadd wsd
```

`_wsd` runs the daemon. It is deliberately non-root; it reaches root only for
the account-management commands listed in the sudoers rules below.

### 3.3 Configure sudo (recommended)

Edit `/usr/pkg/etc/sudoers` with `visudo` (note the pkgsrc path):

```text
# Up: let the daemon manage accounts only.
_wsd ALL=(root) NOPASSWD: /usr/sbin/useradd, /usr/sbin/userdel, /usr/sbin/groupadd, /usr/sbin/groupdel, /usr/sbin/userinfo, /usr/sbin/chown, /bin/rm, /bin/mkdir, /bin/test

# Down: let the daemon run commands as any workspace account (members of wsd).
_wsd ALL=(%wsd) NOPASSWD: ALL
```

The second rule is what makes `sudo -u ac-XXXXXXXXXXXX …` work for a *dynamic*
set of accounts without editing sudoers per workspace. Because it is scoped to
`%wsd`, the daemon can only become workspace accounts, not root or other host
users.

`sudo` lives at `/usr/pkg/bin/sudo`, so the daemon's `PATH` must include
`/usr/pkg/bin` (see §4).

### 3.4 Configure doas (alternative)

`doas` is a pkgsrc port (`security/doas`) and takes the same `-u` shape. The
catch is authorizing run-as-dynamic-accounts: OpenBSD's doas.conf has no
group run-as, so you would either add a `permit nopass _wsd as ac-XXXXXXXXXXXX
cmd sh` line per workspace (fragile) or grant `permit nopass _wsd as root cmd su`
and drop with `su -m`, which **does not apply login-class resource limits**.
Prefer sudo unless you have a reason to accept those tradeoffs.

### 3.5 Define a login class (optional)

Stock NetBSD ships `/etc/login.conf` fully commented out — there is no `daemon`
class to reuse. Define one, e.g.:

```text
wsd:\
    :datasize-cur=4G:\
    :datasize-max=4G:\
    :maxproc-cur=128:\
    :maxproc-max=256:\
    :openfiles-cur=256:\
    :openfiles-max=512:\
    :stacksize-cur=16M:
```

Then pass `--account-login-class wsd` to the daemon. Two caveats, both yours to
verify:

1. The class only takes effect if the escalator actually applies
   `setusercontext(3)` (`sudo` does when configured to; plain `su -m` does
   not).
2. Login classes bound *resource limits* (fork bombs, memory, cpu). They give
   **no** filesystem, network, or uid isolation. In testing, neither `sudo` nor
   `doas` applied the class settings on NetBSD.  They are called `login classes` after all
   and were historically applied at login.
3. You can apply quotas for all workspace accounts with `quotaon -a` and `edquota`, which apply limits to all users for each quota-enabled filesystem.
4. You can apply `sysctl -w security.curtain=1` to enable the "curtain" security feature, which helps prevent information leaks between processes.

---

## 4. Pre-install tooling

This runtime has no base image and does not bootstrap a package manager, so the
workspace account can only use what is already installed on the host. Install
the toolchains agents need once, as root:

```sh
pkg_add git python3 go rust # your preferred tools but git is required for forge usage
```

The agent's system package-manager is reported as `pkg_add`, but as an
unprivileged account it cannot actually run it (install failures are non-fatal;
see §6).

---

## 5. Run the daemon

```sh
# sudo (and pkg tools) live under /usr/pkg
export PATH="/usr/pkg/bin:/usr/pkg/sbin:$PATH"

mcwsd daemon \
  --plugin plugins/netbsd-account.js \
  --port 9080 \
  --bind-address=0.0.0.0 \
  --state-file ~/wsd/netbsd/state.json \
  --escalation-command sudo \
  --account-login-class wsd \
  --debug
```

The `--account-*` and `--escalation-command` flags are declared by the plugin
and appear in `mcwsd daemon --plugin plugins/netbsd-account.js --help`.

---

## 6. Security briefing

This runtime is intentionally the weakest isolation model mcwsd offers. Treat
it as a convenience for a single trusted tenant, not a boundary:

- **No filesystem isolation.** `/etc/passwd`, `/tmp`, and any world-readable
  file are visible. Workspace accounts are separated only by uid + home 0700.
- **Workspace IDs are not enumerable.** Accounts are named after random
  instance ids, not public workspace ids, so listing `/home` or `/etc/passwd`
  does not hand an agent another workspace's API id. This is defense in depth:
  it does not change the fact that any holder of the daemon's shared bearer
  token can act on any workspace id it already knows.
- **No network isolation.** Workspaces share the host network and can reach
  localhost services; `setNetwork` is a no-op (always on).
- **No escalation by default.** Workspace accounts have no sudo/doas. They can
  only run as themselves, which is what keeps one workspace from reading
  another's files.
- **"Package-only" escalation is not a boundary.** If you choose to grant a
  workspace account sudo/doas for `pkg_add`, be aware `pkg_add` runs
  `+INSTALL`/`+DEINSTALL` scripts as root — arbitrary code execution — so that
  grant is effectively root. The safe default is to pre-install everything and
  grant nothing.
- **Shared `/tmp`.** The commit bundle uses `/tmp/wsd-out.bundle`; concurrent
  workspaces can in principle race on it, though the window is tiny.

---

## 7. Troubleshooting

- **`escalator … cannot reach root (test -r /etc/master.passwd)`** — the
  sudoers/doas rule isn't allowing `_wsd` to run `test` (or the target command)
  as root. `/etc/master.passwd` is root-only, so this check genuinely proves
  root access.
- **`shared group wsd does not exist`** — run `groupadd wsd` (or set
  `--account-group` to a group you created).
- **`login class … is not defined`** — add the class to `/etc/login.conf` (and
  note it must be applied by `setusercontext`, see §3.5).
- **`useradd: user … exists`** on re-provision — a leftover from a crashed
  provision. `mcwsd daemon --cleanup-orphans` (or manual `userdel -r` +
  `groupdel`) clears it.
