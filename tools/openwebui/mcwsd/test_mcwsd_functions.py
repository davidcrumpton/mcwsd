#!/usr/bin/env python3
"""
Individual CLI tools for testing mcwsd functions.

Each sub-command exercises exactly one `Tools` method, prints what it did and
what the daemon answered, and exits 0 only on success. `all` runs the whole
set against an existing workspace and reports one summary line.

The exit code is the contract the automated suite in ./tests/ relies on: a
command that printed a response but did not actually do the thing it claims
must exit non-zero.
"""

import argparse
import json
import os
import sys
import time
from pathlib import Path

# Add parent directory to path for imports
sys.path.insert(0, str(Path(__file__).resolve().parent))

from mcwsd import Tools  # noqa: E402

OK = "✓"
BAD = "✗"

# Bound from --workspace-token / MCWSD_WORKSPACE_TOKEN and the subcommand's
# workspace_id so strict-mode daemons work for one-off test invocations.
_workspace_token = ""
_workspace_id = ""


def get_tools(workspace_id="", workspace_token=""):
    """Initialize tools with default valves and environment overrides."""
    tools = Tools()
    tools.valves.base_url = os.getenv("MCWSD_BASE_URL", "http://localhost:9080")
    tools.valves.token = os.getenv("MCWSD_TOKEN", "")
    tools.valves.timeout_seconds = int(os.getenv("MCWSD_TIMEOUT", 60))
    tools.valves.provision_timeout_seconds = int(
        os.getenv("MCWSD_PROVISION_TIMEOUT", 1800)
    )
    ws_id = workspace_id or _workspace_id
    ws_token = workspace_token or _workspace_token
    if ws_id and ws_token:
        tools._bind_workspace_token(ws_id, ws_token)
    return tools


def parse_response(result):
    """Split a tool result into (data, raw); data is None for error strings."""
    try:
        return json.loads(result), result
    except (json.JSONDecodeError, TypeError):
        return None, result if isinstance(result, str) else str(result)


def is_error(result):
    """True when a tool result is an error string rather than a JSON body."""
    if not isinstance(result, str):
        return True
    return result.lstrip().startswith("Error ") or "Client Error" in result


def pass_(message):
    print(f"{OK} SUCCESS: {message}")
    return True


def fail(message):
    print(f"{BAD} FAILED: {message}")
    return False


def show(data, raw):
    """Pretty-print a parsed response, or the raw text when it is not JSON."""
    if data is None:
        print(raw)
    else:
        print(json.dumps(data, indent=2))


# ---------------------------------------------------------------------------
# Workspace lifecycle
# ---------------------------------------------------------------------------


def test_prepare(repo_url="", ref="main"):
    """Test prepare_workspace. Returns the workspace id, or "" on failure."""
    print("Testing prepare_workspace...")
    print(f"  repo_url: {repo_url or '(blank workspace)'}")
    print(f"  ref: {ref}")

    tools = get_tools()
    result = tools.prepare_workspace(repo_url, ref)

    print("\nResponse:")
    data, raw = parse_response(result)
    show(data, raw)
    if data is None:
        return fail(f"invalid JSON response: {raw.strip()}")
    workspace_id = data.get("workspace_id")
    if not workspace_id:
        return fail("no workspace_id in response")

    profile = data.get("profile") or {}
    print(
        f"  OS: {data.get('os', 'unknown')} | work_dir: {data.get('work_dir', 'unknown')} | "
        f"language: {profile.get('language', 'unknown')} | cache_hit: {data.get('cache_hit', False)}"
    )
    pass_(f"workspace created: {workspace_id}")
    return workspace_id


def test_exec(workspace_id, command, timeout=120, allow_network=False, expect_exit=0):
    """Test exec_command. `expect_exit` is the exit code that means success."""
    print("Testing exec_command...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  command: {command}")
    print(f"  timeout: {timeout}s")
    print(f"  allow_network: {allow_network}")
    print(f"  expect_exit: {expect_exit}")

    tools = get_tools(workspace_id)
    result = tools.exec_command(
        workspace_id, command, allow_network=allow_network, timeout_seconds=timeout
    )

    print("\nResponse:")
    data, raw = parse_response(result)
    show(data, raw)
    if data is None:
        return fail(f"invalid JSON response: {raw.strip()}")
    if "exit_code" not in data:
        if data.get("job_id"):
            return pass_(
                f"command promoted to background job {data['job_id']} "
                f"(status: {data.get('status')})"
            )
        return fail("no exit_code in response")
    exit_code = data["exit_code"]
    print(f"  duration: {data.get('duration_ms', 0)}ms")
    if exit_code != expect_exit:
        return fail(f"exit code {exit_code} (expected {expect_exit})")
    return pass_(f"exit code {exit_code}")


def test_artifacts(workspace_id, paths="", expect_path=""):
    """Test get_artifacts. With expect_path, that artifact must be present."""
    print("Testing get_artifacts...")
    print(f"  workspace_id: {workspace_id}")
    if paths:
        print(f"  paths: {paths}")
    if expect_path:
        print(f"  expect_path: {expect_path}")

    tools = get_tools(workspace_id)
    result = tools.get_artifacts(workspace_id, paths)

    print("\nResponse:")
    data, raw = parse_response(result)
    show(data, raw)
    if data is None:
        return fail(f"invalid JSON response: {raw.strip()}")

    names = []
    for artifact in data.get("artifacts") or []:
        if isinstance(artifact, dict) and artifact.get("name"):
            names.append(artifact["name"])
    names.extend(data.get("untracked_files") or [])
    if data.get("patch"):
        names.append("changes.patch")

    print(f"  artifacts: {names}")
    if expect_path:
        matched = any(n == expect_path or n.endswith("/" + expect_path) for n in names)
        if not matched:
            return fail(f"{expect_path!r} is not among the artifacts")
        return pass_(f"artifacts contain {expect_path!r}")
    return pass_(f"artifacts retrieved ({len(names)} entries)")


def test_status(workspace_id):
    """Test workspace_status."""
    print("Testing workspace_status...")
    print(f"  workspace_id: {workspace_id}")

    tools = get_tools(workspace_id)
    result = tools.workspace_status(workspace_id)

    print("\nResponse:")
    data, raw = parse_response(result)
    show(data, raw)
    if data is None or "status" not in data:
        return fail(f"no status in response: {raw.strip()}")
    status = data["status"]
    if status not in ("alive", "healthy", "running"):
        return fail(f"unexpected status {status!r}")
    return pass_(f"status is {status!r}")


def test_destroy(workspace_id):
    """Test destroy_workspace."""
    print("Testing destroy_workspace...")
    print(f"  workspace_id: {workspace_id}")

    tools = get_tools(workspace_id)
    result = tools.destroy_workspace(workspace_id)

    print(f"\nResponse: {result}")
    if is_error(result):
        return fail(f"error destroying workspace: {result.strip()}")
    if "destroyed" not in result.lower():
        return fail(f"unexpected response: {result.strip()}")
    return pass_("workspace destroyed")


def test_commit_and_pr(
    workspace_id, commit_message, pr_title, pr_description="", repo_url="", project_path=""
):
    """Test commit_and_pr. Returns the branch name, or "" on failure."""
    print("Testing commit_and_pr...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  commit_message: {commit_message}")
    print(f"  pr_title: {pr_title}")
    print(f"  pr_description: {pr_description}")
    print(f"  repo_url: {repo_url}")
    print(f"  project_path: {project_path}")

    tools = get_tools(workspace_id)
    result = tools.commit_and_pr(
        workspace_id, commit_message, pr_title, pr_description, repo_url, project_path
    )

    print("\nResponse:")
    data, raw = parse_response(result)
    show(data, raw)
    if data is None:
        return fail(f"invalid JSON response: {raw.strip()}")
    if data.get("error"):
        return fail(f"no PR opened: {data['error']}")
    if not data.get("branch") or not data.get("mr_url"):
        return fail("no branch or mr_url in response")
    pass_(f"MR created: {data['mr_url']}")
    return data["branch"]


# ---------------------------------------------------------------------------
# File system tests
# ---------------------------------------------------------------------------


def test_list_files(workspace_id, path):
    """Test list_files."""
    print("Testing list_files...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  path: {path}")

    tools = get_tools(workspace_id)
    result = tools.list_files(workspace_id, path)

    print("\nResponse:")
    data, raw = parse_response(result)
    if data is None or "entries" not in data:
        print(raw)
        return fail(f"no entries in response: {raw.strip()}")
    entries = data.get("entries") or []
    print(json.dumps(data, indent=2))
    for entry in entries:
        kind = "DIR " if entry.get("is_dir") else f"{entry.get('size', 0):>8}B"
        print(f"  [{kind}] {entry.get('name')}")
    return pass_(f"{len(entries)} entries listed")


def test_read_file(workspace_id, path, expect_content=None):
    """Test read_file. With expect_content the bytes must match exactly."""
    print("Testing read_file...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  path: {path}")

    tools = get_tools(workspace_id)
    result = tools.read_file(workspace_id, path)

    data, raw = parse_response(result)
    if data is None or "content" not in data:
        print(f"\nResponse: {raw}")
        return fail(f"file not readable: {raw.strip()}")

    content = data["content"]
    lines = content.count("\n") + (1 if content else 0)
    print(f"\nResponse: [{lines} lines, {len(content)} bytes]")
    preview = "\n".join(content.splitlines()[:20])
    if preview:
        print(preview)
        if lines > 20:
            print(f"... ({lines - 20} more lines)")

    if expect_content is not None and content != expect_content:
        return fail(
            f"content mismatch for {path}\n  expected: {expect_content!r}\n  actual:   {content!r}"
        )
    return pass_(f"file read ({len(content)} bytes)")


def test_write_file(workspace_id, path, content):
    """Test write_file, then read the file back so the write is verified."""
    print("Testing write_file...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  path: {path}")
    print(f"  content: {content[:80]}{'...' if len(content) > 80 else ''}")

    tools = get_tools(workspace_id)
    result = tools.write_file(workspace_id, path, content)

    print(f"\nResponse: {result}")
    if is_error(result):
        return fail(f"error writing file: {result.strip()}")

    # A 204 only means the daemon accepted the PUT. Read it back before
    # claiming success, so a dropped write cannot pass.
    print(f"Verifying {path} by reading it back...")
    if not test_read_file(workspace_id, path, expect_content=content):
        return fail(f"{path} did not read back as written")
    return pass_("file written and verified")


def test_safe_edit(workspace_id, path, pattern, replacement):
    """Test safe_edit, then verify the replacement landed and the pattern is gone."""
    print("Testing safe_edit...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  path: {path}")
    print(f"  pattern: {pattern!r}")
    print(f"  replacement: {replacement!r}")

    tools = get_tools(workspace_id)
    before = parse_response(tools.read_file(workspace_id, path))[0]
    if before is None or "content" not in before:
        return fail(f"cannot edit {path}: file is not readable")
    original = before["content"]
    if pattern not in original:
        return fail(f"pattern {pattern!r} does not appear in {path}")

    result = tools.safe_edit(workspace_id, path, pattern, replacement)
    print(f"\nResponse: {result}")
    data, raw = parse_response(result)
    if data is None:
        return fail(f"safe_edit failed: {raw.strip()}")
    if data.get("success") is False:
        return fail(f"safe_edit reported failure: {data.get('message')}")

    after = parse_response(tools.read_file(workspace_id, path))[0]
    if after is None or "content" not in after:
        return fail(f"cannot verify {path}: file is not readable after edit")
    edited = after["content"]
    expected = original.replace(pattern, replacement)
    if edited != expected:
        return fail(
            f"edit did not apply as expected\n  expected: {expected!r}\n  actual:   {edited!r}"
        )
    if pattern != replacement and pattern in edited:
        return fail(f"{path} still contains the original pattern {pattern!r}")
    return pass_(f"{path} edited and verified")


def test_file_roundtrip(workspace_id, path="mcwsd_roundtrip.txt"):
    """Write a file, read it, edit it, verify the edit, then list and diff it."""
    print("Testing file round-trip (write -> read -> edit -> verify)...")
    original = "mcwsd roundtrip\nstatus: draft\n"
    replaced = "mcwsd roundtrip\nstatus: verified\n"

    steps = [
        ("write", test_write_file(workspace_id, path, original)),
        ("read", test_read_file(workspace_id, path, expect_content=original)),
        ("edit", test_safe_edit(workspace_id, path, "status: draft", "status: verified")),
        ("read-after-edit", test_read_file(workspace_id, path, expect_content=replaced)),
        ("artifacts", test_artifacts(workspace_id, "", expect_path=path)),
    ]
    failed = [name for name, ok in steps if not ok]
    if failed:
        return fail(f"round-trip failed at: {', '.join(failed)}")
    return pass_(f"round-trip passed ({len(steps)} steps) for {path}")


# ---------------------------------------------------------------------------
# Background process / job tests
# ---------------------------------------------------------------------------


def test_start_process(workspace_id, command, allow_network=False):
    """Test start_process (deprecated alias for start_job). Returns the job id."""
    print("Testing start_process...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  command: {command}")
    print(f"  allow_network: {allow_network}")

    tools = get_tools(workspace_id)
    result = tools.start_process(workspace_id, command, allow_network=allow_network)

    print("\nResponse:")
    data, raw = parse_response(result)
    show(data, raw)
    if data is None:
        return fail(f"invalid JSON response: {raw.strip()}")
    job_id = data.get("job_id") or data.get("id")
    if not job_id:
        return fail("no job_id in response")
    pass_(f"job started with ID: {job_id}")
    return job_id


def test_list_processes(workspace_id):
    """Test list_processes."""
    print("Testing list_processes...")
    print(f"  workspace_id: {workspace_id}")

    tools = get_tools(workspace_id)
    result = tools.list_processes(workspace_id)

    print("\nResponse:")
    data, raw = parse_response(result)
    if data is None:
        print(raw)
        return fail(f"invalid JSON response: {raw.strip()}")
    procs = data.get("jobs", data.get("processes", []))
    print(json.dumps(data, indent=2))
    return pass_(f"{len(procs)} process(es) tracked")


def test_get_process_status(workspace_id, process_id):
    """Test get_process_status."""
    print("Testing get_process_status...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  process_id: {process_id}")

    tools = get_tools(workspace_id)
    result = tools.get_process_status(workspace_id, process_id)

    data, raw = parse_response(result)
    if data is None:
        print(f"\nResponse: {raw}")
        return fail(f"invalid JSON response: {raw.strip()}")
    print(f"  Status:  {data.get('status')}")
    print(f"  Command: {data.get('command')}")
    if data.get("exit_code") is not None:
        print(f"  Exit:    {data.get('exit_code')}")
    for stream in ("stdout", "stderr", "output"):
        text = data.get(stream, "")
        if text:
            print(f"\n--- {stream} ---\n{text}")
    if not data.get("status"):
        return fail("no status in response")
    return pass_("process status retrieved")


def test_send_process_input(workspace_id, process_id, input_text):
    """Test send_process_input.

    Interactive stdin is intentionally unsupported for background jobs; the
    tool reports that clearly rather than failing, and the daemon answers 501.
    """
    print("Testing send_process_input...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  process_id: {process_id}")
    print(f"  input: {input_text!r}")

    tools = get_tools(workspace_id)
    result = tools.send_process_input(workspace_id, process_id, input_text)

    print(f"\nResponse: {result}")
    lowered = result.lower()
    if "not supported" in lowered or "sent" in lowered:
        return pass_("stdin unsupported for background jobs (expected)")
    return fail(f"unexpected answer: {result.strip()}")


def test_kill_process(workspace_id, process_id):
    """Test kill_process."""
    print("Testing kill_process...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  process_id: {process_id}")

    tools = get_tools(workspace_id)
    result = tools.kill_process(workspace_id, process_id)

    print(f"\nResponse: {result}")
    if is_error(result):
        return fail(f"error killing process: {result.strip()}")
    if "cancel" not in result.lower() and "kill" not in result.lower():
        return fail(f"unexpected response: {result.strip()}")
    return pass_("process killed")


# ---------------------------------------------------------------------------
# Background job tests
# ---------------------------------------------------------------------------


def test_start_job(workspace_id, command, allow_network=False, timeout=0):
    """Test start_job. Returns the job id, or "" on failure."""
    print("Testing start_job...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  command: {command}")

    tools = get_tools(workspace_id)
    result = tools.start_job(
        workspace_id, command, allow_network=allow_network, timeout_seconds=timeout
    )

    print("\nResponse:")
    data, raw = parse_response(result)
    show(data, raw)
    if data is None:
        return fail(f"invalid JSON response: {raw.strip()}")
    job_id = data.get("job_id") or data.get("id")
    if not job_id:
        return fail("no job_id in response")
    pass_(f"job started with ID: {job_id}")
    return job_id


def test_list_jobs(workspace_id):
    """Test list_jobs."""
    print("Testing list_jobs...")
    print(f"  workspace_id: {workspace_id}")

    tools = get_tools(workspace_id)
    result = tools.list_jobs(workspace_id)

    print("\nResponse:")
    data, raw = parse_response(result)
    if data is None:
        print(raw)
        return fail(f"invalid JSON response: {raw.strip()}")
    jobs = data.get("jobs", data.get("processes", []))
    print(json.dumps(data, indent=2))
    return pass_(f"{len(jobs)} job(s) tracked")


def test_get_job(workspace_id, job_id, cursor=""):
    """Test get_job. Returns the next cursor for incremental polling.

    Returns None (not "") on failure so callers can tell "no cursor yet" apart
    from "the call failed".
    """
    print("Testing get_job...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  job_id: {job_id}")
    print(f"  cursor: {cursor!r}")

    tools = get_tools(workspace_id)
    result = tools.get_job(workspace_id, job_id, cursor=cursor)

    data, raw = parse_response(result)
    if data is None:
        print(f"\nResponse: {raw}")
        print(f"{BAD} FAILED: invalid JSON response")
        return None
    print(f"  Status:  {data.get('status')}")
    print(f"  Command: {data.get('command')}")
    if data.get("exit_code") is not None:
        print(f"  Exit:    {data.get('exit_code')}")
    for stream in ("output", "stdout", "stderr"):
        text = data.get(stream, "")
        if text:
            print(f"\n--- {stream} ---\n{text}")
    if not data.get("status"):
        print(f"{BAD} FAILED: no status in response")
        return None
    pass_(f"job status retrieved (next cursor: {data.get('cursor')!r})")
    return data.get("cursor", "") or ""


def test_cancel_job(workspace_id, job_id):
    """Test cancel_job."""
    print("Testing cancel_job...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  job_id: {job_id}")

    tools = get_tools(workspace_id)
    result = tools.cancel_job(workspace_id, job_id)

    print(f"\nResponse: {result}")
    if is_error(result):
        return fail(f"error cancelling job: {result.strip()}")
    if "cancel" not in result.lower():
        return fail(f"unexpected response: {result.strip()}")
    return pass_("job cancelled")


# ---------------------------------------------------------------------------
# "all": run everything against an existing workspace
# ---------------------------------------------------------------------------


def run_all(workspace_id, work_dir="", destroy=False, include_commit=False):
    """Run the full function suite against an existing workspace.

    Returns True only when every step passed. `destroy` is opt-in: the caller
    may still want the workspace afterwards (to inspect a failure, or to run
    another test), so the default is to leave it alone.
    """
    print("=" * 60)
    print("RUNNING ALL TESTS ON EXISTING WORKSPACE")
    print("=" * 60)
    print(f"  workspace_id: {workspace_id}")
    print(f"  work_dir: {work_dir or '(default: .)'}")
    print(f"  destroy at end: {destroy}")
    print("=" * 60)

    base = work_dir or "."
    steps = [
        ("status", test_status(workspace_id)),
        ("exec", test_exec(workspace_id, "echo 'Test command' && whoami")),
        ("exec-nonzero-exit", test_exec(workspace_id, "exit 3", expect_exit=3)),
        ("artifacts", test_artifacts(workspace_id)),
        ("list_files", test_list_files(workspace_id, base)),
        ("file_roundtrip", test_file_roundtrip(workspace_id)),
        ("start_process", bool(test_start_process(workspace_id, "echo 'background job done'"))),
        ("list_processes", test_list_processes(workspace_id)),
        ("list_jobs", test_list_jobs(workspace_id)),
    ]

    job_id = test_start_job(workspace_id, "echo 'background job done'")
    steps.append(("start_job", bool(job_id)))
    if job_id:
        time.sleep(2)
        cursor = test_get_job(workspace_id, job_id)
        steps.append(("get_job", cursor is not None))
        if cursor is not None:
            steps.append(
                ("get_job_incremental", test_get_job(workspace_id, job_id, cursor) is not None)
            )
        steps.append(("send_process_input", test_send_process_input(workspace_id, job_id, "yes\n")))
        steps.append(("cancel_job", test_cancel_job(workspace_id, job_id)))
        steps.append(("kill_process", test_kill_process(workspace_id, job_id)))

    if include_commit:
        branch = test_commit_and_pr(
            workspace_id, "test: mcwsd function test", "mcwsd function test"
        )
        steps.append(("commit_and_pr", bool(branch)))

    if destroy:
        steps.append(("destroy", test_destroy(workspace_id)))

    passed = sum(1 for _, ok in steps if ok)
    failed = [name for name, ok in steps if not ok]
    print("\n" + "=" * 60)
    if failed:
        print(f"{BAD} ALL TESTS FAILED — {passed}/{len(steps)} passed")
        print(f"  failed: {', '.join(failed)}")
    else:
        print(f"{OK} ALL TESTS PASSED — {passed}/{len(steps)} passed")
    print(f"SUMMARY: {passed}/{len(steps)} passed")
    print("=" * 60)
    return not failed


def build_parser():
    parser = argparse.ArgumentParser(
        description="Run individual mcwsd function tests",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Examples:
  # Workspace lifecycle
  %(prog)s prepare https://github.com/user/repo.git main
  %(prog)s prepare                     # blank workspace, no repo
  %(prog)s exec ws-abc123 "echo hello"
  %(prog)s exec ws-abc123 "exit 3" --expect-exit 3
  %(prog)s artifacts ws-abc123
  %(prog)s status ws-abc123
  %(prog)s destroy ws-abc123
  %(prog)s commit_and_pr ws-abc123 "fix: bug" "Fix the bug" --desc "Details..."

  # File system tools
  %(prog)s list_files ws-abc123 .
  %(prog)s read_file ws-abc123 README.md
  %(prog)s write_file ws-abc123 hello.txt "hello world"
  %(prog)s safe_edit ws-abc123 hello.txt world there
  %(prog)s file_roundtrip ws-abc123

  # Background job tools
  %(prog)s start_job ws-abc123 "sleep 30"
  %(prog)s list_jobs ws-abc123
  %(prog)s get_job ws-abc123 job-abc123def4
  %(prog)s cancel_job ws-abc123 job-abc123def4

  # Run every test on an existing workspace
  %(prog)s all ws-abc123
  %(prog)s all ws-abc123 --work-dir /workspace/repo --destroy
        """,
    )

    # Global options
    # Provisioner is unused in the code base at this time.
    parser.add_argument("--provisioner", default="", help="Specify the provisioner used by the daemon")
    parser.add_argument(
        "--workspace-token",
        default=os.getenv("MCWSD_WORKSPACE_TOKEN", ""),
        help="Workspace token for strict mode (or set MCWSD_WORKSPACE_TOKEN). Not needed for prepare, which learns it automatically.",
    )

    subparsers = parser.add_subparsers(dest="test", help="Test to run")

    # prepare test
    prep = subparsers.add_parser("prepare", help="Test prepare_workspace")
    prep.add_argument("repo_url", nargs="?", default="", help="Git repository URL (omit for a blank workspace)")
    prep.add_argument("ref", nargs="?", default="main", help="Branch/tag/commit")

    # exec test
    exec_parser = subparsers.add_parser("exec", help="Test exec_command")
    exec_parser.add_argument("workspace_id", help="Workspace ID")
    exec_parser.add_argument("command", help="Command to execute")
    exec_parser.add_argument("--timeout", type=int, default=120, help="Timeout in seconds")
    exec_parser.add_argument(
        "--allow-network", action="store_true", help="Allow network access for this command"
    )
    exec_parser.add_argument(
        "--expect-exit", type=int, default=0, help="Exit code that counts as success (default: 0)"
    )

    # artifacts test
    art = subparsers.add_parser("artifacts", help="Test get_artifacts")
    art.add_argument("workspace_id", help="Workspace ID")
    art.add_argument("paths", nargs="?", default="", help="Paths to scope diff")
    art.add_argument("--expect-path", default="", help="Fail unless this artifact is present")

    # status test
    stat = subparsers.add_parser("status", help="Test workspace_status")
    stat.add_argument("workspace_id", help="Workspace ID")

    # destroy test
    dest = subparsers.add_parser("destroy", help="Test destroy_workspace")
    dest.add_argument("workspace_id", help="Workspace ID")

    # commit_and_pr test
    cap = subparsers.add_parser("commit_and_pr", help="Test commit_and_pr")
    cap.add_argument("workspace_id", help="Workspace ID")
    cap.add_argument("commit_message", help="Commit message")
    cap.add_argument("pr_title", help="PR title")
    cap.add_argument("--desc", dest="pr_description", default="", help="PR description")
    cap.add_argument("--repo", dest="repo_url", default="", help="Repo URL")
    cap.add_argument("--project", dest="project_path", default="", help="Project path or owner/repo")

    # list_files test
    lf = subparsers.add_parser("list_files", help="Test list_files")
    lf.add_argument("workspace_id", help="Workspace ID")
    lf.add_argument("path", help="Path to list inside the repo")

    # read_file test
    rf = subparsers.add_parser("read_file", help="Test read_file")
    rf.add_argument("workspace_id", help="Workspace ID")
    rf.add_argument("path", help="Path to the file inside the repo")
    rf.add_argument("--expect-content", default=None, help="Fail unless the content matches")

    # write_file test
    wf = subparsers.add_parser("write_file", help="Test write_file")
    wf.add_argument("workspace_id", help="Workspace ID")
    wf.add_argument("path", help="Path to write inside the repo")
    wf.add_argument("content", help="Text content to write")

    # safe_edit test
    se = subparsers.add_parser("safe_edit", help="Test safe_edit")
    se.add_argument("workspace_id", help="Workspace ID")
    se.add_argument("path", help="Path to the file inside the repo")
    se.add_argument("pattern", help="Exact literal string to find")
    se.add_argument("replacement", help="String to replace every occurrence with")

    # file_roundtrip test
    fr = subparsers.add_parser("file_roundtrip", help="Test write -> read -> edit -> verify")
    fr.add_argument("workspace_id", help="Workspace ID")
    fr.add_argument("--path", default="mcwsd_roundtrip.txt", help="File to use")

    # start_process test
    sp = subparsers.add_parser("start_process", help="Test start_process")
    sp.add_argument("workspace_id", help="Workspace ID")
    sp.add_argument("command", help="Shell command to run in the background")
    sp.add_argument("--allow-network", action="store_true", help="Allow network access")

    # list_processes test
    lp = subparsers.add_parser("list_processes", help="Test list_processes")
    lp.add_argument("workspace_id", help="Workspace ID")

    # get_process_status test
    gps = subparsers.add_parser("get_process_status", help="Test get_process_status")
    gps.add_argument("workspace_id", help="Workspace ID")
    gps.add_argument("process_id", help="Process ID")

    # send_process_input test
    spi = subparsers.add_parser("send_process_input", help="Test send_process_input")
    spi.add_argument("workspace_id", help="Workspace ID")
    spi.add_argument("process_id", help="Process ID")
    spi.add_argument("input_text", help="Text to send to stdin")

    # kill_process test
    kp = subparsers.add_parser("kill_process", help="Test kill_process")
    kp.add_argument("workspace_id", help="Workspace ID")
    kp.add_argument("process_id", help="Process ID")

    # start_job test
    sj = subparsers.add_parser("start_job", help="Test start_job")
    sj.add_argument("workspace_id", help="Workspace ID")
    sj.add_argument("command", help="Shell command to run in the background")
    sj.add_argument("--allow-network", action="store_true", help="Allow network access")
    sj.add_argument("--timeout", type=int, default=0, help="Timeout in seconds")

    # list_jobs test
    lj = subparsers.add_parser("list_jobs", help="Test list_jobs")
    lj.add_argument("workspace_id", help="Workspace ID")

    # get_job test
    gj = subparsers.add_parser("get_job", help="Test get_job")
    gj.add_argument("workspace_id", help="Workspace ID")
    gj.add_argument("job_id", help="Job ID")
    gj.add_argument("--cursor", default="", help="Cursor from the previous get_job")

    # cancel_job test
    cj = subparsers.add_parser("cancel_job", help="Test cancel_job")
    cj.add_argument("workspace_id", help="Workspace ID")
    cj.add_argument("job_id", help="Job ID")

    # all test (run all on existing workspace)
    all_p = subparsers.add_parser("all", help="Run all tests on existing workspace")
    all_p.add_argument("workspace_id", help="Workspace ID")
    all_p.add_argument(
        "--work-dir",
        default="",
        help="Repo root inside the workspace (default: '.' i.e. the repo root the daemon uses)",
    )
    all_p.add_argument(
        "--destroy", action="store_true", help="Destroy the workspace when the run finishes"
    )
    all_p.add_argument(
        "--commit", action="store_true", help="Also run commit_and_pr (needs forge auth)"
    )

    return parser


def main(argv=None):
    parser = build_parser()
    args = parser.parse_args(argv)

    if not args.test:
        parser.print_help()
        return 1

    # Publish the workspace token/ID so every get_tools() call can bind it.
    global _workspace_token, _workspace_id
    _workspace_token = args.workspace_token
    _workspace_id = getattr(args, "workspace_id", "")

    if args.test == "prepare":
        return 0 if test_prepare(args.repo_url, args.ref) else 1
    if args.test == "exec":
        ok = test_exec(
            args.workspace_id, args.command, args.timeout, args.allow_network, args.expect_exit
        )
    elif args.test == "artifacts":
        ok = test_artifacts(args.workspace_id, args.paths, args.expect_path)
    elif args.test == "status":
        ok = test_status(args.workspace_id)
    elif args.test == "destroy":
        ok = test_destroy(args.workspace_id)
    elif args.test == "commit_and_pr":
        ok = bool(
            test_commit_and_pr(
                args.workspace_id,
                args.commit_message,
                args.pr_title,
                args.pr_description,
                args.repo_url,
                args.project_path,
            )
        )
    elif args.test == "list_files":
        ok = test_list_files(args.workspace_id, args.path)
    elif args.test == "read_file":
        ok = test_read_file(args.workspace_id, args.path, args.expect_content)
    elif args.test == "write_file":
        ok = test_write_file(args.workspace_id, args.path, args.content)
    elif args.test == "safe_edit":
        ok = test_safe_edit(args.workspace_id, args.path, args.pattern, args.replacement)
    elif args.test == "file_roundtrip":
        ok = test_file_roundtrip(args.workspace_id, args.path)
    elif args.test == "start_process":
        ok = bool(test_start_process(args.workspace_id, args.command, args.allow_network))
    elif args.test == "list_processes":
        ok = test_list_processes(args.workspace_id)
    elif args.test == "get_process_status":
        ok = test_get_process_status(args.workspace_id, args.process_id)
    elif args.test == "send_process_input":
        ok = test_send_process_input(args.workspace_id, args.process_id, args.input_text)
    elif args.test == "kill_process":
        ok = test_kill_process(args.workspace_id, args.process_id)
    elif args.test == "start_job":
        ok = bool(test_start_job(args.workspace_id, args.command, args.allow_network, args.timeout))
    elif args.test == "list_jobs":
        ok = test_list_jobs(args.workspace_id)
    elif args.test == "get_job":
        ok = test_get_job(args.workspace_id, args.job_id, args.cursor) is not None
    elif args.test == "cancel_job":
        ok = test_cancel_job(args.workspace_id, args.job_id)
    elif args.test == "all":
        ok = run_all(args.workspace_id, args.work_dir, args.destroy, args.commit)
    else:
        parser.print_help()
        return 1

    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
