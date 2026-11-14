#!/usr/bin/env python3
"""
CLI tool to test all mcwsd (Workspace Daemon) functions.
Usage:
    python test_mcwsd_cli.py <command> [args...]

Commands:
    prepare <repo_url> [ref]        - Create a new workspace
    exec <workspace_id> <cmd>       - Execute a command in workspace
    artifacts <workspace_id> [paths]- Get diff/patches from workspace
    status <workspace_id>           - Check workspace status
    destroy <workspace_id>          - Destroy a workspace
    list-files <workspace_id> <path>- List files at a path in workspace
    read-file <workspace_id> <path> - Read a file from workspace
    write-file <workspace_id> <path> <content> - Write a file in workspace
    start-process <workspace_id> <cmd>          - Start a background process
    list-processes <workspace_id>               - List background processes
    process-status <workspace_id> <pid>         - Get process output/status
    send-input <workspace_id> <pid> <input>     - Send stdin to process
    kill-process <workspace_id> <pid>           - Kill a background process
    commit-and-pr <workspace_id> <msg> <title>  - Commit changes and open a PR
    full-test <repo_url> [ref]      - Run full workflow: prepare -> exec -> artifacts -> destroy

Global Options:
    --provisioner <type>            - Specify the provisioner used by the daemon (e.g., docker, podman, apple-containers, freebsd-jails)
"""

import os
import json
import argparse
from mcwsd import Tools


def get_tools():
    """Initialize tools with default valves."""
    tools = Tools()
    # You can override these via environment variables if needed
    # tools.valves.base_url = "http://localhost:9080"
    # tools.valves.token = ""  # Set your token here or via MCWSD_TOKEN env
    # tools.valves.timeout_seconds = 60
    tools.valves.base_url = os.getenv("MCWSD_BASE_URL", "http://localhost:9080")
    tools.valves.token = os.getenv("MCWSD_TOKEN", "")
    tools.valves.timeout_seconds = int(os.getenv("MCWSD_TIMEOUT", 60))
    return tools


def bind_workspace_token(tools, workspace_id, token):
    """Bind a workspace token for strict-mode daemons."""
    if workspace_id and token:
        tools._bind_workspace_token(workspace_id, token)


def cmd_prepare(tools, repo_url, ref="main"):
    """Test prepare_workspace function."""
    print(f"Preparing workspace from {repo_url} (ref={ref})...")
    result = tools.prepare_workspace(repo_url, ref)
    print("Response:")
    try:
        data = json.loads(result)
        print(json.dumps(data, indent=2))
        if "workspace_id" in data:
            profile = data.get("profile") or {}
            print(f"\n✓ Workspace created: {data['workspace_id']}")
            print(f"  OS: {data.get('os', 'unknown')}")
            print(
                "  Environment: "
                f"{profile.get('language', 'unknown')} "
                f"(package manager: {profile.get('package_manager', 'none')})"
            )
            print(f"  Cache hit: {data.get('cache_hit', False)}")
    except json.JSONDecodeError:
        print(result)
    return result


def cmd_exec(tools, workspace_id, command, timeout=120, allow_network=False):
    """Test exec_command function."""
    print(f"Executing '{command}' in workspace {workspace_id}...")
    result = tools.exec_command(
        workspace_id, command, allow_network=allow_network, timeout_seconds=timeout
    )
    print("Response:")
    try:
        data = json.loads(result)
        print(json.dumps(data, indent=2))
        if "exit_code" in data:
            print(f"\n✓ Command completed with exit code: {data['exit_code']}")
            print(f"  Duration: {data.get('duration_ms', 0)}ms")
    except json.JSONDecodeError:
        print(result)
    return result


def cmd_artifacts(tools, workspace_id, paths=""):
    """Test get_artifacts function."""
    print(f"Fetching artifacts from workspace {workspace_id}...")
    if paths:
        print(f"  Scoped to paths: {paths}")
    result = tools.get_artifacts(workspace_id, paths)
    print("Response:")
    try:
        data = json.loads(result)
        print(json.dumps(data, indent=2))
        if "patch" in data:
            patch_lines = data["patch"].count("\n")
            print(f"\n✓ Diff patch retrieved ({patch_lines} lines)")
        if "untracked_files" in data:
            print(f"  Untracked files: {len(data['untracked_files'])}")
    except json.JSONDecodeError:
        print(result)
    return result


def cmd_status(tools, workspace_id):
    """Test workspace_status function."""
    print(f"Checking status of workspace {workspace_id}...")
    result = tools.workspace_status(workspace_id)
    print("Response:")
    try:
        data = json.loads(result)
        print(json.dumps(data, indent=2))
        if "status" in data:
            print(f"\n✓ Workspace status: {data['status']}")
    except json.JSONDecodeError:
        print(result)
    return result


def cmd_destroy(tools, workspace_id):
    """Test destroy_workspace function."""
    print(f"Destroying workspace {workspace_id}...")
    result = tools.destroy_workspace(workspace_id)
    print(f"Response: {result}")
    return result


def cmd_commit_and_pr(tools, workspace_id, msg, title, desc="", repo="", proj=""):
    """Test commit_and_pr function."""
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
    try:
        data = json.loads(result)
        print(json.dumps(data, indent=2))
        if "mr_url" in data:
            print(f"\n✓ PR created: {data['mr_url']}")
    except json.JSONDecodeError:
        print(result)
    return result


# --- File System Tool Commands ---

def cmd_list_files(tools, workspace_id, path):
    """Test list_files function."""
    print(f"Listing files at '{path}' in workspace {workspace_id}...")
    result = tools.list_files(workspace_id, path)
    print("Response:")
    try:
        data = json.loads(result)
        print(json.dumps(data, indent=2))
        entries = data.get("entries", [])
        print(f"\n✓ {len(entries)} entries returned")
        for e in entries:
            kind = "DIR " if e.get("is_dir") else f"{e.get('size', 0):>8}B"
            print(f"  [{kind}] {e.get('name')}")
    except json.JSONDecodeError:
        print(result)
    return result


def cmd_read_file(tools, workspace_id, path):
    """Test read_file function."""
    print(f"Reading file '{path}' from workspace {workspace_id}...")
    result = tools.read_file(workspace_id, path)
    print("Response:")
    try:
        data = json.loads(result)
        content = data.get("content", "")
        lines = content.count("\n") + (1 if content else 0)
        print(f"[{lines} lines]\n")
        print(content)
        print(f"\n✓ File read ({len(content)} bytes)")
    except json.JSONDecodeError:
        print(result)
    return result


def cmd_write_file(tools, workspace_id, path, content):
    """Test write_file function."""
    print(f"Writing to '{path}' in workspace {workspace_id}...")
    result = tools.write_file(workspace_id, path, content)
    print(f"Response: {result}")
    return result


# --- Background Process Management Commands ---

def cmd_start_process(tools, workspace_id, command, allow_network=False):
    """Test start_process function."""
    print(f"Starting background process in workspace {workspace_id}...")
    print(f"  Command: {command}")
    result = tools.start_process(workspace_id, command, allow_network=allow_network)
    print("Response:")
    try:
        data = json.loads(result)
        print(json.dumps(data, indent=2))
        if "id" in data:
            print(f"\n✓ Process started: {data['id']} (status: {data.get('status')})")
    except json.JSONDecodeError:
        print(result)
    return result


def cmd_list_processes(tools, workspace_id):
    """Test list_processes function."""
    print(f"Listing processes in workspace {workspace_id}...")
    result = tools.list_processes(workspace_id)
    print("Response:")
    try:
        data = json.loads(result)
        procs = data.get("processes", [])
        print(json.dumps(data, indent=2))
        print(f"\n✓ {len(procs)} process(es) tracked")
    except json.JSONDecodeError:
        print(result)
    return result


def cmd_process_status(tools, workspace_id, process_id):
    """Test get_process_status function."""
    print(f"Getting status of process {process_id} in workspace {workspace_id}...")
    result = tools.get_process_status(workspace_id, process_id)
    print("Response:")
    try:
        data = json.loads(result)
        print(f"  ID:      {data.get('id')}")
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
        print(f"\n✓ Status retrieved")
    except json.JSONDecodeError:
        print(result)
    return result


def cmd_send_input(tools, workspace_id, process_id, input_text):
    """Test send_process_input function."""
    print(f"Sending input to process {process_id} in workspace {workspace_id}...")
    result = tools.send_process_input(workspace_id, process_id, input_text)
    print(f"Response: {result}")
    return result


def cmd_kill_process(tools, workspace_id, process_id):
    """Test kill_process function."""
    print(f"Killing process {process_id} in workspace {workspace_id}...")
    result = tools.kill_process(workspace_id, process_id)
    print(f"Response: {result}")
    return result


def cmd_full_test(tools, repo_url, ref="main"):
    """Run a full workflow test."""
    print("=" * 60)
    print("FULL WORKFLOW TEST")
    print("=" * 60)

    # Step 1: Prepare workspace
    print("\n[Step 1/6] Preparing workspace...")
    prepare_result = cmd_prepare(tools, repo_url, ref)

    try:
        ws_data = json.loads(prepare_result)
        workspace_id = ws_data.get("workspace_id")
        if not workspace_id:
            print("✗ Failed to get workspace_id from prepare response")
            return
    except json.JSONDecodeError:
        print("✗ Failed to parse prepare response as JSON")
        return

    # Step 2: Check status
    print("\n[Step 2/6] Checking workspace status...")
    cmd_status(tools, workspace_id)

    # Step 3: Execute a simple command
    print("\n[Step 3/6] Executing test command...")
    cmd_exec(tools, workspace_id, "echo 'Hello from mcwsd workspace!' && pwd && ls -la")

    # Step 4: List files
    print("\n[Step 4/6] Listing workspace files...")
    cmd_list_files(tools, workspace_id, ws_data.get("work_dir", "/workspace/repo"))

    # Step 5: Start a background process and poll it
    print("\n[Step 5/6] Starting a background process...")
    proc_result = cmd_start_process(tools, workspace_id, "sleep 5 && echo done")
    try:
        proc_data = json.loads(proc_result)
        pid = proc_data.get("id")
        if pid:
            import time
            time.sleep(1)
            cmd_process_status(tools, workspace_id, pid)
            cmd_kill_process(tools, workspace_id, pid)
    except (json.JSONDecodeError, KeyError):
        pass

    # Step 6: Get artifacts (may be empty if no changes)
    print("\n[Step 6/6] Fetching artifacts...")
    cmd_artifacts(tools, workspace_id)

    # Cleanup
    print("\n[Cleanup] Destroying workspace...")
    cmd_destroy(tools, workspace_id)

    print("\n" + "=" * 60)
    print("FULL TEST COMPLETE")
    print("=" * 60)


def main():
    parser = argparse.ArgumentParser(
        description="CLI tool to test mcwsd (Workspace Daemon) functions",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Examples:
  %(prog)s prepare https://github.com/user/repo.git main
  %(prog)s exec ws-abc123 "go test ./..."
  %(prog)s artifacts ws-abc123 src/,tests/
  %(prog)s status ws-abc123
  %(prog)s destroy ws-abc123
  %(prog)s list-files ws-abc123 /workspace/repo
  %(prog)s read-file ws-abc123 /workspace/repo/README.md
  %(prog)s write-file ws-abc123 /workspace/repo/hello.txt "hello world"
  %(prog)s start-process ws-abc123 "sleep 30"
  %(prog)s list-processes ws-abc123
  %(prog)s process-status ws-abc123 proc-abc123def4
  %(prog)s send-input ws-abc123 proc-abc123def4 "yes\\n"
  %(prog)s kill-process ws-abc123 proc-abc123def4
  %(prog)s full-test https://github.com/user/repo.git main
        """,
    )

    subparsers = parser.add_subparsers(dest="command", help="Available commands")

    # Global options
    # Provisioner is unused in the code base at this time.
    parser.add_argument("--provisioner", default="", help="Specify the provisioner used by the daemon (e.g., docker, podman, apple-containers, freebsd-jails)")
    parser.add_argument(
        "--workspace-token",
        default=os.getenv("MCWSD_WORKSPACE_TOKEN", ""),
        help="Workspace token for strict mode (or set MCWSD_WORKSPACE_TOKEN). Not needed for prepare/full-test, which learn it automatically.",
    )

    # prepare command
    prep_parser = subparsers.add_parser("prepare", help="Create a new workspace")
    prep_parser.add_argument("repo_url", help="Git repository URL")
    prep_parser.add_argument("ref", nargs="?", default="main", help="Branch/tag/commit (default: main)")

    # exec command
    exec_parser = subparsers.add_parser("exec", help="Execute a command in workspace")
    exec_parser.add_argument("workspace_id", help="Workspace ID")
    exec_parser.add_argument("command", help="Command to execute")
    exec_parser.add_argument("--timeout", type=int, default=120, help="Timeout in seconds")
    exec_parser.add_argument("--allow-network", action="store_true", help="Allow network access")

    # artifacts command
    art_parser = subparsers.add_parser("artifacts", help="Get diff/patches from workspace")
    art_parser.add_argument("workspace_id", help="Workspace ID")
    art_parser.add_argument("paths", nargs="?", default="", help="Comma-separated paths to scope diff")

    # status command
    stat_parser = subparsers.add_parser("status", help="Check workspace status")
    stat_parser.add_argument("workspace_id", help="Workspace ID")

    # destroy command
    dest_parser = subparsers.add_parser("destroy", help="Destroy a workspace")
    dest_parser.add_argument("workspace_id", help="Workspace ID")

    # list-files command
    lf_parser = subparsers.add_parser("list-files", help="List files at a path in workspace")
    lf_parser.add_argument("workspace_id", help="Workspace ID")
    lf_parser.add_argument("path", help="Absolute path inside the container to list")

    # read-file command
    rf_parser = subparsers.add_parser("read-file", help="Read a file from workspace")
    rf_parser.add_argument("workspace_id", help="Workspace ID")
    rf_parser.add_argument("path", help="Absolute path to the file inside the container")

    # write-file command
    wf_parser = subparsers.add_parser("write-file", help="Write a file in workspace")
    wf_parser.add_argument("workspace_id", help="Workspace ID")
    wf_parser.add_argument("path", help="Absolute path to write inside the container")
    wf_parser.add_argument("content", help="Text content to write")

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
    full_parser.add_argument("repo_url", help="Git repository URL")
    full_parser.add_argument("ref", nargs="?", default="main", help="Branch/tag/commit (default: main)")

    args = parser.parse_args()

    if not args.command:
        parser.print_help()
        sys.exit(1)

    tools = get_tools()
    bind_workspace_token(tools, getattr(args, "workspace_id", ""), args.workspace_token)

    if args.command == "prepare":
        cmd_prepare(tools, args.repo_url, args.ref)
    elif args.command == "exec":
        cmd_exec(tools, args.workspace_id, args.command, args.timeout, args.allow_network)
    elif args.command == "artifacts":
        cmd_artifacts(tools, args.workspace_id, args.paths)
    elif args.command == "status":
        cmd_status(tools, args.workspace_id)
    elif args.command == "destroy":
        cmd_destroy(tools, args.workspace_id)
    elif args.command == "list-files":
        cmd_list_files(tools, args.workspace_id, args.path)
    elif args.command == "read-file":
        cmd_read_file(tools, args.workspace_id, args.path)
    elif args.command == "write-file":
        cmd_write_file(tools, args.workspace_id, args.path, args.content)
    elif args.command == "start-process":
        cmd_start_process(tools, args.workspace_id, args.command, args.allow_network)
    elif args.command == "list-processes":
        cmd_list_processes(tools, args.workspace_id)
    elif args.command == "process-status":
        cmd_process_status(tools, args.workspace_id, args.process_id)
    elif args.command == "send-input":
        cmd_send_input(tools, args.workspace_id, args.process_id, args.input_text)
    elif args.command == "kill-process":
        cmd_kill_process(tools, args.workspace_id, args.process_id)
    elif args.command == "commit-and-pr":
        cmd_commit_and_pr(tools, args.workspace_id, args.commit_message, args.pr_title, args.desc, args.repo, args.project)
    elif args.command == "full-test":
        cmd_full_test(tools, args.repo_url, args.ref)
    else:
        parser.print_help()
        sys.exit(1)


if __name__ == "__main__":
    main()
