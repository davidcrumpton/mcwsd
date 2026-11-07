# Mr. Crumpton's Workspace Daemon (mcwsd)

![Logo](./images/mcwsd-256x189.png)

`mcwsd` is a Go-based daemon designed to manage ephemeral development environments for coding agents. It abstracts the complexity of environment provisioning, dependency installation, and container lifecycle management behind a simple HTTP API. Best practice is to build out a host dedicated to the Workspace Daemon.

See the [OpenWebUI Examples](./examples/OpenWebUI.md) directory for usage instructions or the [MCP Examples](./examples/MCP.md) directory for MCP-specific usage, or [combined tool usage](./examples/MCWSD-AND-FORGE-TOOL.md) for a combined tool usage.  You must create your own code for a [headless client](./examples/HEADLESS.md) which can run without AI, just program logic.

## Requirements

- Go 1.25 or later
- Docker, Podman, or Kubernetes (for container support)
- FreeBSD or OpenBSD (for jail/VMD support)
- OpenIndiana/illumos (for zones support)

## Features

- **Automated Environment Detection**: Automatically detects project types including Go, Rust, Python (via `pyproject.toml` or `requirements.txt`), and Node.js (via `package.json`). Supports `.devcontainer` configurations.
- **Sandboxed Execution**: Utilizes OCI containers (docker, podman, kubernetes), FreeBSD jails, OpenBSD VMD, and OpenIndiana/illumos zones for sandboxed execution.
- **Smart Caching**: Implements a high-performance caching mechanism based on dependency manifests (e.g., `go.sum`, `package-lock.json`) to skip redundant dependency installations.
- **Secure Secret Injection**: Provides short-lived, single-use environment variable injection for sensitive credentials during command execution.
- **Artifact Retrieval**: Easily retrieve changes from workspaces via unified `git diff` patches and untracked file lists.
- **API-First Design**: A lightweight RESTful API designed specifically for consumption by LLM agents and automation tools.
- **MCP Server**: A JavaScript Plugin (mcwsd.js) exists for the [MCP Host Engine](https://github.com/davidcrumpton/mcp-host-engine).  See the instructions for setup.

## Quick Starts

Substitute podman for docker below if you use docker.  This will create the files and directories needed for the daemon to run in the user's home directory.
You must note the generated token and have your API client configured to use it or use the export command `export MCWSD_TOKEN="wsd-<token>"` to set it before you run the daemon:

```bash
mcwsd daemon --runtime podman  --port 9080 --bind-address=0.0.0.0 --debug
# We set the token in the environment variable to avoid having to pass it to the daemon, this is useful if you are running the bind address in case your agent is in docker and can't reach mcwsd via localhost.

# Running the daemon locally allows you to enter the container and see what the AI is doing.  Nothing prohibits you from execing docker exec -it <container-id> to see what is happening inside the container.  The same is true for all other runtimes.
```

## Installation

### Prerequisites

Before running the daemon, ensure you have:

- [Go](https://go.dev/) (1.21+)
- [Docker](https://www.docker.com/) or Podman (3.4+)
- [Kubernetes](https://kubernetes.io/) (optional, need kubeconfig and kubectl for Kubernetes support)

> **Note:** For alternative runtimes:
> - FreeBSD jails: see [`FREEBSD-SETUP.md`](FREEBSD-SETUP.md)
> - OpenBSD vmd VMs: see [`OPENBSD-VMD-SETUP.md`](OPENBSD-VMD-SETUP.md)
> - OpenIndiana/illumos zones: see [`ZONES.md`](ZONES.md)

### Running the Daemon

```bash
go run cmd/mcwsd/main.go daemon --runtime podman --port 9080 --bind-address=0.0.0.0
```

### Compiling the Daemon

```bash
go build -o mcwsd cmd/mcwsd/main.go
```

**Configuration flags** (all optional, with defaults shown):

| Flag | Default | Description |
| ------ | --------- | ------------- |
| `--port` | `8080` | HTTP port to listen on |
| `--cache-dir` | `/var/lib/wsd/cache` | Dependency cache directory |
| `--max-cache-gb` | `20` | Maximum cache size in GB |
| `--state-file` | `/var/lib/wsd/state.json` | Workspace tracking file path |
| `--workspace-ttl` | `24h` | Workspace TTL before auto-destruction |
| `--prune-interval` | `1h` | How often to prune stale workspaces while running |
| `--prune-on-startup` | `true` | Prune on startup before serving requests |
| `--prune` | `false` | Prune now, then exit (for cron use) |
| `--cleanup-orphans` | `false` | Force-remove leftover containers from unclean shutdowns |
| `--capacity-memory-percent` | `0` | Memory threshold (%) - disable with `0` |
| `--capacity-swap-percent` | `0` | Swap threshold (%) - disable with `0` |
| `--capacity-cpu-percent` | `0` | CPU threshold (%) - disable with `0` |
| `--capacity-filesystems` | `-` | Comma-separated path=percent rules (e.g., `"/=90,/var=85"`) |

#### Capacity Gating

For supported platforms, configure resource limits to prevent the daemon from provisioning workspaces when host resources are constrained:

```bash
mcwsd daemon \\
  --capacity-memory-percent 90 \\
  --capacity-swap-percent 50 \\
  --capacity-cpu-percent 95 \\
  --capacity-filesystems "/=90,/var=85,/usr/local/jails=95"
```

Each filesystem rule applies to the filesystem containing the specified path, allowing independent gating for root, jail pools, and workspace directories. Memory, swap, and CPU checks work on Linux and FreeBSD; on other platforms, setting these values at startup results in an error rather than silently ignored settings.

#### Workspace Cleanup

The daemon automatically maintains clean workspaces:

- **Auto-deletion**: Workspaces unused for longer than `--workspace-ttl` are destroyed
- **Startup pruning**: Stale workspaces cleaned on launch (`--prune-on-startup`)
- **Periodic pruning**: Background cleanup during operation (`--prune-interval`)
- **On-demand pruning**: Manual cleanup with `--prune`

Default retention is deliberately aggressive (24h). Extend it for longer-lived environments, or set to `0` to prune immediately:

```bash
mcwsd cleanup --workspace-ttl 0s --prune
```

#### Authentication

The daemon uses Bearer token authentication:

1. **Set via environment variable**:

```bash
  export MCWSD_TOKEN="your-secure-token"
  mcwsd daemon
```

2. **Daemon generates random token** (if `MCWSD_TOKEN` not set):

```text
   2026/07/04 15:54:28 WARN MCWSD_TOKEN not set, generating random token
   2026/07/04 15:54:28 INFO generated_token token=wsd-<long-random-number>
```

   *Note: The token is logged once at startup. Keep this value or export it with `export MCWSD_TOKEN="<token>"` before running the daemon.*

#### Audit Logging

Enable a separate structured audit trail with:

```bash
mcwsd daemon --audit
```

By default, this writes JSONL records to `~/audit.jsonl` in the daemon user's home directory. Override the destination with `--audit-file`:

```bash
mcwsd daemon --audit --audit-file /var/lib/wsd/audit.jsonl
```

The audit facility writes one JSON object per line containing the request lifecycle, outcome, workspace identifier, authenticated caller, and correlation ID. Secret-bearing keys such as `Authorization`, `token`, `password`, and `api_key` are redacted before writing. Audit logs are intended for daemon-level operator evidence and are not a guarantee of complete in-sandbox visibility.

The daemon still cannot prove exactly what every command inside a workspace did, and the shared bearer token does not identify a specific user or AI client. For that reason, audit records should be treated as durable operational evidence for API requests only, not as a definitive record of everything executed by code running in the sandbox.

#### Running in Docker Container

**Note:** Container-in-container setups are not yet supported.

To run a local workspace and inspect it:

```bash
# Run the daemon locally first (allows entry into workspaces)
mcwsd daemon --debug

# Enter any active workspace to see what's happening
docker exec -it <container-id> bash
```

#### Next Steps

- Explore the [API Reference](./docs/api.md) for endpoint documentation
- Review [Configuration Guide](./docs/config.md) for advanced settings
- Check [Troubleshooting](./docs/troubleshooting.md) for common issues

## Timeouts

A single `exec_command` is bounded by several independent timeouts that live in
three different places. They are **not** coordinated by the daemon, so they must
be aligned by the operator or the shortest one wins in surprising ways:

| Timeout | Configured in | Notes |
| --------- | --------------- | ------- |
| Agent tool-call timeout | The LLM host (e.g. LM Studio) | Cancels the MCP call when it fires. |
| MCP Host Engine HTTP timeout | `plugins.mcwsd.http_timeout_seconds` (`config.yaml`) | Also the HTTP client timeout in `host/httpclient`. |
| mcwsd exec timeout | `plugins.mcwsd.options.timeout_seconds`, or `timeout_seconds` on the request | Sent to the daemon and enforced as the command deadline. |

Rules of thumb:

- Keep the **exec timeout** (`options.timeout_seconds`, default `180`) *lower*
  than the **MCPHE HTTP timeout** (`http_timeout_seconds`, e.g. `300`) so the
  daemon returns a real "deadline exceeded" error instead of the HTTP client
  dropping the connection. A dropped connection cancels the daemon's request
  context, which is harder to clean up and produces a bare `context canceled`.
- Keep the **agent tool-call timeout** at or above the MCPHE HTTP timeout, or
  the agent gives up while the daemon is still working.
- Commands that genuinely need to outlive all of these (large package installs,
  long test suites) should run as a **background job** instead of a synchronous
  `exec_command` (see below).

The daemon makes a best effort to detach a workspace's network even when the
request is cancelled, and its network attach/detach is idempotent, so a timed-out
command no longer wedges a workspace with `endpoint ... already exists in network`.

## Background Jobs

Long-running commands can be launched as background jobs that run detached from
the request, survive the client disconnecting, and survive a daemon restart. The
AI-facing surface uses `job_id`; the original `process` routes remain as aliases.

```bash
# Start a job; returns immediately with job_id
curl -X POST .../workspaces/$WS/jobs \
  -d '{"command":"go build ./...","allow_network":true,"timeout_seconds":600}'

# Poll only NEW output using the cursor from the previous response
curl ".../workspaces/$WS/jobs/$JOB?cursor=123:0"

# List / cancel
curl .../workspaces/$WS/jobs
curl -X DELETE .../workspaces/$WS/jobs/$JOB
```

`GET /workspaces/{id}/jobs/{job_id}` returns the status
(`queued`, `running`, `succeeded`, `failed`, `cancelled`, `timed_out`), the exit
code, an `output` field of combined, timestamped JSON-lines records
(`{"ts","stream","line"}`), and a `cursor` to pass back for incremental polling.
A finished job's record and a capped tail of its output are retained so the
result can still be read after the workspace is destroyed.

`exec` can also promote a slow command automatically: send `wait_seconds` (how
long to block before promoting) or `async: true` (promote at once). A command
that finishes within the wait returns output inline; otherwise the response
carries a `job_id` and empty output, and the caller polls `get_job`. Foreground
execs that are promoted never count against the background limits, so a full
background queue cannot strand a running command; an explicit `async` start is
rejected before launch when the queue is full. Secrets are synchronous-only, so
a secret-bearing exec always blocks.

Provisioning uses the same machinery. `POST /workspaces` accepts `wait_seconds`,
`async`, and `timeout_seconds`; with `async: true` it returns
`workspace_id`/`job_id`/`status` immediately and the whole pipeline (host clone,
runtime boot, repo copy, install) runs as a background provision job. Poll
`GET /workspaces/{workspace_id}/jobs/{job_id}` exactly like any other job; when
its status reaches `succeeded`, the response carries a `workspace` object with
the same `os`/`work_dir`/`profile`/`cache_hit` fields as an inline provision.
This keeps a slow VM boot or a large clone from tripping the HTTP timeout.

Job behavior is bounded by daemon flags:

| Flag | Default | Description |
| ------ | --------- | ------------- |
| `--jobs-file` | `~/wsd/jobs.json` | Job tracking file (empty disables persistence) |
| `--max-jobs-per-workspace` | `1` | Concurrent background jobs per workspace (0 = unlimited) |
| `--max-jobs-total` | `0` | Concurrent background jobs daemon-wide (0 = unlimited) |
| `--job-max-runtime` | `0` | Daemon-wide cap on job duration (0 = disabled) |
| `--job-log-max-bytes` | `10 MiB` | Per-stream output cap (0 = unlimited) |
| `--job-retention` | `24h` | How long finished job records/logs are retained |
| `--exec-wait-seconds` | `0` | Default exec wait before promoting to a job (0 = never; requests can override) |
| `--provision-wait-seconds` | `0` | Default provision wait before promoting the whole pipeline (0 = never; requests can override) |
| `--provision-timeout` | `0` | Daemon-wide cap on a provisioning pipeline (0 = disabled; requests can override) |

The MCP/OpenWebUI tool exposes the same operations as `start_job`, `list_jobs`,
`get_job`, and `cancel_job`, and `exec_command` accepts `wait_seconds` /
`run_async`. Interactive stdin is not supported.

## Contributing

We welcome contributions! Please review our [contributing guidelines](./CONTRIBUTING.md) before submitting pull requests.

## License

This project is licensed under the MIT License - see [LICENSE](./LICENSE) for details.
