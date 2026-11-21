"""Positive cases: the documented behaviour of ``mcwsd.Tools`` happens.

Every test here talks to ``support.fake_daemon.FakeDaemon`` over real HTTP, so
what is under test is the request the tool builds, the response it parses, and
the side effect on the daemon — not a mocked return value.
"""

import json
import unittest

from tests.support.fake_daemon import FakeDaemon
from tests.support.harness import tools_client

REPO_URL = "https://github.com/example/demo.git"


class DaemonTestCase(unittest.TestCase):
    """Start a fresh fake daemon and a client per test."""

    daemon_options = {}

    def setUp(self):
        self.daemon = FakeDaemon(**self.daemon_options).start()
        self.addCleanup(self.daemon.stop)
        self.tools = tools_client(self.daemon)
        self.workspace = self.daemon.create_workspace(repo_url=REPO_URL)

    def prepare(self, repo_url=REPO_URL, **kwargs):
        """Prepare a workspace and return its parsed response."""
        return json.loads(self.tools.prepare_workspace(repo_url, **kwargs))


class ProvisionTests(DaemonTestCase):
    def test_blank_workspace(self):
        """A blank workspace needs no repo and reports a blank profile."""
        data = self.prepare(repo_url="")
        self.assertTrue(data["workspace_id"].startswith("ws-"))
        self.assertEqual(data["os"], "linux")
        self.assertEqual(data["work_dir"], "/workspace/repo")
        self.assertEqual(data["profile"]["language"], "blank")
        self.assertFalse(data["cache_hit"])
        self.assertNotIn("repo_url", data)

    def test_repo_workspace(self):
        """A repo workspace reports the environment the daemon detected."""
        data = self.prepare()
        self.assertEqual(data["repo_url"], REPO_URL)
        self.assertEqual(data["profile"]["language"], "go")
        self.assertEqual(data["profile"]["package_manager"], "go")
        listed = json.loads(self.tools.list_files(data["workspace_id"], "."))
        self.assertIn("README.md", [entry["name"] for entry in listed["entries"]])

    def test_inline_provision_returns_body_unchanged(self):
        """run_async=False returns the daemon's inline body, with no polling."""
        raw = self.tools.prepare_workspace(REPO_URL, "main", run_async=False)
        data = json.loads(raw)
        self.assertTrue(data["workspace_id"].startswith("ws-"))
        self.assertEqual(self.daemon.requests_to("GET", r"/jobs/"), [])

    def test_env_overrides_are_forwarded(self):
        """env_overrides pin the environment instead of relying on detection."""
        data = self.prepare(env_overrides={"language": "node", "package_manager": "npm"})
        self.assertEqual(data["profile"]["language"], "node")
        self.assertEqual(data["profile"]["package_manager"], "npm")
        sent = self.daemon.last_request("POST", r"^/workspaces$")["body"]
        self.assertEqual(sent["env_overrides"], {"language": "node", "package_manager": "npm"})

    def test_workspace_token_is_bound_after_prepare(self):
        """prepare learns the workspace token and uses it for later calls."""
        data = self.prepare()
        workspace_id = data["workspace_id"]
        self.tools.exec_command(workspace_id, "echo hi", allow_network=False)
        request = self.daemon.last_request("POST", r"/exec$")
        self.assertEqual(request["headers"]["Authorization"], f"Bearer {data['workspace_token']}")
        self.assertNotEqual(request["headers"]["Authorization"], f"Bearer {self.daemon.admin_token}")


class AsyncProvisionTests(DaemonTestCase):
    # One "running" poll is enough to prove the polling loop works without
    # paying the client's 2 s back-off twice.
    daemon_options = {"provision_polls": 1}

    def test_async_provision_is_polled_to_completion(self):
        """An async provision polls the job and returns the finished workspace."""
        data = self.prepare()
        self.assertTrue(data["workspace_id"].startswith("ws-"))
        self.assertIn("profile", data)
        polls = self.daemon.requests_to("GET", r"/jobs/")
        self.assertGreaterEqual(len(polls), 2)
        self.assertEqual(polls[0]["query"].get("cursor", ""), "")


class LifecycleTests(DaemonTestCase):
    def test_status_alive(self):
        """A live workspace reports itself alive."""
        data = json.loads(self.tools.workspace_status(self.workspace.id))
        self.assertEqual(data["status"], "alive")
        self.assertEqual(data["workspace_id"], self.workspace.id)

    def test_destroy(self):
        """Destroying a workspace reports it destroyed."""
        result = self.tools.destroy_workspace(self.workspace.id)
        self.assertIn("destroyed", result.lower())
        self.assertTrue(self.daemon.workspace(self.workspace.id).destroyed)


class AuthTests(DaemonTestCase):
    def test_admin_token_works_in_scoped_mode(self):
        """In scoped mode the admin token is accepted on workspace routes."""
        self.assertEqual(self.daemon.auth_mode, "scoped")
        data = json.loads(self.tools.workspace_status(self.workspace.id))
        self.assertEqual(data["status"], "alive")
        request = self.daemon.last_request("GET", r"/status$")
        self.assertEqual(request["headers"]["Authorization"], f"Bearer {self.daemon.admin_token}")

    def test_request_headers(self):
        """Requests carry a Bearer token and a JSON content type."""
        self.tools.exec_command(self.workspace.id, "echo hi", allow_network=False)
        request = self.daemon.last_request("POST", r"/exec$")
        self.assertEqual(request["headers"]["Authorization"], f"Bearer {self.daemon.admin_token}")
        self.assertEqual(request["headers"]["Content-Type"], "application/json")


class StrictModeAuthTests(DaemonTestCase):
    daemon_options = {"auth_mode": "strict"}

    def test_workspace_token_works_in_strict_mode(self):
        """In strict mode only the workspace's own token is accepted."""
        tools = tools_client(self.daemon, token=self.workspace.token)
        data = json.loads(tools.workspace_status(self.workspace.id))
        self.assertEqual(data["status"], "alive")


class ExecTests(DaemonTestCase):
    def test_exit_zero(self):
        """A successful command returns stdout and a zero exit code."""
        data = json.loads(
            self.tools.exec_command(self.workspace.id, "echo hello", allow_network=False)
        )
        self.assertEqual(data["exit_code"], 0)
        self.assertEqual(data["stdout"], "hello\n")
        self.assertEqual(data["stderr"], "")
        self.assertIn("duration_ms", data)

    def test_nonzero_exit_is_reported_not_raised(self):
        """A failing command is data, not an exception."""
        data = json.loads(
            self.tools.exec_command(self.workspace.id, "exit 3", allow_network=False)
        )
        self.assertEqual(data["exit_code"], 3)

    def test_payload_is_forwarded(self):
        """allow_network, timeout_seconds and secrets reach the daemon."""
        self.tools.exec_command(
            self.workspace.id,
            "echo hi",
            allow_network=True,
            timeout_seconds=42,
            secrets=["DEPLOY_KEY"],
        )
        body = self.daemon.last_request("POST", r"/exec$")["body"]
        self.assertTrue(body["allow_network"])
        self.assertEqual(body["timeout_seconds"], 42)
        self.assertEqual(body["secrets"], ["DEPLOY_KEY"])

    def test_default_timeout_uses_the_valve(self):
        """timeout_seconds=0 falls back to the configured valve value."""
        self.tools.valves.timeout_seconds = 77
        self.tools.exec_command(self.workspace.id, "echo hi", allow_network=False, timeout_seconds=0)
        body = self.daemon.last_request("POST", r"/exec$")["body"]
        self.assertEqual(body["timeout_seconds"], 77)

    def test_promoted_command_returns_a_job_id(self):
        """A promoted command answers with a job_id instead of output."""
        data = json.loads(
            self.tools.exec_command(self.workspace.id, "echo hi", allow_network=False, run_async=True)
        )
        self.assertIn("job_id", data)
        self.assertNotIn("exit_code", data)


class FileTests(DaemonTestCase):
    def test_write_then_read(self):
        """A written file reads back byte for byte."""
        content = "line one\nline two\n"
        result = self.tools.write_file(self.workspace.id, "notes.txt", content)
        self.assertIn("written successfully", result)
        data = json.loads(self.tools.read_file(self.workspace.id, "notes.txt"))
        self.assertEqual(data["content"], content)

    def test_overwrite(self):
        """Writing the same path twice replaces the content."""
        self.tools.write_file(self.workspace.id, "notes.txt", "first")
        self.tools.write_file(self.workspace.id, "notes.txt", "second")
        data = json.loads(self.tools.read_file(self.workspace.id, "notes.txt"))
        self.assertEqual(data["content"], "second")

    def test_safe_edit(self):
        """safe_edit replaces the literal pattern and reports success."""
        self.tools.write_file(self.workspace.id, "notes.txt", "status: draft\n")
        data = json.loads(
            self.tools.safe_edit(self.workspace.id, "notes.txt", "draft", "final")
        )
        self.assertTrue(data["success"])
        self.assertEqual(
            json.loads(self.tools.read_file(self.workspace.id, "notes.txt"))["content"],
            "status: final\n",
        )

    def test_safe_edit_replaces_every_occurrence(self):
        """safe_edit is a literal global replace, not a first-match replace."""
        self.tools.write_file(self.workspace.id, "notes.txt", "a a a\n")
        self.tools.safe_edit(self.workspace.id, "notes.txt", "a", "b")
        self.assertEqual(
            json.loads(self.tools.read_file(self.workspace.id, "notes.txt"))["content"],
            "b b b\n",
        )

    def test_list_files_entries(self):
        """list_files returns name/path/is_dir/size for each entry."""
        data = json.loads(self.tools.list_files(self.workspace.id, "."))
        entries = {entry["name"]: entry for entry in data["entries"]}
        self.assertIn("README.md", entries)
        self.assertIn("src", entries)
        self.assertFalse(entries["README.md"]["is_dir"])
        self.assertEqual(entries["README.md"]["size"], len(self.workspace.files["README.md"]))
        self.assertTrue(entries["src"]["is_dir"])

    def test_relative_path_resolves_against_work_dir(self):
        """A relative path is anchored at the repo root."""
        data = json.loads(self.tools.list_files(self.workspace.id, "src"))
        self.assertEqual([entry["name"] for entry in data["entries"]], ["main.go"])

    def test_absolute_path_inside_repo(self):
        """An absolute path inside the repo is accepted."""
        data = json.loads(
            self.tools.read_file(self.workspace.id, "/workspace/repo/README.md")
        )
        self.assertEqual(data["content"], self.workspace.files["README.md"])

    def test_absolute_path_at_work_dir_parent_is_rerooted(self):
        """'/workspace/notes.txt' lands inside the repo, not beside it."""
        self.tools.write_file(self.workspace.id, "/workspace/notes.txt", "hello\n")
        data = json.loads(self.tools.read_file(self.workspace.id, "notes.txt"))
        self.assertEqual(data["content"], "hello\n")


class ArtifactTests(DaemonTestCase):
    def test_unchanged_workspace_has_no_artifacts(self):
        """Nothing changed means no artifacts."""
        data = json.loads(self.tools.get_artifacts(self.workspace.id))
        self.assertEqual(data["artifacts"], [])

    def test_new_file_appears_as_an_artifact(self):
        """A new file shows up as an artifact alongside the patch."""
        self.tools.write_file(self.workspace.id, "notes.txt", "hello\n")
        data = json.loads(self.tools.get_artifacts(self.workspace.id))
        names = [artifact["name"] for artifact in data["artifacts"]]
        self.assertIn("notes.txt", names)
        self.assertIn("changes.patch", names)
        entry = next(a for a in data["artifacts"] if a["name"] == "notes.txt")
        self.assertEqual(entry["type"], "file")
        self.assertEqual(entry["content"], "hello\n")

    def test_paths_are_forwarded(self):
        """The paths argument is sent as a query parameter."""
        self.tools.get_artifacts(self.workspace.id, "src/,tests/")
        request = self.daemon.last_request("GET", r"/artifacts$")
        self.assertEqual(request["query"]["paths"], "src/,tests/")


class JobTests(DaemonTestCase):
    def test_start_job(self):
        """start_job returns the identifiers and the command."""
        data = json.loads(self.tools.start_job(self.workspace.id, "echo done"))
        self.assertTrue(data["job_id"].startswith("job-"))
        self.assertEqual(data["id"], data["job_id"])
        self.assertEqual(data["command"], "echo done")
        self.assertEqual(data["status"], "running")

    def test_list_jobs(self):
        """list_jobs returns the canonical array and the legacy alias."""
        self.tools.start_job(self.workspace.id, "echo done")
        data = json.loads(self.tools.list_jobs(self.workspace.id))
        self.assertEqual(len(data["jobs"]), 1)
        self.assertEqual(data["jobs"], data["processes"])

    def test_get_job(self):
        """get_job returns status, exit code and captured output."""
        job = json.loads(self.tools.start_job(self.workspace.id, "echo done"))
        data = json.loads(self.tools.get_job(self.workspace.id, job["job_id"]))
        self.assertEqual(data["status"], "succeeded")
        self.assertEqual(data["exit_code"], 0)
        self.assertIn("done", data["stdout"])

    def test_cursor_suppresses_seen_output(self):
        """Passing a cursor forwards it and returns only new output."""
        job = json.loads(self.tools.start_job(self.workspace.id, "echo done"))
        first = json.loads(self.tools.get_job(self.workspace.id, job["job_id"]))
        self.assertTrue(first["output"])
        second = json.loads(
            self.tools.get_job(self.workspace.id, job["job_id"], cursor=first["cursor"])
        )
        self.assertEqual(second["output"], "")
        self.assertEqual(
            self.daemon.last_request("GET", r"/jobs/[^/]+$")["query"]["cursor"],
            str(first["cursor"]),
        )

    def test_cancel_job(self):
        """cancel_job cancels the job and reports it."""
        job = json.loads(self.tools.start_job(self.workspace.id, "echo done"))
        result = self.tools.cancel_job(self.workspace.id, job["job_id"])
        self.assertIn("cancelled", result.lower())
        self.assertEqual(
            json.loads(self.tools.get_job(self.workspace.id, job["job_id"]))["status"],
            "cancelled",
        )


class CommitTests(DaemonTestCase):
    def test_commit_and_pr(self):
        """commit_and_pr returns a server-chosen branch and an MR URL."""
        self.tools.write_file(self.workspace.id, "notes.txt", "hello\n")
        data = json.loads(
            self.tools.commit_and_pr(
                self.workspace.id, "test: add notes", "Add notes", repo_url=REPO_URL
            )
        )
        self.assertTrue(data["branch"].startswith("mcwsd/agent-"))
        self.assertTrue(data["mr_url"].startswith("https://"))
        self.assertNotIn("error", data)


# case id -> "Class.method". test_inventory.py checks these against the
# inventory of record, so a test without a case (or the reverse) fails the run.
CASES = {
    "ProvisionTests.test_blank_workspace": "prep.blank",
    "ProvisionTests.test_repo_workspace": "prep.repo",
    "ProvisionTests.test_inline_provision_returns_body_unchanged": "prep.inline",
    "ProvisionTests.test_env_overrides_are_forwarded": "prep.env-overrides",
    "ProvisionTests.test_workspace_token_is_bound_after_prepare": "prep.token-bound",
    "AsyncProvisionTests.test_async_provision_is_polled_to_completion": "prep.async-poll",
    "LifecycleTests.test_status_alive": "life.status",
    "LifecycleTests.test_destroy": "life.destroy",
    "AuthTests.test_admin_token_works_in_scoped_mode": "auth.admin-scoped",
    "AuthTests.test_request_headers": "auth.header-shape",
    "StrictModeAuthTests.test_workspace_token_works_in_strict_mode": "auth.workspace-token",
    "ExecTests.test_exit_zero": "exec.exit-zero",
    "ExecTests.test_nonzero_exit_is_reported_not_raised": "exec.exit-nonzero",
    "ExecTests.test_payload_is_forwarded": "exec.payload",
    "ExecTests.test_default_timeout_uses_the_valve": "exec.default-timeout",
    "ExecTests.test_promoted_command_returns_a_job_id": "exec.promoted-job",
    "FileTests.test_write_then_read": "files.write-read",
    "FileTests.test_overwrite": "files.overwrite",
    "FileTests.test_safe_edit": "files.edit",
    "FileTests.test_safe_edit_replaces_every_occurrence": "files.edit-all-occurrences",
    "FileTests.test_list_files_entries": "files.list",
    "FileTests.test_relative_path_resolves_against_work_dir": "files.list-relative",
    "FileTests.test_absolute_path_inside_repo": "files.absolute-path",
    "FileTests.test_absolute_path_at_work_dir_parent_is_rerooted": "files.rerooted-path",
    "ArtifactTests.test_unchanged_workspace_has_no_artifacts": "artifacts.empty",
    "ArtifactTests.test_new_file_appears_as_an_artifact": "artifacts.after-write",
    "ArtifactTests.test_paths_are_forwarded": "artifacts.scoped",
    "JobTests.test_start_job": "jobs.start",
    "JobTests.test_list_jobs": "jobs.list",
    "JobTests.test_get_job": "jobs.get",
    "JobTests.test_cursor_suppresses_seen_output": "jobs.cursor",
    "JobTests.test_cancel_job": "jobs.cancel",
    "CommitTests.test_commit_and_pr": "commit.pr",
}

# Test classes in this module that intentionally have no case mapping.
UNMAPPED = {"DaemonTestCase"}
