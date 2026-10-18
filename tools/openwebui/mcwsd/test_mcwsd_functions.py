#!/usr/bin/env python3
"""
Individual CLI tools for testing mcwsd functions.
Each tool can be run independently with its specific arguments.
"""

import sys
import json
import argparse
from pathlib import Path

# Add parent directory to path for imports
sys.path.insert(0, str(Path(__file__).parent))

from mcwsd import Tools


def get_tools():
    """Initialize tools with default valves."""
    tools = Tools()
    return tools


def test_prepare(repo_url, ref="main"):
    """Test prepare_workspace function."""
    print(f"Testing prepare_workspace...")
    print(f"  repo_url: {repo_url}")
    print(f"  ref: {ref}")

    tools = get_tools()
    result = tools.prepare_workspace(repo_url, ref)

    print("\nResponse:")
    try:
        data = json.loads(result)
        print(json.dumps(data, indent=2))
        if "workspace_id" in data:
            print(f"\n✓ SUCCESS: Workspace created: {data['workspace_id']}")
            return data["workspace_id"]
        else:
            print("\n✗ FAILED: No workspace_id in response")
            return None
    except json.JSONDecodeError:
        print(result)
        print("\n✗ FAILED: Invalid JSON response")
        return None


def test_exec(workspace_id, command, timeout=120, allow_network=False):
    """Test exec_command function."""
    print(f"Testing exec_command...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  command: {command}")
    print(f"  timeout: {timeout}s")
    print(f"  allow_network: {allow_network}")

    tools = get_tools()
    result = tools.exec_command(
        workspace_id, command, allow_network=allow_network, timeout_seconds=timeout
    )

    print("\nResponse:")
    try:
        data = json.loads(result)
        print(json.dumps(data, indent=2))
        if "exit_code" in data:
            print(f"\n✓ SUCCESS: Exit code {data['exit_code']}")
            return True
        else:
            print("\n✗ FAILED: No exit_code in response")
            return False
    except json.JSONDecodeError:
        print(result)
        print("\n✗ FAILED: Invalid JSON response")
        return False


def test_artifacts(workspace_id, paths=""):
    """Test get_artifacts function."""
    print(f"Testing get_artifacts...")
    print(f"  workspace_id: {workspace_id}")
    if paths:
        print(f"  paths: {paths}")

    tools = get_tools()
    result = tools.get_artifacts(workspace_id, paths)

    print("\nResponse:")
    try:
        data = json.loads(result)
        print(json.dumps(data, indent=2))
        if "patch" in data or "untracked_files" in data or "artifacts" in data:
            print(f"\n✓ SUCCESS: Artifacts retrieved")
            return True
        else:
            print(f"\n✓ SUCCESS: No changes (empty artifacts)")
            return True
    except json.JSONDecodeError:
        print(result)
        print("\n✗ FAILED: Invalid JSON response")
        return False


def test_status(workspace_id):
    """Test workspace_status function."""
    print(f"Testing workspace_status...")
    print(f"  workspace_id: {workspace_id}")

    tools = get_tools()
    result = tools.workspace_status(workspace_id)

    print("\nResponse:")
    try:
        data = json.loads(result)
        print(json.dumps(data, indent=2))
        if "status" in data:
            print(f"\n✓ SUCCESS: Status is '{data['status']}'")
            return True
        else:
            print("\n✗ FAILED: No status in response")
            return False
    except json.JSONDecodeError:
        print(result)
        print("\n✗ FAILED: Invalid JSON response")
        return False


def test_destroy(workspace_id):
    """Test destroy_workspace function."""
    print(f"Testing destroy_workspace...")
    print(f"  workspace_id: {workspace_id}")

    tools = get_tools()
    result = tools.destroy_workspace(workspace_id)

    print(f"\nResponse: {result}")
    if "destroyed" in result.lower() or "error" not in result.lower():
        print("✓ SUCCESS: Workspace destroyed")
        return True
    else:
        print("✗ FAILED: Error destroying workspace")
        return False


def commit_and_pr(workspace_id, commit_message, pr_title, pr_description="", repo_url="", project_path=""):
    """Test commit_and_pr function."""
    print(f"Testing commit_and_pr...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  commit_message: {commit_message}")
    print(f"  pr_title: {pr_title}")
    print(f"  pr_description: {pr_description}")
    print(f"  repo_url: {repo_url}")
    print(f"  project_path: {project_path}")

    tools = get_tools()
    result = tools.commit_and_pr(workspace_id, commit_message, pr_title, pr_description, repo_url, project_path)

    print("\nResponse:")
    try:
        data = json.loads(result)
        print(json.dumps(data, indent=2))
        if "branch" in data and "mr_url" in data:
            print(f"\n✓ SUCCESS: MR created: {data['mr_url']}")
            return data["branch"]
        else:
            print("\n✗ FAILED: No branch or mr_url in response")
            return None
    except json.JSONDecodeError:
        print(result)
        print("\n✗ FAILED: Invalid JSON response")
        return None


# --- File System Tool Tests ---

def test_list_files(workspace_id, path):
    """Test list_files function."""
    print(f"Testing list_files...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  path: {path}")

    tools = get_tools()
    result = tools.list_files(workspace_id, path)

    print("\nResponse:")
    try:
        data = json.loads(result)
        entries = data.get("entries", [])
        print(json.dumps(data, indent=2))
        print(f"\n✓ SUCCESS: {len(entries)} entries listed")
        return True
    except json.JSONDecodeError:
        print(result)
        print("\n✗ FAILED: Invalid JSON response")
        return False


def test_read_file(workspace_id, path):
    """Test read_file function."""
    print(f"Testing read_file...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  path: {path}")

    tools = get_tools()
    result = tools.read_file(workspace_id, path)

    print("\nResponse:")
    try:
        data = json.loads(result)
        content = data.get("content", "")
        lines = content.count("\n") + (1 if content else 0)
        print(f"[{lines} lines, {len(content)} bytes]")
        # Print first 20 lines only to keep output manageable
        preview = "\n".join(content.splitlines()[:20])
        if preview:
            print(preview)
            if lines > 20:
                print(f"... ({lines - 20} more lines)")
        print(f"\n✓ SUCCESS: File read ({len(content)} bytes)")
        return True
    except json.JSONDecodeError:
        print(result)
        print("\n✗ FAILED: Invalid JSON response")
        return False


def test_write_file(workspace_id, path, content):
    """Test write_file function."""
    print(f"Testing write_file...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  path: {path}")
    print(f"  content: {content[:80]}{'...' if len(content) > 80 else ''}")

    tools = get_tools()
    result = tools.write_file(workspace_id, path, content)

    print(f"\nResponse: {result}")
    if "written successfully" in result or "error" not in result.lower():
        print("✓ SUCCESS: File written")
        return True
    else:
        print("✗ FAILED: Error writing file")
        return False


# --- Background Process Management Tests ---

def test_start_process(workspace_id, command, allow_network=False):
    """Test start_process (deprecated alias for start_job)."""
    print(f"Testing start_process...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  command: {command}")
    print(f"  allow_network: {allow_network}")

    tools = get_tools()
    result = tools.start_process(workspace_id, command, allow_network=allow_network)

    print("\nResponse:")
    try:
        data = json.loads(result)
        print(json.dumps(data, indent=2))
        job_id = data.get("job_id") or data.get("id")
        if job_id:
            print(f"\n✓ SUCCESS: Job started with ID: {job_id}")
            return job_id
        else:
            print("\n✗ FAILED: No job_id in response")
            return None
    except json.JSONDecodeError:
        print(result)
        print("\n✗ FAILED: Invalid JSON response")
        return None


def test_list_processes(workspace_id):
    """Test list_processes function."""
    print(f"Testing list_processes...")
    print(f"  workspace_id: {workspace_id}")

    tools = get_tools()
    result = tools.list_processes(workspace_id)

    print("\nResponse:")
    try:
        data = json.loads(result)
        procs = data.get("processes", [])
        print(json.dumps(data, indent=2))
        print(f"\n✓ SUCCESS: {len(procs)} process(es) tracked")
        return True
    except json.JSONDecodeError:
        print(result)
        print("\n✗ FAILED: Invalid JSON response")
        return False


def test_get_process_status(workspace_id, process_id):
    """Test get_process_status function."""
    print(f"Testing get_process_status...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  process_id: {process_id}")

    tools = get_tools()
    result = tools.get_process_status(workspace_id, process_id)

    print("\nResponse:")
    try:
        data = json.loads(result)
        print(f"  Status:  {data.get('status')}")
        print(f"  Command: {data.get('command')}")
        if data.get("exit_code") is not None:
            print(f"  Exit:    {data.get('exit_code')}")
        stdout = data.get("stdout", "")
        stderr = data.get("stderr", "")
        if stdout:
            print(f"\n--- stdout ---\n{stdout}")
        if stderr:
            print(f"\n--- stderr ---\n{stderr}")
        print(f"\n✓ SUCCESS: Process status retrieved")
        return True
    except json.JSONDecodeError:
        print(result)
        print("\n✗ FAILED: Invalid JSON response")
        return False


def test_send_process_input(workspace_id, process_id, input_text):
    """Test send_process_input function."""
    print(f"Testing send_process_input...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  process_id: {process_id}")
    print(f"  input: {input_text!r}")

    tools = get_tools()
    result = tools.send_process_input(workspace_id, process_id, input_text)

    print(f"\nResponse: {result}")
    # Interactive stdin is intentionally unsupported for background jobs; the
    # method reports that clearly rather than failing.
    if "not supported" in result.lower() or "sent" in result.lower():
        print("✓ SUCCESS: Input is unsupported (expected)")
        return True
    else:
        print("✗ FAILED: Error sending input")
        return False


def test_kill_process(workspace_id, process_id):
    """Test kill_process function."""
    print(f"Testing kill_process...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  process_id: {process_id}")

    tools = get_tools()
    result = tools.kill_process(workspace_id, process_id)

    print(f"\nResponse: {result}")
    if "killed" in result.lower() or "error" not in result.lower():
        print("✓ SUCCESS: Process killed")
        return True
    else:
        print("✗ FAILED: Error killing process")
        return False


# --- Background Job Tests ---

def test_start_job(workspace_id, command, allow_network=False, timeout=0):
    """Test start_job function."""
    print(f"Testing start_job...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  command: {command}")

    tools = get_tools()
    result = tools.start_job(
        workspace_id, command, allow_network=allow_network, timeout_seconds=timeout
    )

    print("\nResponse:")
    try:
        data = json.loads(result)
        print(json.dumps(data, indent=2))
        job_id = data.get("job_id") or data.get("id")
        if job_id:
            print(f"\n✓ SUCCESS: Job started with ID: {job_id}")
            return job_id
        print("\n✗ FAILED: No job_id in response")
        return None
    except json.JSONDecodeError:
        print(result)
        print("\n✗ FAILED: Invalid JSON response")
        return None


def test_list_jobs(workspace_id):
    """Test list_jobs function."""
    print(f"Testing list_jobs...")
    print(f"  workspace_id: {workspace_id}")

    tools = get_tools()
    result = tools.list_jobs(workspace_id)

    print("\nResponse:")
    try:
        data = json.loads(result)
        jobs = data.get("jobs", data.get("processes", []))
        print(json.dumps(data, indent=2))
        print(f"\n✓ SUCCESS: {len(jobs)} job(s) tracked")
        return True
    except json.JSONDecodeError:
        print(result)
        print("\n✗ FAILED: Invalid JSON response")
        return False


def test_get_job(workspace_id, job_id, cursor=""):
    """Test get_job function. Returns the next cursor for incremental polling."""
    print(f"Testing get_job...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  job_id: {job_id}")
    print(f"  cursor: {cursor!r}")

    tools = get_tools()
    result = tools.get_job(workspace_id, job_id, cursor=cursor)

    print("\nResponse:")
    try:
        data = json.loads(result)
        print(f"  Status:  {data.get('status')}")
        print(f"  Command: {data.get('command')}")
        if data.get("exit_code") is not None:
            print(f"  Exit:    {data.get('exit_code')}")
        output = data.get("output", "")
        if output:
            print(f"\n--- output ---\n{output}")
        print(f"\n✓ SUCCESS: Job status retrieved (next cursor: {data.get('cursor')!r})")
        return data.get("cursor", "")
    except json.JSONDecodeError:
        print(result)
        print("\n✗ FAILED: Invalid JSON response")
        return ""


def test_cancel_job(workspace_id, job_id):
    """Test cancel_job function."""
    print(f"Testing cancel_job...")
    print(f"  workspace_id: {workspace_id}")
    print(f"  job_id: {job_id}")

    tools = get_tools()
    result = tools.cancel_job(workspace_id, job_id)

    print(f"\nResponse: {result}")
    if "cancelled" in result.lower() or "error" not in result.lower():
        print("✓ SUCCESS: Job cancelled")
        return True
    print("✗ FAILED: Error cancelling job")
    return False


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description="Run individual mcwsd function tests",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Examples:
  # Existing tools
  %(prog)s prepare https://github.com/user/repo.git main
  %(prog)s exec ws-abc123 "echo hello"
  %(prog)s artifacts ws-abc123
  %(prog)s status ws-abc123
  %(prog)s destroy ws-abc123
  %(prog)s commit_and_pr ws-abc123 "fix: bug" "Fix the bug" "Details..." https://...

  # File system tools
  %(prog)s list_files ws-abc123 /workspace/repo
  %(prog)s read_file ws-abc123 /workspace/repo/README.md
  %(prog)s write_file ws-abc123 /workspace/repo/hello.txt "hello world"

  # Background job tools
  %(prog)s start_job ws-abc123 "sleep 30"
  %(prog)s list_jobs ws-abc123
  %(prog)s get_job ws-abc123 proc-abc123def4
  %(prog)s cancel_job ws-abc123 proc-abc123def4

  # Run all tests on existing workspace
  %(prog)s all ws-abc123
        """,
    )

    # Global options
    parser.add_argument("--provisioner", default="", help="Specify the provisioner used by the daemon")

    subparsers = parser.add_subparsers(dest="test", help="Test to run")

    # prepare test
    prep = subparsers.add_parser("prepare", help="Test prepare_workspace")
    prep.add_argument("repo_url", help="Git repository URL")
    prep.add_argument("ref", nargs="?", default="main", help="Branch/tag/commit")

    # exec test
    exec_parser = subparsers.add_parser("exec", help="Test exec_command")
    exec_parser.add_argument("workspace_id", help="Workspace ID")
    exec_parser.add_argument("command", help="Command to execute")
    exec_parser.add_argument("--timeout", type=int, default=120, help="Timeout in seconds")
    exec_parser.add_argument("--allow-network", action="store_true", help="Allow network access for this command")

    # artifacts test
    art = subparsers.add_parser("artifacts", help="Test get_artifacts")
    art.add_argument("workspace_id", help="Workspace ID")
    art.add_argument("paths", nargs="?", default="", help="Paths to scope diff")

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
    lf.add_argument("path", help="Absolute path to list inside the container")

    # read_file test
    rf = subparsers.add_parser("read_file", help="Test read_file")
    rf.add_argument("workspace_id", help="Workspace ID")
    rf.add_argument("path", help="Absolute path to the file inside the container")

    # write_file test
    wf = subparsers.add_parser("write_file", help="Test write_file")
    wf.add_argument("workspace_id", help="Workspace ID")
    wf.add_argument("path", help="Absolute path to write inside the container")
    wf.add_argument("content", help="Text content to write")

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

    args = parser.parse_args()

    if not args.test:
        parser.print_help()
        sys.exit(1)

    success = True

    if args.test == "prepare":
        test_prepare(args.repo_url, args.ref)
    elif args.test == "exec":
        success = test_exec(args.workspace_id, args.command, args.timeout, args.allow_network)
    elif args.test == "artifacts":
        success = test_artifacts(args.workspace_id, args.paths)
    elif args.test == "status":
        success = test_status(args.workspace_id)
    elif args.test == "destroy":
        success = test_destroy(args.workspace_id)
    elif args.test == "commit_and_pr":
        commit_and_pr(
            args.workspace_id, args.commit_message, args.pr_title,
            args.pr_description, args.repo_url, args.project_path
        )
    elif args.test == "list_files":
        success = test_list_files(args.workspace_id, args.path)
    elif args.test == "read_file":
        success = test_read_file(args.workspace_id, args.path)
    elif args.test == "write_file":
        success = test_write_file(args.workspace_id, args.path, args.content)
    elif args.test == "start_process":
        test_start_process(args.workspace_id, args.command, args.allow_network)
    elif args.test == "list_processes":
        success = test_list_processes(args.workspace_id)
    elif args.test == "get_process_status":
        success = test_get_process_status(args.workspace_id, args.process_id)
    elif args.test == "send_process_input":
        success = test_send_process_input(args.workspace_id, args.process_id, args.input_text)
    elif args.test == "kill_process":
        success = test_kill_process(args.workspace_id, args.process_id)
    elif args.test == "start_job":
        test_start_job(args.workspace_id, args.command, args.allow_network, args.timeout)
    elif args.test == "list_jobs":
        success = test_list_jobs(args.workspace_id)
    elif args.test == "get_job":
        test_get_job(args.workspace_id, args.job_id, args.cursor)
    elif args.test == "cancel_job":
        success = test_cancel_job(args.workspace_id, args.job_id)
    elif args.test == "all":
        print("=" * 60)
        print("RUNNING ALL TESTS ON EXISTING WORKSPACE")
        print("=" * 60)
        test_status(args.workspace_id)
        test_exec(args.workspace_id, "echo 'Test command' && whoami")
        test_artifacts(args.workspace_id)
        test_list_files(args.workspace_id, "/workspace/repo")
        job_id = test_start_job(args.workspace_id, "echo 'background job done'")
        if job_id:
            import time
            time.sleep(2)
            cursor = test_get_job(args.workspace_id, job_id)
            test_get_job(args.workspace_id, job_id, cursor)
            test_cancel_job(args.workspace_id, job_id)
        test_destroy(args.workspace_id)

    sys.exit(0 if success else 1)
