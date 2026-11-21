"""Negative cases: failures are reported, not swallowed.

``mcwsd.Tools`` never raises for a transport or backend failure — it returns an
English ``Error ...`` string. That contract is only worth anything if the
string actually says what went wrong, so these tests assert on the failure
mode (401 vs 500 vs connection refused) and on the side effects that must *not*
happen, such as a write landing outside the workspace.
"""

import json
import unittest

from tests.support.fake_daemon import FakeDaemon, unused_port
from tests.support.harness import tools_client

REPO_URL = "https://github.com/example/demo.git"


class DaemonTestCase(unittest.TestCase):
    daemon_options = {}

    def setUp(self):
        self.daemon = FakeDaemon(**self.daemon_options).start()
        self.addCleanup(self.daemon.stop)
        self.tools = tools_client(self.daemon)
        self.workspace = self.daemon.create_workspace(repo_url=REPO_URL)

    def assertError(self, result, *fragments):
        """Assert a tool result is an error string mentioning each fragment."""
        self.assertIsInstance(result, str)
        self.assertTrue(
            result.lstrip().startswith("Error "), f"expected an Error string, got: {result!r}"
        )
        for fragment in fragments:
            self.assertIn(fragment, result)


class ProvisionFailureTests(DaemonTestCase):
    daemon_options = {"provision_fails": True}

    def test_failed_provision_returns_an_error(self):
        """A failed provision job is reported, not returned as a workspace."""
        result = self.tools.prepare_workspace(REPO_URL, "main")
        self.assertError(result, "provisioning failed", "clone failed")
        with self.assertRaises(json.JSONDecodeError):
            json.loads(result)


class LifecycleFailureTests(DaemonTestCase):
    def test_status_after_destroy_is_404(self):
        """A destroyed workspace is gone as far as status is concerned."""
        self.tools.destroy_workspace(self.workspace.id)
        self.assertError(self.tools.workspace_status(self.workspace.id), "404")

    def test_destroy_unknown_workspace_is_an_error(self):
        """Destroying an unknown workspace reports the backend error."""
        self.tools.destroy_workspace(self.workspace.id)
        self.assertError(self.tools.destroy_workspace(self.workspace.id), "500")


class AuthFailureTests(DaemonTestCase):
    def test_missing_token_is_rejected(self):
        """No token at all is a 401, not a silent anonymous call."""
        tools = tools_client(self.daemon, token="")
        self.assertError(tools.prepare_workspace(REPO_URL, "main"), "401")

    def test_bad_token_is_rejected(self):
        """A wrong token is a 401."""
        tools = tools_client(self.daemon, token="not-the-token")
        self.assertError(tools.prepare_workspace(REPO_URL, "main"), "401")


class StrictModeAuthFailureTests(DaemonTestCase):
    daemon_options = {"auth_mode": "strict"}

    def test_admin_token_is_refused_on_workspace_routes(self):
        """In strict mode the admin token cannot touch a workspace."""
        self.assertError(self.tools.workspace_status(self.workspace.id), "401")


class ExecFailureTests(DaemonTestCase):
    def test_empty_command_is_rejected(self):
        """An empty command is a 400 from the daemon."""
        self.assertError(
            self.tools.exec_command(self.workspace.id, "", allow_network=False), "400"
        )


class FileFailureTests(DaemonTestCase):
    def test_read_missing_file(self):
        """Reading a missing file is an error, not an empty string."""
        self.assertError(self.tools.read_file(self.workspace.id, "nope.txt"), "500")

    def test_edit_with_absent_pattern(self):
        """safe_edit refuses to pretend a no-op edit succeeded."""
        self.tools.write_file(self.workspace.id, "notes.txt", "status: draft\n")
        self.assertError(
            self.tools.safe_edit(self.workspace.id, "notes.txt", "absent", "x"), "500"
        )
        self.assertEqual(
            json.loads(self.tools.read_file(self.workspace.id, "notes.txt"))["content"],
            "status: draft\n",
        )

    def test_write_outside_the_workspace_is_rejected(self):
        """A path escaping the repo is refused and nothing is written."""
        self.assertError(self.tools.write_file(self.workspace.id, "../../etc/passwd", "x"), "500")
        self.assertNotIn("../../etc/passwd", self.workspace.files)
        self.assertEqual(self.workspace.artifacts(), [])

    def test_list_files_without_a_path(self):
        """An empty path is a 400, not a listing of the filesystem root."""
        self.assertError(self.tools.list_files(self.workspace.id, ""), "400")

    def test_write_file_without_a_path(self):
        """An empty path is a 400."""
        self.assertError(self.tools.write_file(self.workspace.id, "", "x"), "400")


class JobFailureTests(DaemonTestCase):
    def test_unknown_job(self):
        """Polling a job that does not exist is an error."""
        self.assertError(self.tools.get_job(self.workspace.id, "job-doesnotexist"), "500")

    def test_send_input_reports_unsupported_without_calling_the_daemon(self):
        """Background jobs have no stdin, and the tool says so locally."""
        job = json.loads(self.tools.start_job(self.workspace.id, "echo done"))
        before = len(self.daemon.requests)
        result = self.tools.send_process_input(self.workspace.id, job["job_id"], "yes\n")
        self.assertIn("not supported", result.lower())
        self.assertEqual(len(self.daemon.requests), before)


class CommitFailureTests(DaemonTestCase):
    def test_no_changes(self):
        """An unchanged workspace explains itself instead of returning an MR URL."""
        data = json.loads(
            self.tools.commit_and_pr(self.workspace.id, "msg", "title", repo_url=REPO_URL)
        )
        self.assertEqual(data["error"], "no changes to commit")
        self.assertEqual(data["mr_url"], "")

    def test_repo_url_mismatch(self):
        """A repo_url that is not the pinned one cannot redirect the push."""
        self.tools.write_file(self.workspace.id, "notes.txt", "hello\n")
        self.assertError(
            self.tools.commit_and_pr(
                self.workspace.id, "msg", "title", repo_url="https://github.com/other/thing.git"
            ),
            "500",
        )


class CommitWithoutForgeTests(DaemonTestCase):
    daemon_options = {"commit_and_pr": False}

    def test_no_forge_auth(self):
        """Without forge auth the daemon refuses, and the tool says why."""
        self.tools.write_file(self.workspace.id, "notes.txt", "hello\n")
        self.assertError(
            self.tools.commit_and_pr(self.workspace.id, "msg", "title", repo_url=REPO_URL),
            "500",
        )


class TransportTests(DaemonTestCase):
    def test_connection_refused(self):
        """A dead daemon yields an Error string, not a traceback."""
        tools = tools_client(self.daemon)
        tools.valves.base_url = f"http://127.0.0.1:{unused_port()}"
        self.assertError(tools.workspace_status("ws-anything"), "Error fetching status")

    def test_http_500_is_surfaced(self):
        """A 500 is reported with its status code."""
        self.daemon.add_fault("POST", r"/exec$", 500, "exec blew up")
        self.assertError(
            self.tools.exec_command(self.workspace.id, "echo hi", allow_network=False), "500"
        )

    def test_unauthorized_is_surfaced(self):
        """A 401 on a write is reported with its status code."""
        tools = tools_client(self.daemon, token="nope")
        self.assertError(tools.write_file(self.workspace.id, "notes.txt", "x"), "401")

    def test_non_json_body_is_returned_raw(self):
        """A non-JSON 200 body comes back as text instead of raising."""
        self.daemon.add_fault("GET", r"/file$", 200, "<html>not json</html>", "text/html")
        result = self.tools.read_file(self.workspace.id, "README.md")
        self.assertEqual(result, "<html>not json</html>")
        with self.assertRaises(json.JSONDecodeError):
            json.loads(result)


CASES = {
    "ProvisionFailureTests.test_failed_provision_returns_an_error": "prep.provision-failed",
    "LifecycleFailureTests.test_status_after_destroy_is_404": "life.destroy-then-status",
    "LifecycleFailureTests.test_destroy_unknown_workspace_is_an_error": "life.destroy-twice",
    "AuthFailureTests.test_missing_token_is_rejected": "auth.no-token",
    "AuthFailureTests.test_bad_token_is_rejected": "auth.bad-token",
    "StrictModeAuthFailureTests.test_admin_token_is_refused_on_workspace_routes": "auth.strict-admin-rejected",
    "ExecFailureTests.test_empty_command_is_rejected": "exec.empty-command",
    "FileFailureTests.test_read_missing_file": "files.read-missing",
    "FileFailureTests.test_edit_with_absent_pattern": "files.edit-missing-pattern",
    "FileFailureTests.test_write_outside_the_workspace_is_rejected": "files.write-outside",
    "FileFailureTests.test_list_files_without_a_path": "files.list-no-path",
    "FileFailureTests.test_write_file_without_a_path": "files.write-no-path",
    "JobFailureTests.test_unknown_job": "jobs.unknown",
    "JobFailureTests.test_send_input_reports_unsupported_without_calling_the_daemon": "jobs.stdin-unsupported",
    "CommitFailureTests.test_no_changes": "commit.no-changes",
    "CommitFailureTests.test_repo_url_mismatch": "commit.repo-mismatch",
    "CommitWithoutForgeTests.test_no_forge_auth": "commit.no-forge",
    "TransportTests.test_connection_refused": "transport.connection-refused",
    "TransportTests.test_http_500_is_surfaced": "transport.http-500",
    "TransportTests.test_unauthorized_is_surfaced": "transport.unauthorized",
    "TransportTests.test_non_json_body_is_returned_raw": "transport.non-json-body",
}

UNMAPPED = {"DaemonTestCase"}
