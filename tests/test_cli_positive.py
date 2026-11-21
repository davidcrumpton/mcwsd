"""Positive cases for the CLI script, run as a subprocess.

These tests do not import ``test_mcwsd_cli`` — they execute it, exactly as an
operator would. That is what makes the exit code and the printed report part of
the contract: a step that silently "succeeds" while doing nothing is a failure
here, not a green tick in a log nobody reads.
"""

import re
import unittest

from tests.support.fake_daemon import FakeDaemon
from tests.support.harness import output_of, run_cli

REPO_URL = "https://github.com/example/demo.git"
WORKSPACE_ID_RE = re.compile(r"ws-[0-9a-f]{6,}")


class CliTestCase(unittest.TestCase):
    daemon_options = {}

    def setUp(self):
        self.daemon = FakeDaemon(**self.daemon_options).start()
        self.addCleanup(self.daemon.stop)
        self.workspace = self.daemon.create_workspace(repo_url=REPO_URL)

    def cli(self, *args, **kwargs):
        return run_cli(*args, daemon=self.daemon, **kwargs)

    def assertOk(self, result):
        self.assertEqual(result.returncode, 0, output_of(result))

    def assertFailed(self, result):
        self.assertNotEqual(result.returncode, 0, output_of(result))


class UsageTests(CliTestCase):
    def test_help_lists_every_subcommand(self):
        """--help documents the whole surface, including the new file commands."""
        result = self.cli("--help")
        self.assertOk(result)
        for command in (
            "prepare",
            "exec",
            "artifacts",
            "status",
            "destroy",
            "list-files",
            "read-file",
            "write-file",
            "edit-file",
            "file-roundtrip",
            "full-test",
        ):
            self.assertIn(command, result.stdout)


class PrepareTests(CliTestCase):
    def test_prepare_creates_a_workspace(self):
        """prepare reports the workspace it created and exits 0."""
        result = self.cli("prepare", REPO_URL)
        self.assertOk(result)
        self.assertIn("Workspace created:", result.stdout)
        match = WORKSPACE_ID_RE.search(result.stdout)
        self.assertIsNotNone(match, output_of(result))
        self.assertIsNotNone(self.daemon.workspace(match.group(0)))

    def test_prepare_without_a_repo_creates_a_blank_workspace(self):
        """A blank workspace needs no git remote."""
        result = self.cli("prepare")
        self.assertOk(result)
        self.assertIn("blank", result.stdout)


class ExecTests(CliTestCase):
    def test_exec_runs_a_command(self):
        """exec reports the exit code and exits 0."""
        result = self.cli("exec", self.workspace.id, "echo hello")
        self.assertOk(result)
        self.assertIn("hello", result.stdout)
        self.assertIn("exit code 0", result.stdout)

    def test_expect_exit_makes_a_nonzero_command_successful(self):
        """--expect-exit is how a deliberate non-zero exit is asserted."""
        result = self.cli("exec", self.workspace.id, "exit 3", "--expect-exit", "3")
        self.assertOk(result)
        self.assertIn("expected 3", result.stdout)


class FileVerificationTests(CliTestCase):
    def test_write_file_verifies_by_reading_back(self):
        """write-file does not trust the 204: it reads the file back."""
        content = "hello from the CLI\nsecond line\n"
        result = self.cli("write-file", self.workspace.id, "notes.txt", content)
        self.assertOk(result)
        self.assertIn("matches the expected content", result.stdout)
        self.assertEqual(self.workspace.files["notes.txt"], content)

    def test_edit_file_verifies_the_replacement(self):
        """edit-file proves the edit landed instead of echoing the response."""
        self.workspace.files["notes.txt"] = "status: draft\n"
        result = self.cli(
            "edit-file", self.workspace.id, "notes.txt", "status: draft", "status: final"
        )
        self.assertOk(result)
        self.assertIn("edited and verified", result.stdout)
        self.assertEqual(self.workspace.files["notes.txt"], "status: final\n")

    def test_file_roundtrip(self):
        """file-roundtrip writes, reads, edits, verifies and lists one file."""
        result = self.cli("file-roundtrip", self.workspace.id)
        self.assertOk(result)
        self.assertIn("file round-trip passed", result.stdout)
        created = [name for name in self.workspace.files if name.startswith("mcwsd_roundtrip_")]
        self.assertEqual(len(created), 1, output_of(result))
        self.assertIn("status: edited-", self.workspace.files[created[0]])

    def test_artifacts_expect_path(self):
        """artifacts --expect-path passes when the file is in the diff."""
        self.workspace.files["notes.txt"] = "hello\n"
        result = self.cli("artifacts", self.workspace.id, "--expect-path", "notes.txt")
        self.assertOk(result)
        self.assertIn("artifacts contain 'notes.txt'", result.stdout)


class FullTestTests(CliTestCase):
    def created_workspace(self, result):
        """Return the fake workspace the CLI's prepare step created."""
        match = WORKSPACE_ID_RE.search(result.stdout)
        self.assertIsNotNone(match, output_of(result))
        workspace = self.daemon.workspace(match.group(0))
        self.assertIsNotNone(workspace, output_of(result))
        return workspace

    def test_full_test_passes_and_cleans_up(self):
        """full-test runs every step, summarises, and destroys the workspace."""
        result = self.cli("full-test", REPO_URL)
        self.assertOk(result)
        self.assertIn("FULL TEST PASSED", result.stdout)
        self.assertRegex(result.stdout, r"SUMMARY: \d+/\d+ steps passed")
        self.assertIn("file round-trip passed", result.stdout)
        self.assertIn("edited and verified", result.stdout)
        self.assertTrue(self.created_workspace(result).destroyed, output_of(result))

    def test_full_test_keep_leaves_the_workspace_running(self):
        """--keep is for debugging, so the workspace must survive."""
        result = self.cli("full-test", REPO_URL, "main", "--keep")
        self.assertOk(result)
        self.assertIn("Keeping workspace", result.stdout)
        self.assertFalse(self.created_workspace(result).destroyed, output_of(result))


CASES = {
    "UsageTests.test_help_lists_every_subcommand": "cli.help",
    "PrepareTests.test_prepare_creates_a_workspace": "cli.prepare",
    "PrepareTests.test_prepare_without_a_repo_creates_a_blank_workspace": "cli.prepare-blank",
    "ExecTests.test_exec_runs_a_command": "cli.exec",
    "ExecTests.test_expect_exit_makes_a_nonzero_command_successful": "cli.exec-expect-exit",
    "FileVerificationTests.test_write_file_verifies_by_reading_back": "cli.write-verify",
    "FileVerificationTests.test_edit_file_verifies_the_replacement": "cli.edit-verify",
    "FileVerificationTests.test_file_roundtrip": "cli.file-roundtrip",
    "FileVerificationTests.test_artifacts_expect_path": "cli.artifacts-expect-path",
    "FullTestTests.test_full_test_passes_and_cleans_up": "cli.full-test",
    "FullTestTests.test_full_test_keep_leaves_the_workspace_running": "cli.full-test-keep",
}

UNMAPPED = {"CliTestCase"}
