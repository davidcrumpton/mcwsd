"""Negative cases for the CLI script.

The CLI's exit code is the contract, so these tests are mostly about *when it
exits non-zero* — including the case that matters most for the file tools: a
daemon that accepts a write and then loses it.
"""

import unittest

from tests.support.fake_daemon import FakeDaemon, unused_port
from tests.support.harness import output_of, run_cli

REPO_URL = "https://github.com/example/demo.git"


class CliFailureTestCase(unittest.TestCase):
    daemon_options = {}

    def setUp(self):
        self.daemon = FakeDaemon(**self.daemon_options).start()
        self.addCleanup(self.daemon.stop)
        self.workspace = self.daemon.create_workspace(repo_url=REPO_URL)

    def cli(self, *args, **kwargs):
        return run_cli(*args, daemon=self.daemon, **kwargs)

    def assertFailed(self, result):
        self.assertNotEqual(result.returncode, 0, output_of(result))


class UsageFailureTests(CliFailureTestCase):
    def test_no_command_prints_usage_and_fails(self):
        """Running with no sub-command is an error, not a no-op."""
        result = self.cli()
        self.assertFailed(result)
        self.assertIn("usage:", result.stdout.lower())

    def test_unknown_command_is_a_usage_error(self):
        """argparse rejects an unknown sub-command with exit 2."""
        result = self.cli("bogus-command")
        self.assertEqual(result.returncode, 2, output_of(result))
        self.assertIn("invalid choice", result.stderr)

    def test_missing_argument_is_a_usage_error(self):
        """A missing positional argument is caught before any HTTP happens."""
        result = self.cli("exec", self.workspace.id)
        self.assertEqual(result.returncode, 2, output_of(result))
        self.assertIn("required", result.stderr)
        self.assertEqual(self.daemon.requests, [])


class VerificationFailureTests(CliFailureTestCase):
    def test_write_the_daemon_drops_fails_the_read_back_check(self):
        """A 204 is not proof of a write: the read-back must disagree."""
        self.daemon.add_fault("PUT", r"/file$", 204, "", "text/plain")
        result = self.cli("write-file", self.workspace.id, "notes.txt", "hello\n")
        self.assertFailed(result)
        self.assertIn("read_file failed", result.stdout)

    def test_artifacts_expect_path_missing(self):
        """Asking for an artifact that is not there fails the command."""
        result = self.cli("artifacts", self.workspace.id, "--expect-path", "missing.txt")
        self.assertFailed(result)
        self.assertIn("do not contain", result.stdout)


class DeadDaemonTests(CliFailureTestCase):
    def test_full_test_against_a_dead_daemon_fails_cleanly(self):
        """No daemon means a failed run with a summary, not a traceback."""
        result = run_cli(
            "full-test",
            REPO_URL,
            base_url=f"http://127.0.0.1:{unused_port()}",
            token="irrelevant",
        )
        self.assertFailed(result)
        self.assertIn("FULL TEST FAILED", result.stdout)
        self.assertIn("cannot continue without a workspace", result.stdout)
        self.assertNotIn("Traceback", result.stderr)

    def test_prepare_with_a_bad_token_fails(self):
        """A 401 makes prepare exit non-zero and say why."""
        result = run_cli("prepare", REPO_URL, daemon=self.daemon, token="not-the-token")
        self.assertFailed(result)
        self.assertIn("401", result.stdout)


CASES = {
    "UsageFailureTests.test_no_command_prints_usage_and_fails": "cli.no-command",
    "UsageFailureTests.test_unknown_command_is_a_usage_error": "cli.unknown-command",
    "UsageFailureTests.test_missing_argument_is_a_usage_error": "cli.missing-argument",
    "VerificationFailureTests.test_write_the_daemon_drops_fails_the_read_back_check": "cli.write-verify-mismatch",
    "VerificationFailureTests.test_artifacts_expect_path_missing": "artifacts.expect-path-missing",
    "DeadDaemonTests.test_full_test_against_a_dead_daemon_fails_cleanly": "cli.full-test-dead-daemon",
    "DeadDaemonTests.test_prepare_with_a_bad_token_fails": "cli.bad-token",
}

UNMAPPED = {"CliFailureTestCase"}
