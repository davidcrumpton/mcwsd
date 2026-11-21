"""An in-process fake mcwsd daemon.

Why a real HTTP server instead of mocking ``requests``: the thing under test is
a *client*, and most of what can go wrong lives in the wire contract — the
Authorization header, the JSON field names, the status codes the client has to
interpret, whether a 202 means "poll me" or "done". A mock that answers with
whatever the test author typed proves nothing about that. This server speaks
the same routes as ``internal/api/handlers.go`` and the same auth rules as
``internal/api/auth.go``, so the Python tools and the CLI scripts are exercised
end to end while the suite still runs offline on a random Tuesday.

Deliberate limits, so nobody mistakes it for the real daemon:

* Files live in a dict, not on disk. There is no git: ``artifacts`` is derived
  by diffing the file tree against the snapshot taken at prepare time.
* ``exec`` understands a handful of commands (echo, pwd, cat, ls, test, exit,
  whoami, sleep, true/false) so the scripts can run something meaningful. It
  is not a shell.
* Provisioning is instantaneous. ``async`` requests get a job that reports
  ``running`` for ``provision_polls`` polls and then succeeds (or fails, when
  the daemon is configured that way).
"""

import base64
import hashlib
import hmac
import json
import posixpath
import re
import secrets
import shlex
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

DEFAULT_ADMIN_TOKEN = "test-admin-token"
WORKSPACE_TOKEN_PREFIX = "wst_"
DEFAULT_WORK_DIR = "/workspace/repo"

SEED_FILES = {
    "README.md": "# demo repo\n\nUsed by the mcwsd test suite.\n",
    "src/main.go": "package main\n\nfunc main() {}\n",
}


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


class OutsideWorkspaceError(Exception):
    """Raised when a path escapes the workspace root."""


def workspace_token(admin_token, workspace_id):
    """Mint the same HMAC capability token the Go daemon mints.

    The daemon derives it from the admin token, so a token is valid for one
    workspace and needs no server-side state — which is why the fake daemon can
    authorize a workspace whose provision job has not finished yet.
    """
    key = hmac.new(
        admin_token.encode(), b"mcwsd/workspace-token/v1", hashlib.sha256
    ).digest()
    mac = hmac.new(key, workspace_id.encode(), hashlib.sha256).digest()
    return WORKSPACE_TOKEN_PREFIX + base64.urlsafe_b64encode(mac).rstrip(b"=").decode()


def _within(root, path):
    return path == root or path.startswith(root.rstrip("/") + "/")


def resolve_path(work_dir, path):
    """Mirror backend.resolveWorkspacePath.

    Relative paths resolve against work_dir; absolute paths are accepted when
    they sit inside work_dir (or at work_dir's parent, which is re-rooted);
    anything that escapes is rejected instead of silently landing elsewhere.
    """
    if path is None or not str(path).strip():
        raise OutsideWorkspaceError("path is required")

    root = posixpath.normpath(work_dir)
    candidate = str(path)

    if not candidate.startswith("/"):
        resolved = posixpath.normpath(posixpath.join(root, candidate))
        if _within(root, resolved):
            return resolved
        raise OutsideWorkspaceError(_outside_message(path, root))

    resolved = posixpath.normpath(candidate)
    if _within(root, resolved):
        return resolved

    parent = posixpath.dirname(root)
    if resolved == parent:
        return root
    if _within(parent, resolved):
        rerooted = posixpath.normpath(posixpath.join(root, posixpath.relpath(resolved, parent)))
        if _within(root, rerooted):
            return rerooted

    raise OutsideWorkspaceError(_outside_message(path, root))


def _outside_message(path, root):
    return (
        f"path {path!r} is outside the workspace; the workspace root is {root} "
        "(use a path relative to it, or an absolute path under it)"
    )


def _relpath(work_dir, resolved):
    rel = posixpath.relpath(resolved, posixpath.normpath(work_dir))
    return "" if rel == "." else rel


def split_command(command):
    """Split a shell command on && into simple argv lists (quote-aware)."""
    parts, current, quote = [], "", None
    index = 0
    while index < len(command):
        char = command[index]
        if quote:
            current += char
            if char == quote:
                quote = None
        elif char in ("'", '"'):
            quote = char
            current += char
        elif char == "&" and index + 1 < len(command) and command[index + 1] == "&":
            parts.append(current)
            current = ""
            index += 1
        else:
            current += char
        index += 1
    parts.append(current)

    commands = []
    for part in parts:
        try:
            argv = shlex.split(part)
        except ValueError:
            argv = part.split()
        if argv:
            commands.append(argv)
    return commands


# ---------------------------------------------------------------------------
# State
# ---------------------------------------------------------------------------


class FakeJob:
    def __init__(self, job_id, command, status="running"):
        self.id = job_id
        self.command = command
        self.status = status
        self.exit_code = None
        self.stdout = ""
        self.stderr = ""
        self.output = ""
        self.cursor = 0
        self.provisioning_workspace = None
        self.polls = 0


class FakeWorkspace:
    def __init__(
        self,
        workspace_id,
        token,
        repo_url="",
        ref="main",
        work_dir=DEFAULT_WORK_DIR,
        os_name="linux",
        profile=None,
        cache_hit=False,
    ):
        self.id = workspace_id
        self.token = token
        self.repo_url = repo_url
        self.ref = ref
        self.work_dir = work_dir
        self.os = os_name
        self.profile = profile or {
            "language": "go" if repo_url else "blank",
            "version": "1.22",
            "package_manager": "go" if repo_url else "none",
            "install_command": "go mod download" if repo_url else "",
            "base_image": "golang:1.22" if repo_url else "alpine:3.19",
            "system_package_manager": "apt",
            "system_install_command": "apt-get install -y",
        }
        self.cache_hit = cache_hit
        self.files = dict(SEED_FILES) if repo_url else {}
        self.baseline = dict(self.files)
        self.jobs = {}
        self.destroyed = False

    def prepare_payload(self):
        payload = {
            "workspace_id": self.id,
            "workspace_token": self.token,
            "os": self.os,
            "work_dir": self.work_dir,
            "profile": self.profile,
            "cache_hit": self.cache_hit,
        }
        if self.repo_url:
            payload["repo_url"] = self.repo_url
        return payload

    def resolve(self, path):
        return _relpath(self.work_dir, resolve_path(self.work_dir, path))

    def artifacts(self):
        """Diff the file tree against the snapshot taken at prepare time."""
        artifacts = []
        changed = sorted(
            name
            for name, content in self.files.items()
            if self.baseline.get(name) != content
        )
        if changed:
            patch = "\n".join("diff --git a/%s b/%s" % (name, name) for name in changed)
            artifacts.append({"name": "changes.patch", "type": "patch", "content": patch + "\n"})
        for name in changed:
            artifacts.append({"name": name, "type": "file", "content": self.files[name]})
        return artifacts


class Fault:
    """A scripted non-default response for one route."""

    def __init__(self, method, path_regex, status, body, content_type="text/plain", remaining=None):
        self.method = method.upper()
        self.pattern = re.compile(path_regex)
        self.status = status
        self.body = body
        self.content_type = content_type
        self.remaining = remaining

    def matches(self, method, path):
        return self.method == method.upper() and self.pattern.search(path) is not None


# ---------------------------------------------------------------------------
# The daemon
# ---------------------------------------------------------------------------


class FakeDaemon:
    """A threaded HTTP server that behaves like mcwsd.

    Use it as a context manager::

        with FakeDaemon() as daemon:
            ...  # daemon.base_url points at it
    """

    def __init__(
        self,
        admin_token=DEFAULT_ADMIN_TOKEN,
        auth_mode="scoped",
        provision_polls=0,
        provision_fails=False,
        commit_and_pr=True,
        seed_repo=True,
        default_os="linux",
        default_work_dir=DEFAULT_WORK_DIR,
    ):
        self.admin_token = admin_token
        self.auth_mode = auth_mode
        self.provision_polls = provision_polls
        self.provision_fails = provision_fails
        self.commit_and_pr_enabled = commit_and_pr
        self.seed_repo = seed_repo
        self.default_os = default_os
        self.default_work_dir = default_work_dir

        self.workspaces = {}
        self.jobs = {}
        self.requests = []
        self.faults = []
        self._lock = threading.Lock()
        self._server = None
        self._thread = None

    # -- lifecycle ---------------------------------------------------------

    def start(self):
        daemon = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args):  # keep test output clean
                pass

            def do_GET(self):
                daemon._handle(self, "GET")

            def do_POST(self):
                daemon._handle(self, "POST")

            def do_PUT(self):
                daemon._handle(self, "PUT")

            def do_DELETE(self):
                daemon._handle(self, "DELETE")

        self._server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self._server.daemon_threads = True
        # A short poll interval keeps per-test teardown at ~20 ms instead of
        # the default 500 ms; with dozens of tests that is the difference
        # between a snappy suite and a slow one.
        self._thread = threading.Thread(
            target=self._server.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True
        )
        self._thread.start()
        return self

    def stop(self):
        if self._server is not None:
            self._server.shutdown()
            self._server.server_close()
            self._server = None
        if self._thread is not None:
            self._thread.join(timeout=5)
            self._thread = None

    def __enter__(self):
        return self.start()

    def __exit__(self, *exc):
        self.stop()
        return False

    @property
    def port(self):
        return self._server.server_address[1]

    @property
    def base_url(self):
        return f"http://127.0.0.1:{self.port}"

    # -- test affordances --------------------------------------------------

    def add_fault(self, method, path_regex, status, body="boom", content_type="text/plain", times=None):
        """Answer the next `times` matching requests with a canned response."""
        self.faults.append(Fault(method, path_regex, status, body, content_type, times))

    def requests_to(self, method, path_regex):
        """Return the logged requests matching a method and path pattern."""
        pattern = re.compile(path_regex)
        return [
            r
            for r in self.requests
            if r["method"] == method.upper() and pattern.search(r["path"]) is not None
        ]

    def last_request(self, method, path_regex):
        matched = self.requests_to(method, path_regex)
        return matched[-1] if matched else None

    def workspace(self, workspace_id):
        return self.workspaces.get(workspace_id)

    def create_workspace(self, repo_url="", ref="main"):
        """Create a workspace out of band (for tests that skip prepare)."""
        return self._new_workspace(repo_url, ref)

    def token_for(self, workspace_id):
        return workspace_token(self.admin_token, workspace_id)

    # -- request plumbing --------------------------------------------------

    def _handle(self, handler, method):
        parsed = urlparse(handler.path)
        path = parsed.path
        query = parse_qs(parsed.query)

        length = int(handler.headers.get("Content-Length") or 0)
        raw_body = handler.rfile.read(length) if length else b""
        try:
            body = json.loads(raw_body) if raw_body else {}
        except (ValueError, UnicodeDecodeError):
            body = None

        with self._lock:
            self.requests.append(
                {
                    "method": method,
                    "path": path,
                    "query": {k: v[0] if len(v) == 1 else v for k, v in query.items()},
                    "headers": dict(handler.headers),
                    "raw_body": raw_body,
                    "body": body,
                }
            )

        status, headers, payload = self._route(method, path, query, body, handler.headers)
        self._respond(handler, status, headers, payload)

    def _respond(self, handler, status, headers, payload):
        if isinstance(payload, (dict, list)):
            body = json.dumps(payload).encode()
            content_type = "application/json"
        elif isinstance(payload, bytes):
            body = payload
            content_type = headers.get("Content-Type", "text/plain; charset=utf-8")
        else:
            body = str(payload).encode()
            content_type = headers.get("Content-Type", "text/plain; charset=utf-8")

        handler.send_response(status)
        handler.send_header("Content-Type", content_type)
        handler.send_header("Content-Length", str(len(body)))
        handler.end_headers()
        if body:
            handler.wfile.write(body)

    def _route(self, method, path, query, body, headers):
        for fault in list(self.faults):
            if fault.matches(method, path):
                if fault.remaining is not None:
                    fault.remaining -= 1
                    if fault.remaining <= 0:
                        self.faults.remove(fault)
                return fault.status, {"Content-Type": fault.content_type}, fault.body

        # --- control plane (admin token) ----------------------------------
        if method == "POST" and path == "/workspaces":
            if not self._is_admin(headers):
                return self._unauthorized()
            if body is None:
                return 400, {}, "invalid request body\n"
            return self._prepare(body)

        if method == "POST" and re.fullmatch(r"/workspaces/[^/]+/token", path):
            if not self._is_admin(headers):
                return self._unauthorized()
            wsid = path.split("/")[2]
            return 200, {}, {"workspace_id": wsid, "workspace_token": self.token_for(wsid)}

        if method == "GET" and path == "/admin/leftovers":
            if not self._is_admin(headers):
                return self._unauthorized()
            return 200, {}, {"leftovers": []}

        # --- workspace-scoped routes (workspace token) --------------------
        match = re.fullmatch(r"/workspaces/([^/]+)(/.*)?", path)
        if not match:
            return 404, {}, "not found\n"

        workspace_id, suffix = match.group(1), match.group(2) or ""
        if not self._is_workspace_authorized(headers, workspace_id):
            return self._unauthorized()

        return self._route_workspace(method, workspace_id, suffix, query, body)

    def _route_workspace(self, method, workspace_id, suffix, query, body):
        workspace = self.workspaces.get(workspace_id)

        if method == "GET" and suffix == "/status":
            # handleStatus reports a missing workspace as 404, unlike the
            # backend-backed routes which surface the backend error as a 500.
            if workspace is None or workspace.destroyed:
                return 404, {}, "workspace not found\n"
            return 200, {}, {
                "workspace_id": workspace_id,
                "status": "alive",
                "timestamp": int(time.time()),
            }

        if method == "DELETE" and suffix == "":
            if workspace is None or workspace.destroyed:
                return 500, {}, f"workspace {workspace_id} not found\n"
            workspace.destroyed = True
            return 204, {}, b""

        # Job routes come before the "destroyed" check: a provisioning job is
        # polled through them while its workspace is not yet addressable.
        if workspace is not None:
            job_response = self._route_jobs(method, workspace, suffix, query, body)
            if job_response is not None:
                return job_response

        if workspace is None or workspace.destroyed:
            return 500, {}, f"workspace {workspace_id} not found\n"

        if method == "POST" and suffix == "/exec":
            if body is None:
                return 400, {}, "invalid request body\n"
            return self._exec(workspace, body)

        if method == "GET" and suffix == "/artifacts":
            return 200, {}, {"artifacts": workspace.artifacts()}

        if method == "POST" and suffix == "/safe-edit":
            return self._safe_edit(workspace, body)

        if method == "POST" and suffix == "/commit-and-pr":
            return self._commit_and_pr(workspace, body)

        if method == "GET" and suffix == "/files":
            return self._list_files(workspace, query)
        if method == "GET" and suffix == "/file":
            return self._read_file(workspace, query)
        if method == "PUT" and suffix == "/file":
            return self._write_file(workspace, body)

        return 404, {}, "not found\n"

    def _route_jobs(self, method, workspace, suffix, query, body):
        """Handle /jobs and the legacy /processes alias. None = not a job route."""
        if method == "POST" and suffix in ("/jobs", "/processes"):
            return self._start_job(workspace, body)
        if method == "GET" and suffix in ("/jobs", "/processes"):
            return self._list_jobs(workspace)
        job_match = re.fullmatch(r"/(?:jobs|processes)/([^/]+)(/input)?", suffix)
        if not job_match:
            return None
        job_id, is_input = job_match.group(1), job_match.group(2)
        if is_input and method == "POST":
            return 501, {}, "interactive stdin is not supported for background jobs\n"
        if method == "GET":
            return self._get_job(workspace, job_id, query)
        if method == "DELETE":
            return self._kill_job(workspace, job_id)
        return None

    # -- auth --------------------------------------------------------------

    @staticmethod
    def _bearer(headers):
        value = headers.get("Authorization") or ""
        if not value.lower().startswith("bearer "):
            return ""
        return value[len("Bearer "):].strip()

    def _is_admin(self, headers):
        return self._bearer(headers) == self.admin_token

    def _is_workspace_authorized(self, headers, workspace_id):
        token = self._bearer(headers)
        if not token:
            return False
        expected = self.token_for(workspace_id)
        if hmac.compare_digest(token, expected):
            return True
        # scoped mode lets the admin token act as break-glass; strict mode
        # confines it to provisioning and /admin/*.
        if self.auth_mode == "scoped":
            return hmac.compare_digest(token, self.admin_token)
        return False

    @staticmethod
    def _unauthorized():
        return 401, {}, "unauthorized\n"

    # -- handlers ----------------------------------------------------------

    def _new_workspace(self, repo_url="", ref="main", os_name=None, env_overrides=None):
        workspace_id = "ws-" + secrets.token_hex(6)
        token = self.token_for(workspace_id)
        workspace = FakeWorkspace(
            workspace_id,
            token,
            repo_url=repo_url or "",
            ref=ref or "main",
            work_dir=self.default_work_dir,
            os_name=os_name or self.default_os,
        )
        if env_overrides:
            workspace.profile.update(env_overrides)
        if not self.seed_repo and not repo_url:
            workspace.files = {}
            workspace.baseline = {}
        self.workspaces[workspace_id] = workspace
        return workspace

    def _prepare(self, body):
        repo_url = body.get("repo_url") or ""
        ref = body.get("ref") or "main"
        if body.get("async"):
            workspace = self._new_workspace(repo_url, ref, env_overrides=body.get("env_overrides"))
            job = FakeJob("job-" + secrets.token_hex(5), "provision")
            job.provisioning_workspace = workspace
            self.jobs[job.id] = job
            workspace.destroyed = True  # not addressable until the job finishes
            return 202, {}, {
                "workspace_id": workspace.id,
                "workspace_token": workspace.token,
                "job_id": job.id,
                "status": "queued",
            }

        workspace = self._new_workspace(repo_url, ref, env_overrides=body.get("env_overrides"))
        return 200, {}, workspace.prepare_payload()

    def _exec(self, workspace, body):
        command = body.get("command")
        if not command:
            return 400, {}, "command is required\n"
        if body.get("async"):
            # Promoted to a background job: no output, just a job to poll.
            job = self._register_job(workspace, command)
            return 200, {}, {"job_id": job.id, "status": job.status}
        started = time.time()
        stdout, stderr, exit_code = self._run_command(workspace, command)
        return 200, {}, {
            "stdout": stdout,
            "stderr": stderr,
            "exit_code": exit_code,
            "duration_ms": int((time.time() - started) * 1000),
        }

    def _run_command(self, workspace, command):
        """A tiny deterministic shell: enough for the scripts, not a shell."""
        stdout, stderr = "", ""
        exit_code = 0
        for argv in split_command(command):
            name, args = argv[0], argv[1:]
            if name == "echo":
                stdout += " ".join(args) + "\n"
            elif name == "pwd":
                stdout += workspace.work_dir + "\n"
            elif name == "whoami":
                stdout += "mcwsd\n"
            elif name == "true":
                pass
            elif name == "false":
                exit_code = 1
            elif name == "sleep":
                pass  # never actually sleep: the suite must stay fast
            elif name == "exit":
                try:
                    exit_code = int(args[0]) if args else 0
                except ValueError:
                    exit_code = 2
                return stdout, stderr, exit_code
            elif name == "cat":
                for target in args or ["-"]:
                    try:
                        stdout += workspace.files[workspace.resolve(target)]
                    except (KeyError, OutsideWorkspaceError):
                        stderr += f"cat: {target}: No such file or directory\n"
                        exit_code = 1
            elif name == "ls":
                flags = [a for a in args if a.startswith("-")]
                targets = [a for a in args if not a.startswith("-")] or ["."]
                for target in targets:
                    try:
                        prefix = workspace.resolve(target)
                    except OutsideWorkspaceError as exc:
                        stderr += f"ls: {exc}\n"
                        exit_code = 1
                        continue
                    names = sorted(
                        {
                            entry[len(prefix) + 1:].split("/")[0]
                            for entry in workspace.files
                            if prefix == "" or entry.startswith(prefix + "/")
                        }
                    )
                    if "-l" in flags or "-la" in flags or "-al" in flags:
                        for entry in names:
                            full = f"{prefix}/{entry}" if prefix else entry
                            size = len(workspace.files.get(full, ""))
                            is_dir = any(f.startswith(full + "/") for f in workspace.files)
                            kind = "d" if is_dir else "-"
                            stdout += f"{kind}rw-r--r--  1 mcwsd  mcwsd  {size:>6} {entry}\n"
                    else:
                        stdout += "".join(f"{entry}\n" for entry in names)
            elif name == "test":
                if len(args) >= 2 and args[0] == "-f":
                    try:
                        exists = workspace.resolve(args[1]) in workspace.files
                    except OutsideWorkspaceError:
                        exists = False
                    exit_code = 0 if exists else 1
                else:
                    exit_code = 2
            elif name in ("sh", "bash"):
                # "sh -c '<script>'" — recurse into the script body.
                if len(args) >= 2 and args[0] == "-c":
                    out, err, code = self._run_command(workspace, args[1])
                    stdout += out
                    stderr += err
                    exit_code = code
                else:
                    stderr += f"{name}: unsupported invocation\n"
                    exit_code = 2
            else:
                stderr += f"sh: {name}: command not found\n"
                exit_code = 127
            if exit_code != 0:
                return stdout, stderr, exit_code
        return stdout, stderr, exit_code

    def _safe_edit(self, workspace, body):
        if body is None:
            return 400, {}, "invalid request body\n"
        file_path = body.get("file_path") or ""
        pattern = body.get("pattern")
        replacement = body.get("replacement")
        if pattern is None or replacement is None:
            return 400, {}, "invalid request body\n"
        try:
            name = workspace.resolve(file_path)
        except OutsideWorkspaceError as exc:
            return 500, {}, f"{exc}\n"
        if name not in workspace.files:
            return 500, {}, f"SafeEdit: read {workspace.work_dir}/{name}: no such file\n"
        original = workspace.files[name]
        if pattern not in original:
            return 500, {}, f"SafeEdit: pattern not found in {workspace.work_dir}/{name}\n"
        workspace.files[name] = original.replace(pattern, replacement)
        return 200, {}, {"success": True, "message": "Edit successful"}

    def _commit_and_pr(self, workspace, body):
        if body is None:
            return 400, {}, "invalid request body\n"
        if not self.commit_and_pr_enabled:
            return 500, {}, (
                "commit_and_pr is unavailable: no forge authentication is configured\n"
            )
        if not workspace.repo_url:
            return 500, {}, (
                "workspace was not provisioned from a repository; "
                "commit_and_pr requires one\n"
            )
        if not body.get("commit_message"):
            return 500, {}, "commit_message is required\n"
        if not body.get("pr_title"):
            return 500, {}, "pr_title is required\n"
        requested_repo = (body.get("repo_url") or "").strip()
        if requested_repo and requested_repo != workspace.repo_url:
            return 500, {}, (
                f'commit_and_pr target "{requested_repo}" does not match the '
                f"workspace's repository \"{workspace.repo_url}\"\n"
            )
        if not workspace.artifacts():
            return 200, {}, {
                "branch": "",
                "mr_url": "",
                "error": "no changes to commit",
                "no_changes_detail": "",
            }
        branch = "mcwsd/agent-" + secrets.token_hex(4)
        slug = workspace.repo_url.rstrip("/")
        if slug.endswith(".git"):
            slug = slug[:-4]
        parts = slug.rsplit("/", 2)
        project = "/".join(parts[-2:]) if len(parts) >= 2 else slug
        return 200, {}, {
            "branch": branch,
            "mr_url": f"https://forge.example.com/{project}/-/merge_requests/1",
        }

    def _list_files(self, workspace, query):
        path = (query.get("path") or [""])[0]
        if not path:
            return 400, {}, "path query parameter is required\n"
        try:
            prefix = workspace.resolve(path)
        except OutsideWorkspaceError as exc:
            return 500, {}, f"ListFiles: {exc}\n"

        entries = {}
        for name in workspace.files:
            if prefix:
                if not name.startswith(prefix + "/"):
                    continue
                rest = name[len(prefix) + 1:]
            else:
                rest = name
            if not rest:
                continue
            child, _, deeper = rest.partition("/")
            entry_path = f"{prefix}/{child}" if prefix else child
            if deeper:
                entries[child] = {"name": child, "path": entry_path, "is_dir": True, "size": 0}
            else:
                entries[child] = {
                    "name": child,
                    "path": entry_path,
                    "is_dir": False,
                    "size": len(workspace.files[name]),
                }
        return 200, {}, {"entries": [entries[k] for k in sorted(entries)]}

    def _read_file(self, workspace, query):
        path = (query.get("path") or [""])[0]
        if not path:
            return 400, {}, "path query parameter is required\n"
        try:
            name = workspace.resolve(path)
        except OutsideWorkspaceError as exc:
            return 500, {}, f"ReadFile: {exc}\n"
        if name not in workspace.files:
            return 500, {}, f"ReadFile: {workspace.work_dir}/{name}: no such file\n"
        return 200, {}, {"content": workspace.files[name]}

    def _write_file(self, workspace, body):
        if body is None:
            return 400, {}, "invalid request body\n"
        path = body.get("path") or ""
        if not path:
            return 400, {}, "path is required\n"
        try:
            name = workspace.resolve(path)
        except OutsideWorkspaceError as exc:
            return 500, {}, f"WriteFile: {exc}\n"
        workspace.files[name] = body.get("content") or ""
        return 204, {}, b""

    def _start_job(self, workspace, body):
        if body is None:
            return 400, {}, "invalid request body\n"
        command = body.get("command")
        if not command:
            return 400, {}, "command is required\n"
        job = self._register_job(workspace, command)
        return 201, {}, {
            "job_id": job.id,
            "id": job.id,
            "command": job.command,
            "status": "running",
        }

    def _register_job(self, workspace, command):
        """Run a command to completion up front and record it as a finished job."""
        job = FakeJob("job-" + secrets.token_hex(5), command)
        stdout, stderr, exit_code = self._run_command(workspace, command)
        job.stdout, job.stderr, job.exit_code = stdout, stderr, exit_code
        job.status = "succeeded" if exit_code == 0 else "failed"
        job.output = stdout + stderr
        job.cursor = 1
        self.jobs[job.id] = job
        workspace.jobs[job.id] = job
        return job

    def _list_jobs(self, workspace):
        jobs = [
            {
                "job_id": job.id,
                "id": job.id,
                "command": job.command,
                "status": job.status,
                "exit_code": job.exit_code or 0,
            }
            for job in workspace.jobs.values()
        ]
        return 200, {}, {"jobs": jobs, "processes": jobs}

    def _get_job(self, workspace, job_id, query):
        job = self.jobs.get(job_id)

        # A provisioning job reports "running" for provision_polls polls so the
        # client's polling loop is exercised, then finishes.
        if job is not None and job.provisioning_workspace is not None:
            job.polls += 1
            if job.polls <= self.provision_polls:
                return 200, {}, {
                    "job_id": job.id,
                    "status": "running",
                    "command": "provision",
                    "output": "cloning...\n",
                    "cursor": str(job.polls),
                }
            if self.provision_fails:
                job.status = "failed"
                return 200, {}, {
                    "job_id": job.id,
                    "status": "failed",
                    "command": "provision",
                    "output": "clone failed: repository not found\n",
                    "cursor": str(job.polls),
                }
            job.status = "succeeded"
            provisioned = job.provisioning_workspace
            provisioned.destroyed = False
            return 200, {}, {
                "job_id": job.id,
                "status": "succeeded",
                "command": "provision",
                "output": "done\n",
                "cursor": str(job.polls),
                "workspace": provisioned.prepare_payload(),
            }

        if job is None or job_id not in workspace.jobs:
            return 500, {}, f"job {job_id} not found\n"

        cursor = (query.get("cursor") or [""])[0]
        return 200, {}, {
            "job_id": job.id,
            "id": job.id,
            "command": job.command,
            "status": job.status,
            "exit_code": job.exit_code or 0,
            "stdout": job.stdout,
            "stderr": job.stderr,
            "output": "" if cursor else job.output,
            "cursor": str(job.cursor),
        }

    def _kill_job(self, workspace, job_id):
        job = workspace.jobs.get(job_id)
        if job is None:
            return 500, {}, f"job {job_id} not found\n"
        job.status = "cancelled"
        return 204, {}, b""


def unused_port():
    """Return a port with nothing listening on it (for connection-refused tests)."""
    import socket

    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]
