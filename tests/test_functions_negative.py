"""Negative cases for the individual-function script."""

import unittest

from tests.support.fake_daemon import FakeDaemon
from tests.support.harness import output_of, run_functions

REPO_URL = "https://github.com/example/demo.git"


class FunctionsFailureTestCase(unittest.TestCase):
    daemon_options = {}

    def setUp(self):
        self.daemon = FakeDaemon(**self.daemon_options).start()
        self.addCleanup(self.daemon.stop)
        self.workspace = self.daemon.create_workspace(repo_url=REPO_URL)

    def fn(self, *args, **kwargs):
        return run_functions(*args, daemon=self.daemon, **kwargs)

    def assertFailed(self, result):
        self.assertNotEqual(result.returncode, 0, output_of(result))


class VerificationFailureTests(FunctionsFailureTestCase):
    def test_write_the_daemon_drops_fails_the_read_back_check(self):
        """A 204 with no stored file must not be reported as success."""
        self.daemon.add_fault("PUT", r"/file$", 204, "", "text/plain")
        result = self.fn("write_file", self.workspace.id, "notes.txt", "hello\n")
        self.assertFailed(result)
        self.assertIn("FAILED", result.stdout)


class UnknownWorkspaceTests(FunctionsFailureTestCase):
    def test_unknown_workspace(self):
        """A command against a workspace that does not exist exits 1."""
        result = self.fn("status", "ws-doesnotexist")
        self.assertFailed(result)
        self.assertIn("FAILED", result.stdout)
        self.assertIn("404", result.stdout)


class CommitWithoutForgeTests(FunctionsFailureTestCase):
    daemon_options = {"commit_and_pr": False}

    def test_commit_and_pr_without_forge_auth(self):
        """No forge auth means no PR, and the script says so."""
        result = self.fn("commit_and_pr", self.workspace.id, "msg", "title")
        self.assertFailed(result)
        self.assertIn("FAILED", result.stdout)
        self.assertIn("500", result.stdout)


CASES = {
    "VerificationFailureTests.test_write_the_daemon_drops_fails_the_read_back_check": "fn.write-verify-mismatch",
    "UnknownWorkspaceTests.test_unknown_workspace": "fn.unknown-workspace",
    "CommitWithoutForgeTests.test_commit_and_pr_without_forge_auth": "fn.commit-and-pr-no-forge",
}

UNMAPPED = {"FunctionsFailureTestCase"}
