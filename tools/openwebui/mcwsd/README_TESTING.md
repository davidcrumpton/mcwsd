# MCWSD CLI Testing Tools

This directory contains CLI tools to test all mcwsd (Workspace Daemon) functions,
plus an offline automated suite that exercises them.

## Prerequisites

1. **mcwsd daemon must be running** on `http://localhost:9080` (or configured base URL), started with `--workspace-auth-mode=scoped` or `--workspace-auth-mode=strict`.
2. **Admin token must be set** - Either:
   - Set `MCWSD_TOKEN` environment variable before running tests
   - Or edit the `token` value in the test scripts
   - Or use the token printed by mcwsd on startup
3. **Workspace token for strict mode** - `prepare` and `full-test` learn the token from the daemon's `workspace_token` response automatically. For one-off commands against an existing workspace, pass `--workspace-token wst_...` or set `MCWSD_WORKSPACE_TOKEN`. In `scoped` mode this is optional because the admin token is accepted as break-glass.

The automated suite in `tests/` needs none of this: it runs against an
in-process fake daemon, offline.

## Exit codes

Both scripts exit `0` only when the operation actually succeeded. That is the
contract the automated suite relies on, and the reason a broken daemon fails a
run instead of printing a wall of green ticks.

| Code | Meaning |
|------|---------|
| `0` | The operation succeeded, and every verification step agreed with the daemon. |
| `1` | The operation failed, or a verification step disagreed (e.g. a file did not read back as written). |
| `2` | Bad usage (argparse rejected the arguments). |

Multi-step commands (`full-test`, `file-roundtrip`, `all`) print a summary line —
`SUMMARY: 11/11 steps passed` — and exit non-zero if any step failed.

## Tools

### 1. `test_mcwsd_cli.py` - Full-featured CLI

A comprehensive CLI with subcommands for each function plus full workflow tests.

```bash
cd tools/openwebui/mcwsd

# Show help
python test_mcwsd_cli.py --help

# Individual commands
python test_mcwsd_cli.py prepare https://github.com/user/repo.git main
python test_mcwsd_cli.py prepare                       # blank workspace, no repo
python test_mcwsd_cli.py exec ws-abc123 "go test ./..."
python test_mcwsd_cli.py exec ws-abc123 "exit 3" --expect-exit 3
python test_mcwsd_cli.py artifacts ws-abc123 src/,tests/
python test_mcwsd_cli.py status ws-abc123
python test_mcwsd_cli.py destroy ws-abc123

# File tools (each one verifies what it wrote)
python test_mcwsd_cli.py write-file ws-abc123 notes.txt "status: draft"
python test_mcwsd_cli.py read-file ws-abc123 notes.txt
python test_mcwsd_cli.py edit-file ws-abc123 notes.txt "status: draft" "status: final"
python test_mcwsd_cli.py file-roundtrip ws-abc123      # write -> read -> edit -> verify -> list

# Full workflow: prepare -> status -> exec -> file round-trip -> process -> artifacts -> destroy
python test_mcwsd_cli.py full-test https://github.com/user/repo.git main
python test_mcwsd_cli.py full-test https://github.com/user/repo.git main --keep
```

Every command that writes also verifies:

- `write-file` reads the file back and compares it byte for byte, because a
  `204` from the daemon only means the PUT was accepted.
- `edit-file` reads the file, applies the literal replacement, reads it again,
  and fails unless the replacement landed and the original pattern is gone.
- `artifacts --expect-path notes.txt` fails unless that path appears in the
  returned artifacts, which is what `commit_and_pr` would push.

### 2. `test_mcwsd_functions.py` - Individual Function Tests

Simpler tool focused on testing individual functions with clear success/failure
output.

```bash
cd tools/openwebui/mcwsd

# Show help
python test_mcwsd_functions.py --help

# Test individual functions
python test_mcwsd_functions.py prepare https://github.com/user/repo.git main
python test_mcwsd_functions.py exec ws-abc123 "echo hello" --timeout 60
python test_mcwsd_functions.py exec ws-abc123 "exit 3" --expect-exit 3
python test_mcwsd_functions.py artifacts ws-abc123 --expect-path notes.txt
python test_mcwsd_functions.py status ws-abc123
python test_mcwsd_functions.py destroy ws-abc123

# File tools
python test_mcwsd_functions.py list_files ws-abc123 .
python test_mcwsd_functions.py read_file ws-abc123 README.md
python test_mcwsd_functions.py write_file ws-abc123 notes.txt "hello world"
python test_mcwsd_functions.py safe_edit ws-abc123 notes.txt world there
python test_mcwsd_functions.py file_roundtrip ws-abc123

# Run all tests on an existing workspace
python test_mcwsd_functions.py all ws-abc123
python test_mcwsd_functions.py all ws-abc123 --work-dir /workspace/repo --destroy
```

`all` leaves the workspace alive by default (so a failure can be inspected, or
another test run); pass `--destroy` to tear it down, and `--commit` to include
`commit_and_pr` (which needs forge auth on the daemon).

### 3. `tests/` - Automated suite

Positive and negative tests for all three files, run offline against an
in-process fake daemon that speaks the real HTTP contract. The suite lives at
the **repository root** (`<repo root>/tests/`), not in this directory.

```sh
cd <repo root>
venv/bin/python -m tests   # ~13 s, no daemon required
```

See [`../../../tests/README.md`](../../../tests/README.md) for the coverage
table, the inventory gate, and how to add a case.

## Configuration

Edit the `get_tools()` function in either script to configure:

```python
def get_tools(workspace_id="", workspace_token=""):
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
export MCWSD_TIMEOUT=60                    # per-request HTTP timeout
export MCWSD_PROVISION_TIMEOUT=1800        # how long to poll a backgrounded provision
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

[Step 1/8] Preparing workspace...
Preparing workspace from https://github.com/example/repo.git (ref=main)...
Response:
{
  "workspace_id": "ws-abc123",
  "workspace_token": "wst_<workspace-capability>",
  "os": "linux",
  "work_dir": "/workspace/repo",
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
  Work dir: /workspace/repo
  Environment: python (package manager: pip)
  Cache hit: True

[Step 2/8] Checking workspace status...
✓ workspace status: alive

[Step 3/8] Executing a command...
✓ command exit code 0 (expected 0)

[Step 4/8] File round-trip (write -> read -> edit -> verify)...
✓ File mcwsd_roundtrip_1a2b3c4d.txt written successfully.
✓ mcwsd_roundtrip_1a2b3c4d.txt matches the expected content (49 bytes)
✓ mcwsd_roundtrip_1a2b3c4d.txt edited and verified
✓ artifacts contain 'mcwsd_roundtrip_1a2b3c4d.txt'
✓ file round-trip passed (6 steps) for mcwsd_roundtrip_1a2b3c4d.txt

[Step 5/8] Listing workspace files...
✓ 3 entries listed

[Step 6/8] Starting a background process...
✓ process started: job-abc123 (status: running)
✓ status retrieved (cursor: '1')
✓ 1 process(es) tracked
✓ process killed

[Step 7/8] Fetching artifacts...
✓ artifacts retrieved (2 entries)

[Step 8/8] Destroying workspace...
✓ workspace destroyed

============================================================
✓ FULL TEST PASSED — 11/11 steps passed
SUMMARY: 11/11 steps passed
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

### A write reported success but the file did not change
→ `write-file` and `write_file` read the file back and fail the command, so this
should surface as `✗ … content mismatch` / `✗ FAILED`. If you see success with a
stale file, that is a bug in the script, not in your invocation.

## Adding support for other Git Forges
To support new features, update `mcwsd.py` as well as the test tools (`test_mcwsd_cli.py`, `test_mcwsd_functions.py` and the cases in `tests/`) to support the new additions.

1. **Forge selection is server-side**: `commit_and_pr` takes `project_path` and forwards it to the daemon, but deliberately does **not** take a `forge_type`. The forge (GitHub, GitLab, Gitea, ...) is chosen by the daemon at startup via `-git-forge-type`, so an agent cannot redirect a commit to a different forge than the operator configured.
2. **Updated `test_mcwsd_cli.py`**:
   - Add a new `commit-and-pr` sub-command to allow testing of this specific functionality and passing the repository options (`--project`).
   - Added a global `--provisioner` option, allowing you to explicitly inform the test script which daemon configuration (e.g., `docker`, `podman`, `kubernetes`, `freebsd-jails`) is being used so that tests can adapt their configurations accordingly.
3. **Updated `test_mcwsd_functions.py`**:
   - Added the global `--provisioner` configuration option to match the full CLI tool.
   - Updated the `commit_and_pr` test function to accept the optional `--project` and `--desc` arguments to test those features properly.
4. **Updated `tests/`** (at the repo root): add a case to `tests/case_inventory.py`, write the test, and map it in that module's `CASES` dict — otherwise `venv/bin/python -m tests` fails the coverage gate.

Use the test commands with the provisioner flag to control your daemon, for example:

```bash
python test_mcwsd_cli.py --provisioner podman full-test https://github.com/user/repo.git main
```
