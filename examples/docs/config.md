# Configuration Reference

This document is a practical reference for the `mcwsd` daemon configuration flags. It is intended to complement the built-in CLI help and to make it easier to reason about how the daemon is configured for a specific host or deployment.

You can always see the latest built-in CLI usage with:

```sh
mcwsd daemon --help
```

or:

```sh
mcwsd --help
```

## Overview

The daemon is configured primarily via command-line flags passed to `mcwsd daemon`. The most important settings are grouped into:

- Provisioner settings: runtime selection and platform-specific tuning
- Backend settings: network binding, workspace state, cache, pruning, and capacity limits
- Forge / git auth settings: commit-and-pr support and push authentication
- Operational settings: debug logging and audit logging

The flags are evaluated at daemon startup. Secret values are intentionally read from environment variables rather than passed directly as CLI flags whenever possible.

## Basic usage

```sh
mcwsd daemon \
  --plugin plugins/freebsd-jail.js \
  --bind-address 0.0.0.0 \
  --port 8080 \
  --workspace-base-path /var/wsd/workspaces \
  --state-file /var/wsd/state.json \
  --jobs-file /var/wsd/jobs.json
```

A minimal server can also be started with defaults plus a plugin:

```sh
mcwsd daemon --plugin plugins/docker.js
```

## Global and operational flags

These flags apply to the process at large.

### `--debug`

- Type: bool
- Default: `false`
- Enables verbose DEBUG logging for the daemon.

### `--audit`

- Type: bool
- Default: `false`
- Enables structured JSON audit logging for the daemon process.

### `--audit-file`

- Type: string
- Default: `~/audit.jsonl`
- Output path for the audit log when audit logging is enabled.

## Provisioner flags

These options control which runtime backs new workspaces and how it is configured.

### `--plugin`

- Type: string
- Default: empty (required)
- Path to a compiled runtime plugin (e.g. `plugins/podman.js`, `plugins/docker.js`,
  `plugins/kubernetes.js`, `plugins/freebsd-jail.js`, `plugins/openbsd-vmd.js`,
  or `plugins/zones.js`).
- Every runtime is a plugin; the daemon loads exactly one via `--plugin` and
  registers the plugin's own options as `--help` flags.

### FreeBSD jail flags

The following apply to the jail runtime only.

#### `--freebsd-release`

- Type: string
- Default: `15.1-RELEASE`
- FreeBSD release name, or archive filename when a custom template is configured.

#### `--base-template-url`

- Type: string
- Default: empty
- HTTP(S) directory URL containing custom jail archives.

#### `--base-template-user`

- Type: string
- Default: empty
- Username for HTTP Basic auth when downloading a template archive.

#### `--base-template-password-env`

- Type: string
- Default: empty
- Environment variable name that holds the password for template downloads.

#### `--base-template-header`

- Type: string
- Default: empty
- Authentication header name, such as `Authorization`.

#### `--base-template-header-env`

- Type: string
- Default: empty
- Environment variable name holding the entire authentication header value.

#### `--verify-signature`

- Type: bool
- Default: `false`
- Requires a valid detached OpenPGP signature at `<archive>.sig` before using a custom jail template.

#### `--template-public-key`

- Type: string
- Default: empty
- Local path to a trusted OpenPGP public key file used when signature verification is enabled.

#### `--freebsd-interface`

- Type: string
- Default: empty
- Host network interface, for example `vtnet0`.

#### `--freebsd-ip4-address`

- Type: string
- Default: empty
- IPv4 address for a jail. This is optional; omit it to allow DHCP.

#### `--freebsd-network-mode`

- Type: string
- Default: `shared`
- Jail network mode: `shared` or `vnet`.

#### `--freebsd-vnet-gateway`

- Type: string
- Default: empty
- Default gateway for VNET jail networking when using a static IP.

#### `--freebsd-ip-base-network`

- Type: string
- Default: empty
- Base network in CIDR notation used for automatic IP allocation, such as `192.168.1.200/24`.

#### `--freebsd-ip-pool-size`

- Type: int
- Default: `0`
- Number of IPs to allocate from the base network pool.

#### `--freebsd-dns-servers`

- Type: string
- Default: empty
- Comma-separated DNS server IPs for jailed networking.

#### `--escalation-command`

- Type: string
- Default: `mdo`
- Privilege escalation command used for provisioning on FreeBSD/OpenBSD hosts.

### OpenBSD VM flags

The following apply to the `vmd` runtime only.

#### `--openbsd-vm-root`

- Type: string
- Default: `/var/wsd/vms/openbsd`
- Directory under which each OpenBSD VM disk directory is created.

#### `--openbsd-master-image`

- Type: string
- Default: empty
- Path to the golden qcow2 master image each workspace is cloned from.

#### `--openbsd-memory`

- Type: string
- Default: `1G`
- Memory size passed to `vmctl(8)` using the `-m` option.

#### `--openbsd-login-user`

- Type: string
- Default: `root`
- SSH login user used by the daemon to reach the guest VM.

#### `--openbsd-ssh-key`

- Type: string
- Default: `/var/wsd/agent_ed25519`
- Host path to the SSH private key matching the guest image.

#### `--openbsd-ssh-port`

- Type: int
- Default: `22`
- Guest SSH port.

#### `--openbsd-guest-os`

- Type: string
- Default: `openbsd`
- Guest OS type, such as `openbsd`, `freebsd`, or `linux`.

#### `--openbsd-escalation-command`

- Type: string
- Default: `doas`
- Command used on the host to create or manage VMs.

#### `--openbsd-guest-escalation-command`

- Type: string
- Default: `doas`
- Command used inside the guest VM for privilege escalation.

#### `--openbsd-boot-timeout`

- Type: duration
- Default: `5m`
- Maximum time to wait for a new VM to become reachable over SSH.

#### `--openbsd-ssh-probe-timeout`

- Type: duration
- Default: `20s`
- Bound on a single SSH readiness probe while the VM is booting.

## Backend flags

These control basic daemon behavior and resource management.

### `--provisioning-hints-file`

- Type: string
- Default: `/etc/wsd/provisioning_hints.json`
- Local path to a JSON file containing environment hints passed to the agent for each workspace.

### `--workspace-base-path`

- Type: string
- Default: `$HOME/wsd-workspaces`
- Base directory for workspace repositories on the host.

### `--bind-address`

- Type: string
- Default: `127.0.0.1`
- Address the daemon binds its HTTP server to.

### `--port`

- Type: int
- Default: `8080`
- HTTP port to listen on.

### `--cache-dir`

- Type: string
- Default: `$HOME/wsd/cache`
- Directory used for dependency cache storage.

### `--max-cache-gb`

- Type: int
- Default: `20`
- Maximum cache size in gigabytes.

### `--state-file`

- Type: string
- Default: `$HOME/wsd/state.json`
- Path used to track workspace state and help find stale leftover workspaces.

### `--jobs-file`

- Type: string
- Default: `$HOME/wsd/jobs.json`
- Path to background job metadata. Empty disables persistence.

### `--max-jobs-per-workspace`

- Type: int
- Default: `1`
- Maximum concurrent background jobs per workspace; `0` disables the limit.

### `--max-jobs-total`

- Type: int
- Default: `0`
- Maximum concurrent background jobs across the daemon; `0` disables the limit.

### `--job-max-runtime`

- Type: duration
- Default: `0`
- Maximum runtime for any background job before the daemon stops it; `0` disables the cap.

### `--job-log-max-bytes`

- Type: int64
- Default: `10 MiB`
- Maximum captured output per background-job stream; `0` disables the cap.

### `--job-retention`

- Type: duration
- Default: `24h`
- How long finished job records and log tails are retained.

### `--exec-wait-seconds`

- Type: int
- Default: `0`
- Default seconds an exec waits before being promoted to a background job. `0` disables auto-promotion.

### `--provision-wait-seconds`

- Type: int
- Default: `0`
- Default seconds a provisioning request waits before the whole operation is promoted to a background job.

### `--provision-timeout`

- Type: duration
- Default: `0`
- Global cap on provisioning duration; `0` disables the limit.

### `--workspace-ttl`

- Type: duration
- Default: `24h`
- Workspaces that have not been accessed within this window are considered stale and destroyed.

### `--prune-interval`

- Type: duration
- Default: `1h`
- Period between pruning sweeps while the daemon is running.

### `--prune-on-startup`

- Type: bool
- Default: `true`
- Runs a stale workspace prune once when the daemon starts.

### `--tls-cert-file`

- Type: string
- Default: empty
- TLS certificate file used to serve HTTPS; leave empty for plain HTTP.

### `--tls-key-file`

- Type: string
- Default: empty
- TLS private key file; leave empty for plain HTTP.

## Capacity gating flags

These options refuse provisioning when the host is already under pressure.

### `--capacity-memory-percent`

- Type: float
- Default: `0`
- Refuse to provision when memory use is at or above this percentage.

### `--capacity-swap-percent`

- Type: float
- Default: `0`
- Refuse to provision when swap use is at or above this percentage.

### `--capacity-cpu-percent`

- Type: float
- Default: `0`
- Refuse to provision when CPU use is at or above this percentage.

### `--capacity-filesystems`

- Type: string
- Default: empty
- Comma-separated path-to-max-percent rules, for example:

```sh
--capacity-filesystems '/=90,/var=85,/usr/local/jails=95'
```

## Forge and git auth flags

These settings control the single privileged `commit_and_pr` capability, which is a startup decision rather than a request-time choice.

### `--git-auth-mode`

- Type: string
- Default: value of `MCWSD_GIT_AUTH_MODE`
- Valid values: `ssh` or `token`
- Leave unset to disable commit-and-pr support.

### `--forge-type`

- Type: string
- Default: value of `MCWSD_GIT_FORGE_TYPE`
- Supported values: `gitlab`, `github`, `gitea`, or `forgejo`

### SSH auth settings

#### `--git-ssh-key-path-env-var`

- Type: string
- Default: `MCWSD_GIT_SSH_KEY_PATH`
- Environment variable name that contains the SSH private key path.

#### `--git-ssh-known-hosts-env-var`

- Type: string
- Default: `MCWSD_GIT_SSH_KNOWN_HOSTS`
- Environment variable containing the path to `known_hosts`.

### GitLab settings

#### `--gitlab-token-env-var`

- Type: string
- Default: `MCWSD_GITLAB_TOKEN`
- Environment variable name containing the GitLab token.

#### `--gitlab-token-username`

- Type: string
- Default: `oauth2`
- Username paired with the GitLab token in token mode.

#### `--gitlab-api-base`

- Type: string
- Default: `MCWSD_GITLAB_API_BASE`
- Base URL for the GitLab API, for example `https://gitlab.example.com/api/v4`.

#### `--gitlab-project-path`

- Type: string
- Default: `MCWSD_GITLAB_PROJECT_PATH`
- URL-encoded namespace/project path or numeric project ID.

### GitHub settings

#### `--github-token-env-var`

- Type: string
- Default: `MCWSD_GITHUB_TOKEN`
- Environment variable holding the GitHub token.

#### `--github-api-base`

- Type: string
- Default: `MCWSD_GITHUB_API_BASE`
- GitHub API URL, typically `https://api.github.com`.

#### `--github-project-path`

- Type: string
- Default: `MCWSD_GITHUB_PROJECT_PATH`
- `owner/repo` path, optional when it can be derived from the target repository URL.

### Gitea settings

#### `--gitea-token-env-var`

- Type: string
- Default: `MCWSD_GITEA_TOKEN`
- Environment variable holding the Gitea token.

#### `--gitea-api-base`

- Type: string
- Default: `MCWSD_GITEA_API_BASE`
- Gitea API base URL, for example `https://gitea.example.com/api/v1`.

#### `--gitea-project-path`

- Type: string
- Default: `MCWSD_GITEA_PROJECT_PATH`
- `owner/repo` path for the project.

### Forgejo settings

#### `--forgejo-token-env-var`

- Type: string
- Default: `MCWSD_FORGEJO_TOKEN`
- Environment variable holding the Forgejo token.

#### `--forgejo-api-base`

- Type: string
- Default: `MCWSD_FORGEJO_API_BASE`
- Forgejo API base URL.

#### `--forgejo-project-path`

- Type: string
- Default: `MCWSD_FORGEJO_PROJECT_PATH`
- `owner/repo` path for the project.

### Git identity settings

#### `--git-commit-email`

- Type: string
- Default: `mcwsd-agent@localhost`
- Email recorded on commits created through `commit_and_pr`.

#### `--git-commit-name`

- Type: string
- Default: `mcwsd-agent`
- Name recorded on commits created through `commit_and_pr`.

## Example environment-based configuration

This example keeps secrets out of `ps` output and uses the daemon defaults for most runtime values:

```sh
export MCWSD_TOKEN=super-secret-token
export MCWSD_GIT_AUTH_MODE=token
export MCWSD_GIT_FORGE_TYPE=forgejo
export MCWSD_FORGEJO_TOKEN=forgejo_token
export MCWSD_FORGEJO_API_BASE=https://forgejo.example.com/api/v1
export MCWSD_FORGEJO_PROJECT_PATH=team/workspace

mcwsd daemon \
  --plugin plugins/freebsd-jail.js \
  --bind-address 0.0.0.0 \
  --port 8080 \
  --workspace-base-path /srv/wsd/workspaces \
  --cache-dir /srv/wsd/cache \
  --state-file /srv/wsd/state.json \
  --jobs-file /srv/wsd/jobs.json
```

## Notes and conventions

- Most CLI options are startup-time settings; they are not meant to be changed dynamically per request.
- Secret values should be passed via environment variables, not as public command-line flags.
- The daemon is designed around a single active runtime and forge configuration at a time.
- If you disable `git-auth-mode`, commit-and-pr support is unavailable even if forge settings are present.

For the authoritative list of supported flags, use the built-in help for your installed version:

```sh
mcwsd daemon --help
```
