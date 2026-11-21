"""Positive cases for the individual-function script, run as a subprocess."""

import unittest

from tests.support.fake_daemon import FakeDaemon
from tests.support.harness import output_of, run_functions

REPO_URL = "https://github.com/example/demo.git"


class FunctionsTestCase(unittest.TestCase):
    daemon_options = {}

    def setUp(self):
        self.daemon = FakeDaemon(**self.daemon_options).start()
        self.addCleanup(self.daemon.stop)
        self.workspace = self.daemon.create_workspace(repo_url=REPO_URL)

    def fn(self, *args, **kwargs):
        return run_functions(*args, daemon=self.daemon, **kwargs)

    def assertOk(self, result):
        self.assertEqual(result.returncode, 0, output_of(result))


class PrepareTests(FunctionsTestCase):
    def test_prepare(self):
        """prepare creates a workspace, prints its id and exits 0."""
        result = self.fn("prepare", REPO_URL)
        self.assertOk(result)
        self.assertIn("SUCCESS: workspace created", result.stdout)


class ExecTests(FunctionsTestCase):
    def test_expect_exit(self):
        """--expect-exit makes a deliberate non-zero exit code a success."""
        result = self.fn("exec", self.workspace.id, "exit 3", "--expect-exit", "3")
        self.assertOk(result)
        self.assertIn("exit code 3", result.stdout)


class FileTests(FunctionsTestCase):
    def test_write_file_verifies_by_reading_back(self):
        """write_file reads the file back before claiming success."""
        content = "hello from the function test\n"
        result = self.fn("write_file", self.workspace.id, "notes.txt", content)
        self.assertOk(result)
        self.assertIn("file written and verified", result.stdout)
        self.assertEqual(self.workspace.files["notes.txt"], content)

    def test_safe_edit_verifies_the_replacement(self):
        """safe_edit proves the edit landed."""
        self.workspace.files["notes.txt"] = "status: draft\n"
        result = self.fn(
            "safe_edit", self.workspace.id, "notes.txt", "status: draft", "status: final"
        )
        self.assertOk(result)
        self.assertIn("edited and verified", result.stdout)
        self.assertEqual(self.workspace.files["notes.txt"], "status: final\n")

    def test_file_roundtrip(self):
        """file_roundtrip writes, reads, edits, verifies and diffs one file."""
        result = self.fn("file_roundtrip", self.workspace.id)
        self.assertOk(result)
        self.assertIn("round-trip passed", result.stdout)
        self.assertEqual(self.workspace.files["mcwsd_roundtrip.txt"], "mcwsd roundtrip\nstatus: verified\n")


class AllTests(FunctionsTestCase):
    def test_all_passes_and_keeps_the_workspace(self):
        """all runs the whole set and leaves the workspace for inspection."""
        result = self.fn("all", self.workspace.id)
        self.assertOk(result)
        self.assertIn("ALL TESTS PASSED", result.stdout)
        self.assertRegex(result.stdout, r"SUMMARY: \d+/\d+ passed")
        self.assertFalse(self.workspace.destroyed, output_of(result))

    def test_all_destroy_tears_the_workspace_down(self):
        """--destroy is the only way `all` removes the workspace."""
        result = self.fn("all", self.workspace.id, "--destroy")
        self.assertOk(result)
        self.assertTrue(self.workspace.destroyed, output_of(result))


CASES = {
    "PrepareTests.test_prepare": "fn.prepare",
    "ExecTests.test_expect_exit": "fn.exec-expect-exit",
    "FileTests.test_write_file_verifies_by_reading_back": "fn.write-verify",
    "FileTests.test_safe_edit_verifies_the_replacement": "fn.safe-edit",
    "FileTests.test_file_roundtrip": "fn.file-roundtrip",
    "AllTests.test_all_passes_and_keeps_the_workspace": "fn.all",
    "AllTests.test_all_destroy_tears_the_workspace_down": "fn.all-destroy",
}

UNMAPPED = {"FunctionsTestCase"}
