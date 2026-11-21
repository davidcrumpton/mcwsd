#!/usr/bin/env python3
"""
CLI tool to test all mcwsd (Workspace Daemon) functions.

Every sub-command prints what it did, prints the daemon's response, and exits
0 only when the operation actually succeeded. That exit code is the contract
the automated suite in ./tests/ relies on, so a broken daemon or a stale
expectation fails loudly instead of printing a wall of green ticks.

Usage:
    python test_mcwsd_cli.py <command> [args...]

Commands:
    prepare [repo_url] [ref]         - Create a workspace (no repo_url = blank workspace)
    exec <workspace_id> <cmd>        - Execute a command in workspace
    artifacts <workspace_id> [paths] - Get diff/patches from workspace
    status <workspace_id>            - Check workspace status
    destroy <workspace_id>           - Destroy a workspace
    list-files <workspace_id> <path> - List files at a path in workspace
    read-file <workspace_id> <path>  - Read a file from workspace
    write-file <workspace_id> <path> <content> - Write a file in workspace
    edit-file <workspace_id> <path> <pattern> <replacement> - Literal edit (safe_edit)
    start-process <workspace_id> <cmd>          - Start a background process
    list-processes <workspace_id>               - List background processes
    process-status <workspace_id> <pid>         - Get process output/status
    send-input <workspace_id> <pid> <input>     - Send stdin to process
    kill-process <workspace_id> <pid>           - Kill a background process
    commit-and-pr <workspace_id> <msg> <title>  - Commit changes and open a PR
    file-roundtrip <workspace_id> [path]        - Write, read back, edit, verify, list
    full-test [repo_url] [ref]       - Full workflow incl. write/edit/verify round-trip

Global Options:
    --provisioner <type>            - Specify the provisioner used by the daemon (e.g., docker, podman, apple-containers, freebsd-jails)
    --workspace-token <wst_...>     - Workspace token for strict-mode daemons (or MCWSD_WORKSPACE_TOKEN)

Exit codes:
    0  the operation succeeded (and every assertion in a multi-step command held)
    1  the operation failed, or a verification step disagreed with the daemon
    2  bad usage (argparse)
"""

import argparse
import json
import os
import sys
import time
import uuid
from pathlib import Path

# Allow "python /path/to/test_mcwsd_cli.py" from any working directory.
sys.path.insert(0, str(Path(__file__).resolve().parent))

from mcwsd import Tools  # noqa: E402

OK = "✓"
BAD = "✗"


# ---------------------------------------------------------------------------
# Plumbing
# ---------------------------------------------------------------------------


def get_tools(workspace_id="", workspace_token=""):
    """Initialize tools with default valves and environment overrides."""
    tools = Tools()
    tools.valves.base_url = os.getenv("MCWSD_BASE_URL", "http://localhost:9080")
    tools.valves.token = os.getenv("MCWSD_TOKEN", "")
    tools.valves.timeout_seconds = int(os.getenv("MCWSD_TIMEOUT", 60))
    tools.valves.provision_timeout_seconds = int(
        os.getenv("MCWSD_PROVISION_TIMEOUT", 1800)
    )
    bind_workspace_token(tools, workspace_id, workspace_token)
    return tools


def bind_workspace_token(tools, workspace_id, token):
    """Bind a workspace token for strict-mode daemons."""
    if workspace_id and token:
        tools._bind_workspace_token(workspace_id, token)


def parse_response(result):
    """Split a tool result into (data, raw).

    `Tools` returns either a JSON document or an English error string such as
    "Error executing command: 401 Client Error: Unauthorized". Callers need to
    tell those apart, so this returns data=None for the error case and the raw
    text is always available for printing.
    """
    try:
        return json.loads(result), result
    except (json.JSONDecodeError, TypeError):
        return None, result if isinstance(result, str) else str(result)


def is_error(result):
    """True when a tool result is an error string rather than a JSON body."""
    if not isinstance(result, str):
        return True
    return result.lstrip().startswith("Error ") or "Client Error" in result


def report(ok, message):
    """Print a uniform pass/fail line and return the boolean."""
    print(f"{OK if ok else BAD} {message}")
    return ok


def print_json(data, raw):
    """Pretty-print a parsed response, or the raw text when it is not JSON."""
    if data is None:
        print(raw)
    else:
        print(json.dumps(data, indent=2))


# ---------------------------------------------------------------------------
# Core workspace commands
# ---------------------------------------------------------------------------


def cmd_prepare(tools, repo_url="", ref="main"):
    """Test prepare_workspace. Returns (ok, workspace_id, data)."""
    if repo_url:
        print(f"Preparing workspace from {repo_url} (ref={ref})...")
    else:
        print(f"Preparing blank workspace (ref={ref})...")

    result = tools.prepare_workspace(repo_url, ref)
    print("Response:")
    data, raw = parse_response(result)
    print_json(data, raw)

    if data is None or not data.get("workspace_id"):
        report(False, "prepare_workspace returned no workspace_id")
        return False, "", None

    workspace_id = data["workspace_id"]
    profile = data.get("profile") or {}
    print(
        f"\n{OK} Workspace created: {workspace_id}\n"
        f"  OS: {data.get('os', 'unknown')}\n"
        f"  Work dir: {data.get('work_dir', 'unknown')}\n"
        f"  Environment: {profile.get('language', 'unknown')} "
        f"(package manager: {profile.get('package_manager', 'none')})\n"
        f"  Cache hit: {data.get('cache_hit', False)}"
    )
    return True, workspace_id, data


def cmd_exec(tools, workspace_id, command, timeout=120, allow_network=False, expect_exit=0):
    """Test exec_command. `expect_exit` is the exit code the caller considers success."""
    print(f"Executing '{command}' in workspace {workspace_id}...")
    result = tools.exec_command(
        workspace_id, command, allow_network=allow_network, timeout_seconds=timeout
    )
    print("Response:")
    data, raw = parse_response(result)
    print_json(data, raw)

    if data is None:
        return report(False, f"exec_command returned a non-JSON error: {raw.strip()}")
    if "exit_code" not in data:
        # A promoted background job answers with job_id/status instead of output.
        if data.get("job_id"):
            return report(
                True,
                f"command promoted to background job {data['job_id']} "
                f"(status: {data.get('status')}); poll it with process-status",
            )
        return report(False, "exec_command returned no exit_code and no job_id")

    exit_code = data["exit_code"]
    print(f"  Duration: {data.get('duration_ms', 0)}ms")
    if data.get("stderr"):
        print(f"  stderr: {data['stderr'].strip()}")
    if expect_exit is None:
        return report(True, f"command completed with exit code {exit_code}")
    return report(
        exit_code == expect_exit,
        f"command exit code {exit_code} (expected {expect_exit})",
    )


def artifact_names(data):
    """Return the artifact names in a response, tolerating both shapes.

    Current daemons answer {"artifacts": [{"name","type","content"}]}. Older
    ones answered {"patch": "...", "untracked_files": [...]}; both are
    accepted so this tool works against either.
    """
    names = []
    if not isinstance(data, dict):
        return names
    for artifact in data.get("artifacts") or []:
        if isinstance(artifact, dict) and artifact.get("name"):
            names.append(artifact["name"])
    for name in data.get("untracked_files") or []:
        if isinstance(name, str):
            names.append(name)
    if data.get("patch"):
        names.append("changes.patch")
    return names


def cmd_artifacts(tools, workspace_id, paths="", expect_path=""):
    """Test get_artifacts. With expect_path, the named artifact must be present."""
    print(f"Fetching artifacts from workspace {workspace_id}...")
    if paths:
        print(f"  Scoped to paths: {paths}")
    result = tools.get_artifacts(workspace_id, paths)
    print("Response:")
    data, raw = parse_response(result)
    print_json(data, raw)

    if data is None:
        return report(False, f"get_artifacts returned a non-JSON error: {raw.strip()}")

    names = artifact_names(data)
    print(f"\n  Artifacts: {len(names)}")
    for name in names:
        print(f"    - {name}")
    if expect_path:
        matched = any(
            name == expect_path or name.endswith("/" + expect_path) for name in names
        )
        return report(
            matched,
            f"artifacts contain {expect_path!r}"
            if matched
            else f"artifacts do not contain {expect_path!r} (got {names})",
        )
    return report(True, f"artifacts retrieved ({len(names)} entries)")


def cmd_status(tools, workspace_id):
    """Test workspace_status."""
    print(f"Checking status of workspace {workspace_id}...")
    result = tools.workspace_status(workspace_id)
    print("Response:")
    data, raw = parse_response(result)
    print_json(data, raw)

    if data is None or "status" not in data:
        return report(False, f"workspace_status returned no status: {raw.strip()}")
    status = data["status"]
    return report(status in ("alive", "healthy", "running"), f"workspace status: {status}")


def cmd_destroy(tools, workspace_id):
    """Test destroy_workspace."""
    print(f"Destroying workspace {workspace_id}...")
    result = tools.destroy_workspace(workspace_id)
    print(f"Response: {result}")
    ok = not is_error(result) and "destroyed" in result.lower()
    return report(ok, "workspace destroyed" if ok else f"destroy failed: {result.strip()}")


def cmd_commit_and_pr(tools, workspace_id, msg, title, desc="", repo="", proj=""):
    """Test commit_and_pr."""
    print(f"Committing and opening PR in workspace {workspace_id}...")
    result = tools.commit_and_pr(
        workspace_id=workspace_id,
        commit_message=msg,
        pr_title=title,
        pr_description=desc,
        repo_url=repo,
        project_path=proj,
    )
    print("Response:")
    data, raw = parse_response(result)
    print_json(data, raw)

    if data is None:
        return report(False, f"commit_and_pr returned a non-JSON error: {raw.strip()}")
    if data.get("error"):
        # HTTP 200 with a reason (e.g. "no changes to commit") is a real
        # answer, not a transport failure, so say so plainly.
        return report(False, f"no PR opened: {data['error']}")
    if not data.get("mr_url"):
        return report(False, "commit_and_pr returned no mr_url")
    print(f"  Branch: {data.get('branch')}")
    return report(True, f"PR created: {data['mr_url']}")


# ---------------------------------------------------------------------------
# File system commands
# ---------------------------------------------------------------------------


def cmd_list_files(tools, workspace_id, path):
    """Test list_files."""
    print(f"Listing files at '{path}' in workspace {workspace_id}...")
    result = tools.list_files(workspace_id, path)
    print("Response:")
    data, raw = parse_response(result)
    print_json(data, raw)

    if data is None or "entries" not in data:
        return report(False, f"list_files returned no entries: {raw.strip()}")
    entries = data["entries"]
    print(f"\n  {len(entries)} entries returned")
    for entry in entries:
        kind = "DIR " if entry.get("is_dir") else f"{entry.get('size', 0):>8}B"
        print(f"  [{kind}] {entry.get('name')}")
    return report(True, f"{len(entries)} entries listed")


def cmd_read_file(tools, workspace_id, path, expect_content=None):
    """Test read_file. With expect_content, the bytes must match exactly."""
    print(f"Reading file '{path}' from workspace {workspace_id}...")
    result = tools.read_file(workspace_id, path)
    data, raw = parse_response(result)

    if data is None or "content" not in data:
        print("Response:")
        print(raw)
        return report(False, f"read_file failed: {raw.strip()}")

    content = data["content"]
    lines = content.count("\n") + (1 if content else 0)
    print(f"[{lines} lines, {len(content)} bytes]")
    print(content)

    if expect_content is None:
        return report(True, f"file read ({len(content)} bytes)")
    if content == expect_content:
        return report(True, f"{path} matches the expected content ({len(content)} bytes)")
    return report(
        False,
        f"{path} content mismatch\n  expected: {expect_content!r}\n  actual:   {content!r}",
    )


def cmd_write_file(tools, workspace_id, path, content):
    """Test write_file, then read the file back so 'written' is verified."""
    print(f"Writing to '{path}' in workspace {workspace_id}...")
    result = tools.write_file(workspace_id, path, content)
    print(f"Response: {result}")
    if is_error(result):
        return report(False, f"write_file failed: {result.strip()}")

    # write_file only reports that the daemon accepted the PUT; read it back
    # so a silently dropped write cannot pass as success.
    print(f"Verifying '{path}' by reading it back...")
    return cmd_read_file(tools, workspace_id, path, expect_content=content)


def cmd_edit_file(tools, workspace_id, path, pattern, replacement):
    """Test safe_edit, then verify the replacement landed and the pattern is gone."""
    print(f"Editing '{path}' in workspace {workspace_id}...")
    print(f"  pattern:     {pattern!r}")
    print(f"  replacement: {replacement!r}")

    before = parse_response(tools.read_file(workspace_id, path))[0]
    if before is None or "content" not in before:
        return report(False, f"cannot edit {path}: file is not readable")
    original = before["content"]
    if pattern not in original:
        return report(False, f"pattern {pattern!r} does not appear in {path}")

    result = tools.safe_edit(workspace_id, path, pattern, replacement)
    print(f"Response: {result}")
    data, raw = parse_response(result)
    if data is None:
        return report(False, f"safe_edit failed: {raw.strip()}")
    if data.get("success") is False:
        return report(False, f"safe_edit reported failure: {data.get('message')}")

    after = parse_response(tools.read_file(workspace_id, path))[0]
    if after is None or "content" not in after:
        return report(False, f"cannot verify {path}: file is not readable after edit")
    edited = after["content"]
    expected = original.replace(pattern, replacement)

    if edited != expected:
        return report(
            False,
            f"{path} does not contain the edit\n  expected: {expected!r}\n  actual:   {edited!r}",
        )
    if pattern != replacement and pattern in edited:
        return report(False, f"{path} still contains the original pattern {pattern!r}")
    return report(True, f"{path} edited and verified")


def cmd_file_roundtrip(tools, workspace_id, path=""):
    """Write a file, read it back, edit it, verify the edit, and list it.

    This is the whole reason the file tools exist: an agent writes a file,
    changes one string in it, and reads it again. Exercising that loop end to
    end catches a daemon that accepts a PUT but drops it, an editor that
    replaces nothing, or a read that returns a stale body.
    """
    name = path or f"mcwsd_roundtrip_{uuid.uuid4().hex[:8]}.txt"
    marker = uuid.uuid4().hex[:8]
    original = f"mcwsd roundtrip {marker}\nstatus: draft\n"
    edited_marker = f"status: edited-{marker}"
    replaced = original.replace("status: draft", edited_marker)

    print("=" * 60)
    print(f"FILE ROUND-TRIP: {name}")
    print("=" * 60)

    steps = []
    steps.append(("write", cmd_write_file(tools, workspace_id, name, original)))
    steps.append(("read-back", cmd_read_file(tools, workspace_id, name, expect_content=original)))
    steps.append(
        ("edit", cmd_edit_file(tools, workspace_id, name, "status: draft", edited_marker))
    )
    steps.append(
        ("read-after-edit", cmd_read_file(tools, workspace_id, name, expect_content=replaced))
    )

    # The edited file must also show up in the artifacts the daemon reports,
    # otherwise a later commit_and_pr would push nothing.
    parent = os.path.dirname(name) or "."
    steps.append(
        ("artifacts", cmd_artifacts(tools, workspace_id, "", expect_path=name))
    )
    steps.append(("list", cmd_list_files(tools, workspace_id, parent)))

    failed = [label for label, ok in steps if not ok]
    print()
    if failed:
        return report(False, f"file round-trip FAILED at: {', '.join(failed)}")
    return report(True, f"file round-trip passed ({len(steps)} steps) for {name}")


# ---------------------------------------------------------------------------
# Background process / job commands
# ---------------------------------------------------------------------------


def _job_id(data):
    return (data or {}).get("job_id") or (data or {}).get("id") or ""


def cmd_start_process(tools, workspace_id, command, allow_network=False):
    """Test start_process (deprecated alias for start_job). Returns (ok, job_id)."""
    print(f"Starting background process in workspace {workspace_id}...")
    print(f"  Command: {command}")
    result = tools.start_process(workspace_id, command, allow_network=allow_network)
    print("Response:")
    data, raw = parse_response(result)
    print_json(data, raw)

    if data is None:
        return report(False, f"start_process returned a non-JSON error: {raw.strip()}"), ""
    job_id = _job_id(data)
    if not job_id:
        return report(False, "start_process returned no job_id"), ""
    return report(True, f"process started: {job_id} (status: {data.get('status')})"), job_id


def cmd_list_processes(tools, workspace_id):
    """Test list_processes."""
    print(f"Listing processes in workspace {workspace_id}...")
    result = tools.list_processes(workspace_id)
    print("Response:")
    data, raw = parse_response(result)
    print_json(data, raw)

    if data is None:
        return report(False, f"list_processes returned a non-JSON error: {raw.strip()}")
    jobs = data.get("jobs", data.get("processes", []))
    if not isinstance(jobs, list):
        return report(False, "list_processes returned no jobs array")
    return report(True, f"{len(jobs)} process(es) tracked")


def cmd_process_status(tools, workspace_id, process_id):
    """Test get_process_status."""
    print(f"Getting status of process {process_id} in workspace {workspace_id}...")
    result = tools.get_process_status(workspace_id, process_id)
    data, raw = parse_response(result)
    if data is None:
        print(raw)
        return report(False, f"get_process_status failed: {raw.strip()}")

    print(f"  ID:      {data.get('id') or data.get('job_id')}")
    print(f"  Status:  {data.get('status')}")
    print(f"  Command: {data.get('command')}")
    if data.get("exit_code") is not None:
        print(f"  Exit:    {data.get('exit_code')}")
    for stream in ("stdout", "stderr", "output"):
        text = data.get(stream, "")
        if text:
            print(f"\n--- {stream} ---\n{text}")
    if not data.get("status"):
        return report(False, "get_process_status returned no status")
    return report(True, f"status retrieved (cursor: {data.get('cursor')!r})")


def cmd_send_input(tools, workspace_id, process_id, input_text):
    """Test send_process_input.

    Background jobs have no stdin: the tool reports that in a sentence and the
    daemon answers 501. Either answer is correct — anything else is a failure.
    """
    print(f"Sending input to process {process_id} in workspace {workspace_id}...")
    result = tools.send_process_input(workspace_id, process_id, input_text)
    print(f"Response: {result}")
    lowered = result.lower()
    ok = "not supported" in lowered or "sent" in lowered
    return report(ok, "stdin unsupported for background jobs (expected)" if ok else "unexpected answer")


def cmd_kill_process(tools, workspace_id, process_id):
    """Test kill_process."""
    print(f"Killing process {process_id} in workspace {workspace_id}...")
    result = tools.kill_process(workspace_id, process_id)
    print(f"Response: {result}")
    ok = not is_error(result) and ("cancel" in result.lower() or "kill" in result.lower())
    return report(ok, "process killed" if ok else f"kill failed: {result.strip()}")


# ---------------------------------------------------------------------------
# Composite workflows
# ---------------------------------------------------------------------------


def cmd_full_test(tools, repo_url="", ref="main", keep=False):
    """Run the full workflow: prepare -> status -> exec -> file round-trip ->
    process -> artifacts -> destroy.

    Every step is checked. The workspace is destroyed on the way out even when
    a step fails, so a broken run never leaks a container.
    """
    print("=" * 60)
    print("FULL WORKFLOW TEST")
    print("=" * 60)

    steps = []
    workspace_id = ""
    try:
        print("\n[Step 1/8] Preparing workspace...")
        ok, workspace_id, data = cmd_prepare(tools, repo_url, ref)
        steps.append(("prepare", ok))
        if not ok:
            # Nothing else can run without a workspace, but the summary below
            # still prints: a failed run must always end in a verdict.
            print(f"\n{BAD} cannot continue without a workspace")
            return _summarise(steps)

        work_dir = (data or {}).get("work_dir") or "/workspace/repo"

        print("\n[Step 2/8] Checking workspace status...")
        steps.append(("status", cmd_status(tools, workspace_id)))

        print("\n[Step 3/8] Executing a command...")
        steps.append(
            (
                "exec",
                cmd_exec(
                    tools,
                    workspace_id,
                    "echo 'Hello from mcwsd workspace!' && pwd",
                ),
            )
        )

        print("\n[Step 4/8] File round-trip (write -> read -> edit -> verify)...")
        steps.append(("file-roundtrip", cmd_file_roundtrip(tools, workspace_id)))

        print("\n[Step 5/8] Listing workspace files...")
        steps.append(("list-files", cmd_list_files(tools, workspace_id, work_dir)))

        print("\n[Step 6/8] Starting a background process...")
        ok, job_id = cmd_start_process(tools, workspace_id, "sleep 5 && echo done")
        steps.append(("start-process", ok))
        if job_id:
            time.sleep(1)
            steps.append(("process-status", cmd_process_status(tools, workspace_id, job_id)))
            steps.append(("list-processes", cmd_list_processes(tools, workspace_id)))
            steps.append(("kill-process", cmd_kill_process(tools, workspace_id, job_id)))

        print("\n[Step 7/8] Fetching artifacts...")
        steps.append(("artifacts", cmd_artifacts(tools, workspace_id)))
    finally:
        if workspace_id and not keep:
            print("\n[Step 8/8] Destroying workspace...")
            steps.append(("destroy", cmd_destroy(tools, workspace_id)))
        elif workspace_id:
            print(f"\n[Step 8/8] Keeping workspace {workspace_id} (--keep)")

    return _summarise(steps)


def _summarise(steps):
    """Print the verdict for a multi-step run and return whether it passed."""
    passed = sum(1 for _, ok in steps if ok)
    failed = [name for name, ok in steps if not ok]
    print("\n" + "=" * 60)
    if failed:
        print(f"{BAD} FULL TEST FAILED — {passed}/{len(steps)} steps passed")
        print(f"  failed: {', '.join(failed)}")
    else:
        print(f"{OK} FULL TEST PASSED — {passed}/{len(steps)} steps passed")
    print(f"SUMMARY: {passed}/{len(steps)} steps passed")
    print("=" * 60)
    return not failed


# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------


def build_parser():
    parser = argparse.ArgumentParser(
        description="CLI tool to test mcwsd (Workspace Daemon) functions",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Examples:
  %(prog)s prepare https://github.com/user/repo.git main
  %(prog)s prepare                     # blank workspace, no repo
  %(prog)s exec ws-abc123 "go test ./..."
  %(prog)s exec ws-abc123 "exit 3" --expect-exit 3
  %(prog)s artifacts ws-abc123 src/,tests/
  %(prog)s status ws-abc123
  %(prog)s destroy ws-abc123
  %(prog)s list-files ws-abc123 /workspace/repo
  %(prog)s read-file ws-abc123 /workspace/repo/README.md
  %(prog)s write-file ws-abc123 /workspace/repo/hello.txt "hello world"
  %(prog)s edit-file ws-abc123 /workspace/repo/hello.txt world there
  %(prog)s file-roundtrip ws-abc123
  %(prog)s start-process ws-abc123 "sleep 30"
  %(prog)s list-processes ws-abc123
  %(prog)s process-status ws-abc123 proc-abc123def4
  %(prog)s send-input ws-abc123 proc-abc123def4 "yes\\n"
  %(prog)s kill-process ws-abc123 proc-abc123def4
  %(prog)s full-test https://github.com/user/repo.git main
        """,
    )

    # Global options
    # Provisioner is unused in the code base at this time.
    parser.add_argument(
        "--provisioner",
        default="",
        help="Specify the provisioner used by the daemon (e.g., docker, podman, apple-containers, freebsd-jails)",
    )
    parser.add_argument(
        "--workspace-token",
        default=os.getenv("MCWSD_WORKSPACE_TOKEN", ""),
        help="Workspace token for strict mode (or set MCWSD_WORKSPACE_TOKEN). Not needed for prepare/full-test, which learn it automatically.",
    )

    subparsers = parser.add_subparsers(dest="subcommand", help="Available commands")

    # prepare command
    prep_parser = subparsers.add_parser("prepare", help="Create a new workspace")
    prep_parser.add_argument(
        "repo_url", nargs="?", default="", help="Git repository URL (omit for a blank workspace)"
    )
    prep_parser.add_argument(
        "ref", nargs="?", default="main", help="Branch/tag/commit (default: main)"
    )

    # exec command
    exec_parser = subparsers.add_parser("exec", help="Execute a command in workspace")
    exec_parser.add_argument("workspace_id", help="Workspace ID")
    exec_parser.add_argument("command", help="Command to execute")
    exec_parser.add_argument("--timeout", type=int, default=120, help="Timeout in seconds")
    exec_parser.add_argument("--allow-network", action="store_true", help="Allow network access")
    exec_parser.add_argument(
        "--expect-exit",
        type=int,
        default=0,
        help="Exit code that counts as success (default: 0)",
    )

    # artifacts command
    art_parser = subparsers.add_parser("artifacts", help="Get diff/patches from workspace")
    art_parser.add_argument("workspace_id", help="Workspace ID")
    art_parser.add_argument("paths", nargs="?", default="", help="Comma-separated paths to scope diff")
    art_parser.add_argument(
        "--expect-path",
        default="",
        help="Fail unless this path appears among the returned artifacts",
    )

    # status command
    stat_parser = subparsers.add_parser("status", help="Check workspace status")
    stat_parser.add_argument("workspace_id", help="Workspace ID")

    # destroy command
    dest_parser = subparsers.add_parser("destroy", help="Destroy a workspace")
    dest_parser.add_argument("workspace_id", help="Workspace ID")

    # list-files command
    lf_parser = subparsers.add_parser("list-files", help="List files at a path in workspace")
    lf_parser.add_argument("workspace_id", help="Workspace ID")
    lf_parser.add_argument("path", help="Path inside the repo to list (or an absolute path inside it)")

    # read-file command
    rf_parser = subparsers.add_parser("read-file", help="Read a file from workspace")
    rf_parser.add_argument("workspace_id", help="Workspace ID")
    rf_parser.add_argument("path", help="Path to the file inside the repo")
    rf_parser.add_argument(
        "--expect-content",
        default=None,
        help="Fail unless the file content matches this string exactly",
    )

    # write-file command
    wf_parser = subparsers.add_parser("write-file", help="Write a file in workspace")
    wf_parser.add_argument("workspace_id", help="Workspace ID")
    wf_parser.add_argument("path", help="Path to write inside the repo")
    wf_parser.add_argument("content", help="Text content to write")

    # edit-file command
    ef_parser = subparsers.add_parser(
        "edit-file", help="Literal find/replace in a workspace file (safe_edit)"
    )
    ef_parser.add_argument("workspace_id", help="Workspace ID")
    ef_parser.add_argument("path", help="Path to the file inside the repo")
    ef_parser.add_argument("pattern", help="Exact literal string to find")
    ef_parser.add_argument("replacement", help="String to replace every occurrence with")

    # file-roundtrip command
    fr_parser = subparsers.add_parser(
        "file-roundtrip", help="Write, read back, edit, verify, and list one file"
    )
    fr_parser.add_argument("workspace_id", help="Workspace ID")
    fr_parser.add_argument(
        "path", nargs="?", default="", help="File to use (default: a unique name in the repo root)"
    )

    # start-process command
    sp_parser = subparsers.add_parser("start-process", help="Start a background process")
    sp_parser.add_argument("workspace_id", help="Workspace ID")
    sp_parser.add_argument("command", help="Shell command to run in the background")
    sp_parser.add_argument("--allow-network", action="store_true", help="Allow network access")

    # list-processes command
    lp_parser = subparsers.add_parser("list-processes", help="List background processes")
    lp_parser.add_argument("workspace_id", help="Workspace ID")

    # process-status command
    ps_parser = subparsers.add_parser("process-status", help="Get process output/status")
    ps_parser.add_argument("workspace_id", help="Workspace ID")
    ps_parser.add_argument("process_id", help="Process ID returned by start-process")

    # send-input command
    si_parser = subparsers.add_parser("send-input", help="Send stdin to a background process")
    si_parser.add_argument("workspace_id", help="Workspace ID")
    si_parser.add_argument("process_id", help="Process ID returned by start-process")
    si_parser.add_argument("input_text", help="Text to write to stdin")

    # kill-process command
    kp_parser = subparsers.add_parser("kill-process", help="Kill a background process")
    kp_parser.add_argument("workspace_id", help="Workspace ID")
    kp_parser.add_argument("process_id", help="Process ID returned by start-process")

    # commit-and-pr command
    cpr_parser = subparsers.add_parser("commit-and-pr", help="Commit changes and open a PR")
    cpr_parser.add_argument("workspace_id", help="Workspace ID")
    cpr_parser.add_argument("commit_message", help="Commit message")
    cpr_parser.add_argument("pr_title", help="PR Title")
    cpr_parser.add_argument("--desc", default="", help="PR Description")
    cpr_parser.add_argument("--repo", default="", help="Repository URL to push to")
    cpr_parser.add_argument("--project", default="", help="Project path or owner/repo")

    # full-test command
    full_parser = subparsers.add_parser("full-test", help="Run full workflow test")
    full_parser.add_argument("repo_url", nargs="?", default="", help="Git repository URL")
    full_parser.add_argument("ref", nargs="?", default="main", help="Branch/tag/commit (default: main)")
    full_parser.add_argument(
        "--keep", action="store_true", help="Leave the workspace running (useful for debugging)"
    )

    return parser


def main(argv=None):
    parser = build_parser()
    args = parser.parse_args(argv)

    if not args.subcommand:
        parser.print_help()
        return 1

    tools = get_tools(getattr(args, "workspace_id", ""), args.workspace_token)

    if args.subcommand == "prepare":
        ok, _, _ = cmd_prepare(tools, args.repo_url, args.ref)
    elif args.subcommand == "exec":
        ok = cmd_exec(
            tools,
            args.workspace_id,
            args.command,
            args.timeout,
            args.allow_network,
            args.expect_exit,
        )
    elif args.subcommand == "artifacts":
        ok = cmd_artifacts(tools, args.workspace_id, args.paths, args.expect_path)
    elif args.subcommand == "status":
        ok = cmd_status(tools, args.workspace_id)
    elif args.subcommand == "destroy":
        ok = cmd_destroy(tools, args.workspace_id)
    elif args.subcommand == "list-files":
        ok = cmd_list_files(tools, args.workspace_id, args.path)
    elif args.subcommand == "read-file":
        ok = cmd_read_file(tools, args.workspace_id, args.path, args.expect_content)
    elif args.subcommand == "write-file":
        ok = cmd_write_file(tools, args.workspace_id, args.path, args.content)
    elif args.subcommand == "edit-file":
        ok = cmd_edit_file(tools, args.workspace_id, args.path, args.pattern, args.replacement)
    elif args.subcommand == "file-roundtrip":
        ok = cmd_file_roundtrip(tools, args.workspace_id, args.path)
    elif args.subcommand == "start-process":
        ok, _ = cmd_start_process(tools, args.workspace_id, args.command, args.allow_network)
    elif args.subcommand == "list-processes":
        ok = cmd_list_processes(tools, args.workspace_id)
    elif args.subcommand == "process-status":
        ok = cmd_process_status(tools, args.workspace_id, args.process_id)
    elif args.subcommand == "send-input":
        ok = cmd_send_input(tools, args.workspace_id, args.process_id, args.input_text)
    elif args.subcommand == "kill-process":
        ok = cmd_kill_process(tools, args.workspace_id, args.process_id)
    elif args.subcommand == "commit-and-pr":
        ok = cmd_commit_and_pr(
            tools,
            args.workspace_id,
            args.commit_message,
            args.pr_title,
            args.desc,
            args.repo,
            args.project,
        )
    elif args.subcommand == "full-test":
        ok = cmd_full_test(tools, args.repo_url, args.ref, args.keep)
    else:
        parser.print_help()
        return 1

    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
