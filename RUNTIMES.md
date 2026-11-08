# Runtimes

Every runtime (docker, podman, kubernetes, openbsd-vmd, freebsd-jail, and
zones) is a script plugin loaded with `--plugin`. Because each plugin has a
distinct `name`, mcwsd now derives a per-runtime path for its workspace state
and background-job files when no explicit path is given, so switching runtimes
no longer collides on a shared state file.

## Per-runtime state and jobs paths

mcwsd runs either as a per-user workstation daemon or as a dedicated host
daemon. All built-in defaults are home-directory based (`~/wsd`); nothing in
the code hard-codes `/var/...`. A host daemon (installed via the docs' systemd /
rc(8) / SMF examples) passes explicit `/var/...` paths with `--state-file` /
`--jobs-file` / `--cache-dir` instead.

The `--state-file` and `--jobs-file` flags are empty by default; when left
unset they resolve to `<plugin>.state.json` and `<plugin>.jobs.json` under the
user's `~/wsd` directory:

| Command  | Base directory | State file                  | Jobs file                  |
|----------|----------------|-----------------------------|----------------------------|
| `daemon` | `~/wsd`        | `~/wsd/<plugin>.state.json` | `~/wsd/<plugin>.jobs.json` |
| `cleanup`| `~/wsd`        | `~/wsd/<plugin>.state.json` | `~/wsd/<plugin>.jobs.json` |
| `status` | `~/wsd`        | `~/wsd/<plugin>.state.json` | n/a                        |

Passing `--state-file` or `--jobs-file` explicitly always overrides the derived
path, so a pinned path (e.g. `/var/lib/wsd/...` on a dedicated host) keeps
working unchanged.

```sh
# Two runtimes, two state files, no cross-recovery:
mcwsd daemon --plugin plugins/docker.js  # uses ~/wsd/docker.state.json
mcwsd daemon --plugin plugins/podman.js  # uses ~/wsd/podman.state.json

# Dedicated host daemon pins its own path:
mcwsd daemon --plugin plugins/docker.js --state-file /var/lib/wsd/docker.state.json
```

`status` accepts `--plugin` so it can locate the per-runtime state file:

```sh
mcwsd status --plugin plugins/podman.js
mcwsd status --state-file /home/wsd/podman.state.json   # explicit path also works
```

> **Migration note:** the previous shared default (`~/wsd/state.json`) is no
> longer read automatically. If you relied on the old path, pass it explicitly
> with `--state-file` (and `--jobs-file`) once to keep using it, or let the
> daemon start fresh under the per-plugin path.

## One runtime per daemon instance

mcwsd deliberately runs exactly one runtime per daemon instance; see
[PLUGINS.md](PLUGINS.md) for the single-plugin policy and the full contract.

A typical session with the docker plugin:

```sh
bear@bbgamer:~/Workspace/mcwsd$ ~/mcwsd daemon --plugin plugins/docker.js --debug
2026-11-07T19:13:09-07:00 [DEBUG] command_start command=mcwsd daemon
2026-11-07T19:13:09-07:00 [INFO] session_started application_name=mcwsd version=0.4.000 session_id=048900910453
2026-11-07T19:13:09-07:00 [WARN] git_auth_not_configured detail=running in forgeless mode; blank workspaces are supported, repository preparation and commit_and_pr require forge authentication
2026-11-07T19:13:09-07:00 [INFO] using_script_plugin plugin=docker os=linux isolation=container
2026-11-07T19:13:09-07:00 [INFO] provisioner_validated runtime=docker
2026-11-07T19:13:09-07:00 [INFO] provisioner_initialized runtime=docker
2026-11-07T19:13:09-07:00 [INFO] job_limits_configured max_per_workspace=1 max_total=0 max_runtime=0s max_log_bytes=10485760 retention=24h0m0s
2026-11-07T19:13:09-07:00 [INFO] workspace_recovery_started
2026-11-07T19:13:09-07:00 [INFO] workspace_recovery_complete
2026-11-07T19:13:09-07:00 [INFO] startup_prune_complete removed=0 failed=0
2026-11-07T19:13:09-07:00 [INFO] server_starting address=127.0.0.1 port=8080 auth_token_set=true
```
