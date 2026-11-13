# Known Issues

This document tracks known bugs and behavioral issues in the project. Each entry describes the affected component, the nature of the problem, and any relevant context for future resolution.

---

### Multi-Backend Routing

Now that the Workspace Daemon supports multiple forges and backends, the `mcwsd.py` OpenWebUI tool should route requests to the correct backend based on the repository URL, rather than being locked to one workspace daemon which supports one forge and one container type.  For my use case, I don't need multi backend support but the **mcwsd.py** could be modified to support this without changing the core functionality of the daemon.

**Impact:** Low — This is more of a feature request but the ability to use an AI tool when your multi-platform code breaks on one of the platforms is essential for productive AI coding

**Proposed fix:** Wait to hear from user feedback on how to best implement this.

---

## Workspace Daemon

### `--cleanup-orphans` Flag May Require Full Configuration Options

Running `mcwsd --cleanup-orphans` in isolation fails because the daemon requires all forge-related options to be set before it will start. As a result, users must provide a complete configuration just to perform a clean-up, which is unintuitive.

**Impact:** Medium — the flag is difficult to use in practice and provides a poor operator experience.

**Proposed fix:** Allow `--cleanup-orphans` to run with a minimal config by deriving default workspace and forge paths from the daemon's configured home directory and by validating the orphan scan targets before startup so cleanup can work without a full end-user configuration.
---

### Blank Repository Causes Crash

When the Workspace Daemon attempts to process a repository with no commits or files, it crashes and surfaces an error to the AI.

**Impact:** Medium — the daemon should handle empty repositories gracefully rather than failing.

**Proposed fix:** Clone the empty repository to `/workspace/repo` as normal, and return a JSON response indicating that no files are present such as `blank_repo:1` in the JSON response.  Do not attempt to automatically populate the repository (e.g., by adding a `README.md`).

### Virtual Machine Provisioning

We should add support for provisioning virtual machines for the purpose of agentic work.  The OpenBSD VMD provisioner already does this so we have an idea of how to do it on OpenBSD.  Perhaps support for any cross platform FOSS hypervisor is a good idea, e.g. QEMU/KVM.  This would allow us to have a BYO provisioner where you make a VM that does what you want.  Virtual machines bring more complexity:  FreeBSD machine with Jails in it??  That is possible but complex to do hence the reason for FreeBSD support for jails.

**Impact:** Medium — this improves flexibility for BYO environments but adds provisioning, networking, and lifecycle complexity for operators.

**Proposed fix:** Add a generic VM provisioning backend with support for QEMU/KVM or an existing host hypervisor and define a clear BYO VM contract, including networking and bootstrap expectations.

### Capacity Failure

The capacityCPUPercent flag should probably be off since any server doing something can peg the CPU to 100% which isn't a problem on a busy server.  I may disable this but I'll play with it in the lab first.

**Impact:** Medium — overly aggressive capacity checks can cause unnecessary throttling or false failures on normal workloads.

**Proposed fix:** Default `capacityCPUPercent` to a conservative or disabled setting and document the lab-tested override path for operators who need to tune it.

### Timeout

The timeout value should be configurable on a per-workspace or per-forge basis.  Right now it's hardcoded in mcwsd.py.  This hasn't been a problem after 30 days of use.

**Impact:** Low — this issue is mostly operational and affects long-running or unusually large workspaces rather than normal use.

**Proposed fix:** Add a per-workspace and per-forge timeout override in configuration, with a default that preserves current behavior while allowing explicit tuning.

### Multiple workspaces

The Workspace Daemon supports multiple forges and backends, the `mcwsd.py` OpenWebUI tool should route requests to the correct backend but the forumala for this depends on the user.  We should just provide some examples to modify mcwsd.py to help the user.  One user doesn't use forge, one user uses two forges, another uses forge and containerization, and still another uses forge, multiple containers, and multiple backends.  These combinations are not mutually exclusive and can be combined in any way the user sees fit.

**Impact:** Medium — multi-backend routing is difficult to debug and can lead to workloads being sent to the wrong backend or forge.

**Proposed fix:** Publish concrete example configurations and route-selection templates for common combinations and add validation warnings when a request is ambiguous.

### Forge in forgeless mode

AI always thinks we can forge later, even when no forge is configured. This can lead to confusion and errors when the system attempts to clone a repository that doesn't exist or cannot be accessed.  It would be true if the daemon were in forge mode and I asked for a blank workspace.  Pulling a forge is tied to creating a workspace so right now we can't forge in forgeless mode.  AI is free to run `git clone <repository_url>` inside the workspace, but the daemon won't be able to commit and PR since that requires account credentials.

```text
Your workspace has been provisioned successfully!

**Workspace Details:**
- **Workspace ID:** `ws-401b3e162`
- **Working Directory:** `/workspace/repo`
- **Operating System:** OpenBSD
- **Status:** Blank workspace (no repository files)

You can now start creating project files, or if you have a specific repository URL you'd like to clone, let me know!
```

If we aren't configured for forging, we should fail a forge request in forgeless mode.  Otherwise, we should be able to proceed.  However, what if the user did work in the repo space?  Git pull would barf due to conflicts with the existing local changes.  Perhaps we shouldn't allow this.   Not sure!  Problem for admin at this release.  A solution would be to forge in /workspace/repo/clone-here instead of using the whole repository but we'll keep it simple for now.

**Impact:** Medium — unsupported forge operations can cause confusing AI behavior and noisy failure paths when no forge is configured.

**Proposed fix:** No good proposals so far.  We want to allow the user to use the workspace as  a forge for reading and learning, etc.  But, we can't commit and PR without a forge configured.  In this mode, the user can make some great changes and have no way to save them.  This would burden the admin to login and get the files should an organization not allow access to the workspace.  Sub directories wont help the core problem: user uses the tool and does great work that can't be saved.

### Daemon values hardcoded at startup

Perhaps the mcwsd.py script should send the configuration values as command-line arguments or environment variables instead of hardcoding them to startup CLI values.  This would let the daemon be more flexible and easier to configure in different environments.

**Impact:** Low to Medium — hardcoded startup values reduce portability and make environment-specific deployments harder to manage.

**Proposed fix:** Pass configuration through CLI arguments or environment variables at startup and validate the effective configuration before daemon startup.

### AUDIT LOGGING FILE PATH

The audit logging path file doesn't rotate and it should. The audit log file should also rotate based on size or time, and old logs should be archived or deleted to prevent disk space issues.

**Impact:** Medium — unmanaged audit logs can consume disk space and make forensic review harder over time.

**Proposed fix:** Add size- and time-based log rotation, with retention or archival rules and startup validation for the configured audit log path.

### FreeBSD Network Safety

The daemon disconnects networks when a task is done so a process can't communicate with the outside world after completion.  This doesn't work on OpenBSD with vmd(8) and FreeBSD uses ifconfig which the daemon cannot control.  Alternate methods for network management need to be explored such as pfctl, ipfw, or ifconfig down on the host TAP side of the interface (OpenBSD) or FreeBSD host epair interface side.

**Impact:** High — network isolation can fail on some platforms, leaving workspaces or containers unexpectedly exposed after a task completes.

**Proposed fix:** Review and implement platform-specific network teardown using firewall or host-side interface controls, then validate with a per-platform test matrix.

### Root Check

The root check comes too early.  I should be able to know the version and perhaps we should only enfore root on `daemon`, `status`, and `cleanup` modes where you should be the run as user for proper access.

```text
OpenWSD# mcwsd version
2026-10-11T12:42:17-06:00 [ERROR] ERROR: This program is not designed to run as root.
```

Setting an non-root user for version is NO FUN!  On the other hand, it reinforces the user's mental model to not run it as root.

```sh
OpenWSD# su -m _wsd -c "mcwsd version"
mcwsd 0.3.400
```

**Impact:** Medium — a premature root check blocks harmless operations such as version inspection and increases setup friction for non-root operators.

**Proposed fix:** Allow read-only/version checks to run without root while retaining root enforcement for privileged daemon, status, and cleanup operations.

### Capacity checks on unsupported platforms

The daemon starts up and fails the AI call if the platform isn't supported. Would be better to fail to start so the admin can fix the configuration before the AI calls it.  The daemon should start up and log a warning that the platform isn't supported and that capacity checks are disabled.

**Impact:** Medium — unsupported platforms can fail late and create a poor operator experience when the real issue is configuration or platform mismatch.

**Proposed fix:** Make the daemon start with a warning and disable capacity checks when the platform is unsupported, while clearly reporting the unsupported state in logs.

### Short Repo Path instead of URL

The model assumed the short path could be used since it already used the forgeo MCP tool to interact with the code.  The `mcwsd` replied with a blank repo detected.  Need to either make it pull based on knowing it is connected in Forge mode or error out.  The model figured it out anyway with a second provisioning call anyway.

```text
The user wants to create a workspace for the prime-finder-lite repository so they can test out the code. I need to use the mcwsd function to prepare a workspace. Looking at the parameters, I need:

CommandEvent: "prepare_workspace" (to create a workspace)
project_path or repo_url: Since this is a Forgejo/GiteA repo, I should use the project_path in owner/repo format: "bear/prime-finder-lite"
The repo_url can be left empty as it will clone from the forge URL
Let me prepare the workspace with appropriate settings.
```

**Impact:** Medium — ambiguous repository input can produce blank or incorrect workspace creation and force an unnecessary second provisioning attempt.

**Proposed fix:** Resolve short forge paths automatically when a forge is configured, or return a clear validation error if the path is ambiguous or incomplete.  Should this be in daemon or mcwsd.py?  I think it should be in the daemon since it is the one that knows about the forge configuration.

### OpenPGP Module Lacke Internal Logger

Doesn't use internal logging escpeciallu DEBUG for events. 

***Impact:** Medium — lack of internal logging makes debugging and auditing OpenPGP signing operations difficult.

**Proposed fix:** Add internal logging for OpenPGP signing operations, including DEBUG level events

### OpenPGP Signing

It doesn't use the OpenPGP keyring for signing commits.  This is a problem for users who want to sign their commits with their OpenPGP keys.  The daemon should be able to use the OpenPGP keyring to sign commits when configured to do so.

**Impact:** Medium — users relying on signed commits cannot meet their policy or trust requirements when working through the daemon.

**Proposed fix:** Add explicit OpenPGP signing configuration and key selection so commit signing uses the available keyring when enabled.

### Commit and PR

This should probably run backgrounded like exec and provision so the user can continue to work while the commit and PR is being processed.  The daemon should provide feedback on the status of the commit and PR operation, including any errors encountered during the process.

**Impact:** Medium — long-running commit/PR jobs can block user workflows and leave the operator without useful progress feedback.

**Proposed fix:** Run commit and PR creation asynchronously with status updates and clear error reporting so users can continue working while the job completes.

### Platform Features

The daemon should probably compile in the options it needs specifically for the platform it is running on.  This would reduce the complexity of the codebase and make it easier to maintain.  For example, if the daemon is running on OpenBSD, it should only include the VMD provisioner and related features, while excluding FreeBSD-specific code.

**Impact:** Low — unnecessary platform-specific code increases maintenance complexity and risks misconfiguration across deployments.

**Proposed fix:** Split platform-specific features behind compile-time or build-time flags so each daemon build only includes the relevant backend set and related features.

### Token Used for Mismatched Forge


The server below is configured for Forgejo but it can't pull an anoymous public repo from GitHub because it adds the token.

```text
2026-10-17T00:22:13-06:00 [ERROR] provision_failed error=failed to clone repo: git -c http.extraHeader=Authorization: [redacted] clone --depth 1 --branch main https://github.com/davidcrumpton/llm-control-engine.git /tmp/wsd-repo-2418104480 failed: exit status 128: Cloning into '/tmp/wsd-repo-2418104480'...
fatal: could not read Username for 'https://github.com': terminal prompts disabled
```

```text
wsd     330   0.0 54.6 3956356 1757548  -  S    14:49       3:06.61 /usr/local/bin/mcwsd daemon --port 9080 --workspace-base-path /home/wsd --cache-dir /home/wsd/cache --runtime jail --state-file /home/wsd/state.json --provisioning-hints-file=/etc/wsd/provisioning_hints.json --max-cache-gb=20 --workspace-ttl=24h --prune-interval=1h --prune-on-startup=true --capacity-memory-percent=0 --capacity-swap-percent=0 --capacity-cpu-percent=0 --capacity-filesystems= --max-jobs-per-workspace=1 --max-jobs-total=0 --job-max-runtime=0s --job-log-max-bytes=10485760 --job-retention=24h --provision-wait-seconds=0 --provision-timeout=0s --forge-type=forgejo --git-auth-mode=token --bind-address=0.0.0.0 --jobs-file=/home/wsd/jobs.json --exec-wait-seconds=300 --forgejo-api-base=http://docker-vm.crumpton.org:3000 --forgejo-token-env-var=FORGEJO_TOKEN --git-commit-name=mcwsd-agent --git-commit-email=mcwsd-agent@localhost --freebsd-release=organization-15.1-v2.tgz --freebsd-network-mode=vnet --freebsd-ip-pool-size=0 --escalation-command=doas --base-template-url=http://localhost/freebsd --verify-signature=true --template-public-key=/usr/local/etc/wsd/organization-template.pub --freebsd-interface=em0 --audit --audit-file=/var/log/mcwsd-audit.jsonl --debug
```


**Impact:** Medium — the token is used for a mismatched forge, which can lead to authentication failures.

**Proposed fix:** Ensure that the token is only used for the correct forge by checking the forge type before adding the token to the git command.  If the URL is public and doesn't match the forge we have a token for, we should not add the token to the git command.  This may require a more sophisticated URL parsing and forge matching logic in the daemon.  This may be out of scope since the tool is meant to work with the forge it is configured for.  The user should be able to configure the daemon to use the correct forge for the repository they are trying to clone.

### SSH Auth Mode with HTTPS Repo URL Fails to Push

When the daemon runs with `--git-auth-mode=ssh` but a workspace is provisioned from an `https://` repository URL, `commit_and_pr` fails at the push step:

```text
fatal: could not read Username for 'https://github.com': terminal prompts disabled
```

In SSH mode `GitAuthArgs` returns no credentials (`internal/gitpush/push.go`) and `GitEnv` only configures `GIT_SSH_COMMAND`, which applies to `git@host:path` remotes — never to `https://` remotes.  A public repo therefore clones anonymously all the way through provisioning, and only the host-side push fails, with a misleading "could not read Username" error rather than a clear auth-mode mismatch.

This is not caused by the runtime plugin conversion; the failing path (`gitpush.PushAndOpenMR` / `backend.CommitAndPR`) is unchanged host-side code, and the in-jail `git bundle create` succeeds.

**Impact:** Medium — a daemon configured for SSH auth cannot push HTTPS-provisioned workspaces, and the failure surfaces only at the end of the commit flow.

**Proposed fix:** Either run with `--git-auth-mode=token` (a `GITHUB_TOKEN` is already available on the affected host) or provision from an SSH URL (`git@github.com:...`).  Additionally, fail fast in `Validate`/`runProvision` when an `https://` URL is supplied under `--git-auth-mode=ssh`, so the mismatch is reported at provision time instead of as an obscure push error.