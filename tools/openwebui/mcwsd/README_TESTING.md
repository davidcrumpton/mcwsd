# MCWSD CLI Testing Tools

This directory contains CLI tools to test all mcwsd (Workspace Daemon) functions.

## Prerequisites

1. **mcwsd daemon must be running** on `http://localhost:9080` (or configured base URL), started with `--workspace-auth-mode=scoped` or `--workspace-auth-mode=strict`.
2. **Admin token must be set** - Either:
   - Set `MCWSD_TOKEN` environment variable before running tests
   - Or edit the `token` value in the test scripts
   - Or use the token printed by mcwsd on startup
3. **Workspace token for strict mode** - `prepare` and `full-test` learn the token from the daemon's `workspace_token` response automatically. For one-off commands against an existing workspace, pass `--workspace-token wst_...` or set `MCWSD_WORKSPACE_TOKEN`. In `scoped` mode this is optional because the admin token is accepted as break-glass.

## Tools

### 1. `test_mcwsd_cli.py` - Full-featured CLI

A comprehensive CLI with subcommands for each function plus a full workflow test.

```bash
cd /workspace/tools/openwebui

# Show help
python test_mcwsd_cli.py --help

# Individual commands
python test_mcwsd_cli.py prepare https://github.com/user/repo.git main
python test_mcwsd_cli.py exec ws-abc123 "go test ./..."
python test_mcwsd_cli.py artifacts ws-abc123 src/,tests/
python test_mcwsd_cli.py status ws-abc123
python test_mcwsd_cli.py destroy ws-abc123

# Full workflow test (prepare -> status -> exec -> artifacts -> destroy)
python test_mcwsd_cli.py full-test https://github.com/user/repo.git main
```

### 2. `test_mcwsd_functions.py` - Individual Function Tests

Simpler tool focused on testing individual functions with clear success/failure output.

```bash
cd /workspace/tools/openwebui

# Show help
python test_mcwsd_functions.py --help

# Test individual functions
python test_mcwsd_functions.py prepare https://github.com/user/repo.git main
python test_mcwsd_functions.py exec ws-abc123 "echo hello" --timeout 60
python test_mcwsd_functions.py artifacts ws-abc123
python test_mcwsd_functions.py status ws-abc123
python test_mcwsd_functions.py destroy ws-abc123

# Run all tests on an existing workspace
python test_mcwsd_functions.py all ws-abc123
```

## Configuration

Edit the `get_tools()` function in either script to configure:

```python
def get_tools():
    tools = Tools()
    tools.valves.base_url = "http://localhost:9080"  # mcwsd daemon URL
    tools.valves.token = "your-admin-token-here"     # MCWSD_TOKEN (control plane)
    tools.valves.timeout_seconds = 60                # Default timeout
    return tools
```

Or set environment variables before running:

```bash
export MCWSD_TOKEN="your-admin-token-here"
export MCWSD_BASE_URL="http://localhost:9080"
python test_mcwsd_cli.py full-test https://github.com/user/repo.git main
```

For a strict-mode daemon, one-off commands need the workspace's token:

```bash
python test_mcwsd_cli.py --workspace-token wst_... exec ws-abc123 "go test ./..."
# or:
MCWSD_WORKSPACE_TOKEN=wst_... python test_mcwsd_functions.py all ws-abc123
```

The `prepare` and `full-test` commands handle this automatically because the tool stores the `workspace_token` returned by the daemon.

## Example Output

```
$ python test_mcwsd_cli.py full-test https://github.com/example/repo.git main

============================================================
FULL WORKFLOW TEST
============================================================

[Step 1/4] Preparing workspace...
Preparing workspace from https://github.com/example/repo.git (ref=main)...
Response:
{
  "workspace_id": "ws-abc123",
  "workspace_token": "wst_<workspace-capability>",
  "os": "linux",
  "profile": {
    "language": "python",
    "version": "3.11",
    "package_manager": "pip",
    "install_command": "python -m venv .venv && .venv/bin/pip install -e .",
    "base_image": "python:3.11"
  },
  "cache_hit": true
}

✓ Workspace created: ws-abc123
  OS: linux
  Environment: python (package manager: pip)
  Cache hit: True

[Step 2/4] Checking workspace status...
Checking status of workspace ws-abc123...
Response:
{
  "status": "healthy",
  "uptime_ms": 1234
}

✓ Workspace status: healthy

[Step 3/4] Executing test command...
Executing 'echo 'Hello from mcwsd workspace!' && pwd && ls -la' in workspace ws-abc123...
Response:
{
  "stdout": "Hello from mcwsd workspace!\n/workspace/ws-abc123\n...",
  "stderr": "",
  "exit_code": 0,
  "duration_ms": 45
}

✓ Command completed with exit code: 0
  Duration: 45ms

[Step 4/4] Fetching artifacts...
Fetching artifacts from workspace ws-abc123...
Response:
{
  "patch": "",
  "untracked_files": []
}

✓ Diff patch retrieved (0 lines)
  Untracked files: 0

[Cleanup] Destroying workspace...
Destroying workspace ws-abc123...
Response: Workspace ws-abc123 destroyed.

============================================================
FULL TEST COMPLETE
============================================================
```

## Troubleshooting

### Connection refused
```
Error preparing workspace: HTTPConnectionPool(host='localhost', port=9080): Max retries exceeded...
```
→ Start the mcwsd daemon first

### Unauthorized
```
Error preparing workspace: 401 Client Error: Unauthorized
```
→ Set the correct admin token in `tools.valves.token` or `MCWSD_TOKEN` env var.

```
Error executing command: 401 Client Error: Unauthorized
```
→ In `strict` mode, workspace routes need the workspace token. Use `--workspace-token wst_...`, set `MCWSD_WORKSPACE_TOKEN`, or use `prepare`/`full-test` so the tool learns it automatically.

### Invalid workspace ID
```
Error executing command: 404 Client Error: Not Found
```
→ Ensure you're using a valid workspace_id from `prepare_workspace` response

## Adding support for other Git Forges
To support new features, update `mcwsd.py` as well as the test tools (`test_mcwsd_cli.py` and `test_mcwsd_functions.py`) to support the new additions.

1. **Forge selection is server-side**: `commit_and_pr` takes `project_path` and forwards it to the daemon, but deliberately does **not** take a `forge_type`. The forge (GitHub, GitLab, Gitea, ...) is chosen by the daemon at startup via `-git-forge-type`, so an agent cannot redirect a commit to a different forge than the operator configured.
2. **Updated `test_mcwsd_cli.py`**:
   - Add a new `commit-and-pr` sub-command to allow testing of this specific functionality and passing the repository options (`--project`).
   - Added a global `--provisioner` option, allowing you to explicitly inform the test script which daemon configuration (e.g., `docker`, `podman`, `kubernetes`, `freebsd-jails`) is being used so that tests can adapt their configurations accordingly.
3. **Updated `test_mcwsd_functions.py`**:
   - Added the global `--provisioner` configuration option to match the full CLI tool.
   - Updated the `commit_and_pr` test function to accept the optional `--project` and `--desc` arguments to test those features properly.

Use the test commands with the provisioner flag to control your daemon, for example:

```bash
python test_mcwsd_cli.py --provisioner podman full-test https://github.com/user/repo.git main
```
