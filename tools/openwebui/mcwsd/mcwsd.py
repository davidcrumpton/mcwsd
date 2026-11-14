"""
title: Workspace Daemon (mcwsd)
description: Prepare ephemeral dev workspaces, run commands, pull diffs, and tear down via the local mcwsd Go daemon.
version: 0.3.8
"""

import json
import time

import requests
from pydantic import BaseModel, Field


class Tools:
    class Valves(BaseModel):
        base_url: str = Field(
            default="http://localhost:9080",
            description="Base URL of the mcwsd daemon.",
        )
        token: str = Field(
            default="",
            description="Admin/control-plane bearer token (MCWSD_TOKEN). Workspace tokens are learned automatically from prepare_workspace responses.",
        )
        timeout_seconds: int = Field(
            default=180,
            description="Default HTTP timeout for daemon requests.",
        )
        provision_timeout_seconds: int = Field(
            default=1800,
            description="How long to poll a backgrounded provision before giving up.",
        )

    def __init__(self):
        self.valves = self.Valves()
        self._workspace_tokens = {}

    def _headers(self):
        return {
            "Authorization": f"Bearer {self.valves.token}",
            "Content-Type": "application/json",
        }

    def _workspace_headers(self, workspace_id: str):
        token = self._workspace_tokens.get(workspace_id) or self.valves.token
        return {
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        }

    def _bind_workspace_token(self, workspace_id, token) -> None:
        workspace_id = (workspace_id or "").strip()
        token = (token or "").strip()
        if workspace_id and token:
            self._workspace_tokens[workspace_id] = token

    def _url(self, path: str) -> str:
        return f"{self.valves.base_url.rstrip('/')}{path}"

    def prepare_workspace(
        self,
        repo_url: str = "",
        ref: str = "main",
        env_overrides: dict = {},
        wait_seconds: int = 0,
        run_async: bool = True,
    ) -> str:
        """
        Provision a new ephemeral workspace. When repo_url is empty, create a blank
        workspace without contacting a forge. Otherwise the daemon requires forge
        authentication and clones the repository before detecting its environment.

        Provisioning runs as a background job on the daemon (clone, runtime boot,
        dependency install). This tool starts it asynchronously and polls the job
        until it completes, so a slow VM boot or a large clone does not trip the
        HTTP timeout. The returned JSON is identical whether the job finished
        inline or was polled. Set run_async=False (or wait_seconds>0) to make the
        daemon block inline instead.

        The response's `os` field is the provisioned operating system, and
        `profile.language` is the detected environment. The workspace may be a
        Linux container or a FreeBSD/OpenBSD/NetBSD/Minix jail, so ALWAYS check
        `os` before installing anything and use that platform's tools and
        binaries (do not assume Linux):
          - linux:   apt-get install -y ...  /  apk add ...
          - freebsd: pkg install -y ...
          - openbsd: pkg_add ...
          - netbsd:  pkgin install ...
          - minix:   pkgin install ...

        `profile.system_package_manager` and `profile.system_install_command`
        give the runtime's system package manager and the exact prefix for
        installing system tools (e.g. "pkg" / "pkg install -y" on FreeBSD,
        "apt" / "apt-get install -y" on Debian). IMPORTANT: `package_manager`
        describes only the *project's* dependency manager. When it is "none"
        (or the language is "unknown"), that does NOT mean the runtime has no
        package manager — it means the project's language was not detected.
        Never tell the user tools cannot be installed based on
        `package_manager`; use `system_install_command` to install what you
        need (e.g. `pkg install -y bash`).

        If `profile.language` is "unknown", that is NOT an error and you must NOT
        stop to ask the user which language to use. Instead, inspect the repo
        yourself and set the environment up:
          1. List files (e.g. exec_command with `ls -la` and `cat README.md`).
          2. Install what the project needs, using exec_command with
             allow_network=True and the system install command above.
          3. Continue with the task. Only ask the user if you are genuinely blocked.
        If you already know the environment, pass `env_overrides` to pin it and skip
        the guesswork.

        If `repo_url` is empty, `profile.language` is "blank". Write or create the
        project files first, then inspect them and install only what the project needs.

        :param repo_url: Optional Git URL to clone (https or ssh). Leave empty for a blank workspace.
        :param ref: Branch, tag, or commit to check out. Defaults to "main".
        :param env_overrides: Optional map pinning the environment, e.g.
            {"language": "node", "package_manager": "npm", "install_command":
            "npm ci", "base_image": "node:20"}. Supported keys: language,
            version, package_manager, install_command, base_image,
            post_create_command, system_package_manager,
            system_install_command, notes.
        :param wait_seconds: Seconds the daemon blocks inline before backgrounding
            (0 uses the daemon default; ignored when run_async=True).
        :param run_async: Start as a background job and poll until it finishes
            (default True). Set False to block on the initial request.
        :return: JSON string with workspace_id, workspace_token, os, work_dir,
            detected environment profile (including system_package_manager and
            system_install_command), and cache_hit. The tool stores
            workspace_token internally and uses it for subsequent
            workspace-scoped calls. `work_dir` is the absolute
            path of the repo inside the workspace (currently "/workspace/repo");
            treat it as the base for every file path. Prefer paths relative to
            it (e.g. "main.c"), which are resolved against work_dir.
        """
        try:
            payload = {"ref": ref}
            if repo_url:
                payload["repo_url"] = repo_url
            if env_overrides:
                payload["env_overrides"] = env_overrides
            if wait_seconds and wait_seconds > 0:
                payload["wait_seconds"] = wait_seconds
            if run_async:
                payload["async"] = True

            resp = requests.post(
                self._url("/workspaces"),
                headers=self._headers(),
                json=payload,
                timeout=self.valves.timeout_seconds,
            )
            resp.raise_for_status()
            data = resp.json()
            self._bind_workspace_token(data.get("workspace_id"), data.get("workspace_token"))

            job_id = data.get("job_id")
            if not job_id:
                return resp.text

            # Backgrounded: poll the job until it reaches a terminal state.
            return self._await_provision(data.get("workspace_id"), job_id)
        except requests.exceptions.RequestException as e:
            return f"Error preparing workspace: {e}"

    def _await_provision(self, workspace_id: str, job_id: str) -> str:
        """Poll a backgrounded provision job until it finishes."""
        deadline = time.time() + self.valves.provision_timeout_seconds
        cursor = ""
        while time.time() < deadline:
            params = {"cursor": cursor} if cursor else None
            resp = requests.get(
                self._url(f"/workspaces/{workspace_id}/jobs/{job_id}"),
                headers=self._workspace_headers(workspace_id),
                params=params,
                timeout=self.valves.timeout_seconds,
            )
            resp.raise_for_status()
            data = resp.json()
            cursor = data.get("cursor", cursor)
            status = data.get("status")
            if status in ("succeeded", "failed", "cancelled", "timed_out"):
                if status == "succeeded" and data.get("workspace"):
                    workspace = data["workspace"]
                    self._bind_workspace_token(
                        workspace.get("workspace_id"), workspace.get("workspace_token")
                    )
                    return json.dumps(workspace)
                detail = data.get("output", "")
                return f"Error preparing workspace: provisioning {status}. {detail}".strip()
            time.sleep(2)
        return (
            f"Error preparing workspace: provisioning did not finish within "
            f"{self.valves.provision_timeout_seconds}s (workspace {workspace_id}, job {job_id})."
        )

    def exec_command(
        self,
        workspace_id: str,
        command: str,
        allow_network: bool,
        timeout_seconds: int = 0,
        secrets: list[str] = [],
        wait_seconds: int = 0,
        run_async: bool = False,
    ) -> str:
        """
        Run a shell command inside an existing workspace and return its output.

        Commands run with the repo as the working directory (the `work_dir`
        returned by prepare_workspace, i.e. "/workspace/repo"), so relative
        paths like "test_run.sh" or "./build.sh" refer to files in the repo.
        Do not cd to the repo root — you are already there.

        Long commands can be promoted to a background job: if `wait_seconds` is
        set, the daemon waits that long and, if the command is still running,
        returns a `job_id` instead of output. `run_async=True` returns a
        `job_id` immediately. When a response contains a `job_id`, poll it with
        get_job. If both are left at their defaults the call blocks as before.

        :param workspace_id: ID returned by prepare_workspace.
        :param command: Shell command to execute, e.g. "go test ./..." or "pytest".
        :param allow_network: MANDATORY: Set to True if this command needs internet access.
                              Set to False for local tasks to enhance security.
        :param timeout_seconds: Max seconds to allow the command to run before it's killed.
                                Pass 0 (the default) to use the configured default timeout.
        :param secrets: List of secret reference names to grant for this exec call only.
        :param wait_seconds: Seconds to block before promoting to a background job
                             (0 uses the daemon default).
        :param run_async: Return a job_id immediately without waiting.
        :return: JSON string with stdout, stderr, exit_code, and duration_ms, or
                 with job_id and status when the command was promoted.
        """
        try:
            effective_timeout = (
                timeout_seconds
                if timeout_seconds and timeout_seconds > 0
                else self.valves.timeout_seconds
            )
            payload = {
                "command": command,
                "timeout_seconds": effective_timeout,
                "secrets": secrets,
                "allow_network": allow_network,
            }
            if wait_seconds and wait_seconds > 0:
                payload["wait_seconds"] = wait_seconds
            if run_async:
                payload["async"] = True
            resp = requests.post(
                self._url(f"/workspaces/{workspace_id}/exec"),
                headers=self._workspace_headers(workspace_id),
                json=payload,
                timeout=effective_timeout + 30,
            )
            resp.raise_for_status()
            return resp.text
        except requests.exceptions.RequestException as e:
            return f"Error executing command: {e}"

    def get_artifacts(self, workspace_id: str, paths: str = "") -> str:
        """
        Retrieve a diff-patch of uncommitted changes made inside the workspace,
        plus any new untracked files, without pushing or committing anything.

        :param workspace_id: ID returned by prepare_workspace.
        :param paths: Optional comma-separated list of paths to scope the diff to.
        :return: JSON string with the unified diff patch and untracked file list.
        """
        try:
            params = {"paths": paths} if paths else {}
            resp = requests.get(
                self._url(f"/workspaces/{workspace_id}/artifacts"),
                headers=self._workspace_headers(workspace_id),
                params=params,
                timeout=self.valves.timeout_seconds,
            )
            resp.raise_for_status()
            return resp.text
        except requests.exceptions.RequestException as e:
            return f"Error fetching artifacts: {e}"

    def safe_edit(
        self,
        workspace_id: str,
        file_path: str,
        pattern: str,
        replacement: str,
    ) -> str:
        """
        Perform a literal string replacement of pattern with replacement inside a
        file within the container. Done entirely in Go memory via the Docker API —
        no sed, no shell, no escaping hazards. Returns an error if the pattern is
        not found (so the caller always knows whether the edit took effect).

        :param workspace_id: ID returned by prepare_workspace.
        :param file_path: Path of the file to edit. Prefer a path relative to
                          the repo root (e.g. "src/main.c"); it is resolved
                          against work_dir ("/workspace/repo"). Absolute paths
                          must be inside the repo.
        :param pattern: Exact literal string to find (not a regex).
        :param replacement: String to replace every occurrence of pattern with.
        """
        try:
            payload = {
                "file_path": file_path,
                "pattern": pattern,
                "replacement": replacement,
            }

            resp = requests.post(
                self._url(f"/workspaces/{workspace_id}/safe-edit"),
                headers=self._workspace_headers(workspace_id),
                json=payload,
                timeout=self.valves.timeout_seconds,
            )
            resp.raise_for_status()
            return resp.text
        except requests.exceptions.RequestException as e:
            return f"Error editing file {e}"

    def commit_and_pr(
        self,
        workspace_id: str,
        commit_message: str,
        pr_title: str,
        pr_description: str = "",
        repo_url: str = "",
        project_path: str = "",
        gitlab_project_path: str = "",
    ) -> str:
        """
        Commit the workspace's current changes and open a pull/merge request.
        The branch name is chosen by the daemon, not the model — this can only
        ever create a new branch, never overwrite an existing one or push to
        main directly. Call this once the task is complete and tested.

        The forge (GitHub, GitLab, Gitea, ...) is configured on the daemon, not
        chosen per request. Do NOT pass or guess a forge type; just supply the
        repo URL and let the server use its configured forge.

        :param workspace_id: ID returned by prepare_workspace.
        :param commit_message: Git commit message for the changes.
        :param pr_title: Title for the pull/merge request.
        :param pr_description: Optional longer description for the pull/merge request.
        :param repo_url: Repository URL to push to.
        :param project_path: Project path or owner/repo format for the forge.
        :param gitlab_project_path: URL-encoded namespace/project path, or numeric project ID (legacy, use project_path).
        :return: JSON string with the branch name and merge request URL, or an error.
        """
        try:
            payload = {
                "commit_message": commit_message,
                "pr_title": pr_title,
                "pr_description": pr_description,
                "repo_url": repo_url,
            }
            if project_path:
                payload["project_path"] = project_path
            if gitlab_project_path:
                payload["gitlab_project_path"] = gitlab_project_path

            resp = requests.post(
                self._url(f"/workspaces/{workspace_id}/commit-and-pr"),
                headers=self._workspace_headers(workspace_id),
                json=payload,
                timeout=self.valves.timeout_seconds,
            )
            resp.raise_for_status()
            return resp.text
        except requests.exceptions.RequestException as e:
            return f"Error committing and opening PR: {e}"

    def workspace_status(self, workspace_id: str) -> str:
        """
        Check whether a workspace is alive and healthy.

        :param workspace_id: ID returned by prepare_workspace.
        :return: JSON string with status info.
        """
        try:
            resp = requests.get(
                self._url(f"/workspaces/{workspace_id}/status"),
                headers=self._workspace_headers(workspace_id),
                timeout=self.valves.timeout_seconds,
            )
            resp.raise_for_status()
            return resp.text
        except requests.exceptions.RequestException as e:
            return f"Error fetching status: {e}"

    def destroy_workspace(self, workspace_id: str) -> str:
        """
        Tear down a workspace and free its resources. Call this when done with a task.

        :param workspace_id: ID returned by prepare_workspace.
        :return: Confirmation or error message.
        """
        try:
            resp = requests.delete(
                self._url(f"/workspaces/{workspace_id}"),
                headers=self._workspace_headers(workspace_id),
                timeout=self.valves.timeout_seconds,
            )
            resp.raise_for_status()
            return f"Workspace {workspace_id} destroyed."
        except requests.exceptions.RequestException as e:
            return f"Error destroying workspace: {e}"

    # -------------------------------------------------------------------------
    # File System Tools
    # -------------------------------------------------------------------------

    def list_files(self, workspace_id: str, path: str) -> str:
        """
        Return a structured listing of files and directories at a given path
        inside the workspace. Equivalent to a single-level directory listing.

        The repo root is the `work_dir` returned by prepare_workspace
        ("/workspace/repo"). Use "." or "/workspace/repo" for the repo root.
        Relative paths (e.g. "src") are resolved against the repo root.

        :param workspace_id: ID returned by prepare_workspace.
        :param path: Path to list, relative to the repo root (e.g. "." or
                     "src"), or an absolute path inside the repo.
        :return: JSON string with an "entries" array, each entry containing
                 name, path, is_dir, and size (bytes for files, 0 for directories).
        """
        try:
            resp = requests.get(
                self._url(f"/workspaces/{workspace_id}/files"),
                headers=self._workspace_headers(workspace_id),
                params={"path": path},
                timeout=self.valves.timeout_seconds,
            )
            resp.raise_for_status()
            return resp.text
        except requests.exceptions.RequestException as e:
            return f"Error listing files: {e}"

    def read_file(self, workspace_id: str, path: str) -> str:
        """
        Read the full contents of a specific file inside the workspace.

        The repo root is the `work_dir` returned by prepare_workspace
        ("/workspace/repo"). Prefer a path relative to it (e.g. "main.c");
        relative paths are resolved against the repo root.

        :param workspace_id: ID returned by prepare_workspace.
        :param path: Path to the file, relative to the repo root (e.g.
                     "main.c" or "src/main.c"), or an absolute path inside
                     the repo.
        :return: JSON string with a "content" field containing the file text.
        """
        try:
            resp = requests.get(
                self._url(f"/workspaces/{workspace_id}/file"),
                headers=self._workspace_headers(workspace_id),
                params={"path": path},
                timeout=self.valves.timeout_seconds,
            )
            resp.raise_for_status()
            return resp.text
        except requests.exceptions.RequestException as e:
            return f"Error reading file: {e}"

    def write_file(self, workspace_id: str, path: str, content: str) -> str:
        """
        Write text content to a file inside the workspace, creating or overwriting it.
        Use safe_edit instead when you only want to replace a specific substring.

        The repo root is the `work_dir` returned by prepare_workspace
        ("/workspace/repo"). Prefer a path relative to it (e.g. "test_run.sh");
        relative paths are resolved against the repo root.

        :param workspace_id: ID returned by prepare_workspace.
        :param path: Path to write, relative to the repo root (e.g.
                     "test_run.sh" or "config.json"), or an absolute path
                     inside the repo.
        :param content: Full text content to write to the file.
        :return: Confirmation message on success, or an error.
        """
        try:
            resp = requests.put(
                self._url(f"/workspaces/{workspace_id}/file"),
                headers=self._workspace_headers(workspace_id),
                json={"path": path, "content": content},
                timeout=self.valves.timeout_seconds,
            )
            resp.raise_for_status()
            return f"File {path} written successfully."
        except requests.exceptions.RequestException as e:
            return f"Error writing file: {e}"

    # -------------------------------------------------------------------------
    # Background Job Tools
    # -------------------------------------------------------------------------

    def start_job(
        self,
        workspace_id: str,
        command: str,
        allow_network: bool = False,
        timeout_seconds: int = 0,
    ) -> str:
        """
        Launch a long-running command inside the workspace as a background job and
        return immediately with its job_id. Use get_job to poll for new output and
        check whether it has finished, and cancel_job to stop it. Ideal for builds,
        test suites, servers, or any command that should run concurrently while
        other work continues.

        :param workspace_id: ID returned by prepare_workspace.
        :param command: Shell command to run in the background, e.g. "npm run build".
        :param allow_network: Set to True if the job needs internet access.
        :param timeout_seconds: Kill the job after this many seconds (0 uses the
                                daemon default; the daemon may cap it).
        :return: JSON string with job_id, command, and initial status.
        """
        payload = {"command": command, "allow_network": allow_network}
        if timeout_seconds and timeout_seconds > 0:
            payload["timeout_seconds"] = timeout_seconds
        try:
            resp = requests.post(
                self._url(f"/workspaces/{workspace_id}/jobs"),
                headers=self._workspace_headers(workspace_id),
                json=payload,
                timeout=self.valves.timeout_seconds,
            )
            resp.raise_for_status()
            return resp.text
        except requests.exceptions.RequestException as e:
            return f"Error starting job: {e}"

    def list_jobs(self, workspace_id: str) -> str:
        """
        List all background jobs for a workspace, including their current status
        (queued, running, succeeded, failed, cancelled, or timed_out) and exit
        codes where available.

        :param workspace_id: ID returned by prepare_workspace.
        :return: JSON string with a "jobs" array.
        """
        try:
            resp = requests.get(
                self._url(f"/workspaces/{workspace_id}/jobs"),
                headers=self._workspace_headers(workspace_id),
                timeout=self.valves.timeout_seconds,
            )
            resp.raise_for_status()
            return resp.text
        except requests.exceptions.RequestException as e:
            return f"Error listing jobs: {e}"

    def get_job(self, workspace_id: str, job_id: str, cursor: str = "") -> str:
        """
        Get a background job's status and the output produced since cursor. Pass
        the "cursor" value from the previous response to receive ONLY new output;
        omit it to get everything so far. Polling with the cursor is cheap.

        :param workspace_id: ID returned by prepare_workspace.
        :param job_id: Job ID returned by start_job.
        :param cursor: Opaque cursor from the previous get_job response ("" for all output).
        :return: JSON string with job_id, command, status, exit_code, output, and cursor.
        """
        params = {"cursor": cursor} if cursor else None
        try:
            resp = requests.get(
                self._url(f"/workspaces/{workspace_id}/jobs/{job_id}"),
                headers=self._workspace_headers(workspace_id),
                params=params,
                timeout=self.valves.timeout_seconds,
            )
            resp.raise_for_status()
            return resp.text
        except requests.exceptions.RequestException as e:
            return f"Error getting job: {e}"

    def cancel_job(self, workspace_id: str, job_id: str) -> str:
        """
        Cancel a background job by ID, terminating its whole process group. The
        job's final status and remaining output stay readable via get_job.

        :param workspace_id: ID returned by prepare_workspace.
        :param job_id: Job ID returned by start_job.
        :return: Confirmation message on success, or an error.
        """
        try:
            resp = requests.delete(
                self._url(f"/workspaces/{workspace_id}/jobs/{job_id}"),
                headers=self._workspace_headers(workspace_id),
                timeout=self.valves.timeout_seconds,
            )
            resp.raise_for_status()
            return f"Job {job_id} cancelled."
        except requests.exceptions.RequestException as e:
            return f"Error cancelling job: {e}"

    # -------------------------------------------------------------------------
    # Deprecated process aliases (use the *_job methods above)
    # -------------------------------------------------------------------------

    def start_process(self, workspace_id, command, allow_network=False):
        """Deprecated alias for start_job."""
        return self.start_job(workspace_id, command, allow_network=allow_network)

    def list_processes(self, workspace_id):
        """Deprecated alias for list_jobs."""
        return self.list_jobs(workspace_id)

    def get_process_status(self, workspace_id, process_id):
        """Deprecated alias for get_job."""
        return self.get_job(workspace_id, process_id)

    def kill_process(self, workspace_id, process_id):
        """Deprecated alias for cancel_job."""
        return self.cancel_job(workspace_id, process_id)

    def send_process_input(self, workspace_id, process_id, input_text):
        """Deprecated: interactive stdin is not supported for background jobs."""
        return "Interactive stdin is not supported for background jobs."