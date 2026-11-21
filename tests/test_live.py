"""Opt-in live tests against a real mcwsd daemon.

This is the only module in the suite that provisions anything for real, so it
is deliberately excluded from the default run and from the inventory gate:

* it skips unless ``MCWSD_LIVE_BASE_URL`` and ``MCWSD_LIVE_TOKEN`` are set,
* it skips again if the daemon does not answer,
* it cleans up every workspace it creates, even when a test fails.

    export MCWSD_LIVE_BASE_URL=http://127.0.0.1:9080
    export MCWSD_LIVE_TOKEN=<admin token>
    export MCWSD_LIVE_REPO=https://github.com/example/demo.git   # optional
    python -m unittest tests.test_live -v

With ``MCWSD_LIVE_REPO`` unset the tests use a blank workspace, which needs no
forge credentials.
"""

import json
import os
import unittest

import requests

from tests.support.harness import output_of, run_cli

LIVE_BASE_URL = os.environ.get("MCWSD_LIVE_BASE_URL", "").rstrip("/")
LIVE_TOKEN = os.environ.get("MCWSD_LIVE_TOKEN", "")
LIVE_REPO = os.environ.get("MCWSD_LIVE_REPO", "")
LIVE_REF = os.environ.get("MCWSD_LIVE_REF", "main")
LIVE_TIMEOUT = int(os.environ.get("MCWSD_LIVE_TIMEOUT", "1800"))

SKIP_REASON = (
    "live tests need a running daemon: set MCWSD_LIVE_BASE_URL and MCWSD_LIVE_TOKEN"
)


def setUpModule():
    """Skip the whole module unless a real daemon is configured and reachable."""
    if not (LIVE_BASE_URL and LIVE_TOKEN):
        raise unittest.SkipTest(SKIP_REASON)
    try:
        # Any answer at all — including 401 — proves something is listening.
        requests.get(f"{LIVE_BASE_URL}/workspaces/ws-probe/status", timeout=5)
    except requests.exceptions.RequestException as exc:
        raise unittest.SkipTest(f"live daemon at {LIVE_BASE_URL} is not reachable: {exc}")


def live_tools(token=None):
    """A Tools instance pointed at the live daemon."""
    from mcwsd import Tools

    tools = Tools()
    tools.valves.base_url = LIVE_BASE_URL
    tools.valves.token = LIVE_TOKEN if token is None else token
    tools.valves.timeout_seconds = 120
    tools.valves.provision_timeout_seconds = LIVE_TIMEOUT
    return tools


@unittest.skipUnless(LIVE_BASE_URL and LIVE_TOKEN, SKIP_REASON)
class LiveWorkspaceTests(unittest.TestCase):
    def setUp(self):
        self.tools = live_tools()
        raw = self.tools.prepare_workspace(LIVE_REPO, LIVE_REF)
        try:
            self.prepared = json.loads(raw)
        except json.JSONDecodeError:
            self.fail(f"prepare_workspace did not return JSON: {raw}")
        self.workspace_id = self.prepared["workspace_id"]
        self.addCleanup(self.tools.destroy_workspace, self.workspace_id)

    def test_prepare_exec_status(self):
        """A real workspace comes up, runs a command and reports healthy."""
        self.assertTrue(self.workspace_id.startswith("ws-"))
        self.assertIn("os", self.prepared)
        self.assertIn("work_dir", self.prepared)

        status = json.loads(self.tools.workspace_status(self.workspace_id))
        self.assertEqual(status["status"], "alive")

        exec_result = json.loads(
            self.tools.exec_command(self.workspace_id, "echo live && pwd", allow_network=False)
        )
        self.assertEqual(exec_result["exit_code"], 0)
        self.assertIn("live", exec_result["stdout"])

    def test_file_roundtrip(self):
        """Write, read back, edit, verify and diff a real file."""
        name = "mcwsd_live_roundtrip.txt"
        original = "mcwsd live roundtrip\nstatus: draft\n"
        expected = "mcwsd live roundtrip\nstatus: verified\n"

        self.assertIn("written successfully", self.tools.write_file(self.workspace_id, name, original))
        self.assertEqual(json.loads(self.tools.read_file(self.workspace_id, name))["content"], original)

        edit = json.loads(
            self.tools.safe_edit(self.workspace_id, name, "status: draft", "status: verified")
        )
        self.assertTrue(edit["success"])
        self.assertEqual(json.loads(self.tools.read_file(self.workspace_id, name))["content"], expected)

        artifacts = json.loads(self.tools.get_artifacts(self.workspace_id))["artifacts"]
        self.assertIn(name, [artifact["name"] for artifact in artifacts])

    def test_unknown_workspace_is_reported(self):
        """A workspace that does not exist is an error, not an empty success."""
        result = self.tools.workspace_status("ws-doesnotexist")
        self.assertTrue(result.lstrip().startswith("Error "), result)


@unittest.skipUnless(LIVE_BASE_URL and LIVE_TOKEN, SKIP_REASON)
class LiveAuthTests(unittest.TestCase):
    def test_bad_token_is_rejected(self):
        """A wrong admin token cannot provision anything."""
        tools = live_tools(token="definitely-not-the-token")
        result = tools.prepare_workspace(LIVE_REPO, LIVE_REF)
        self.assertIn("401", result)


@unittest.skipUnless(LIVE_BASE_URL and LIVE_TOKEN, SKIP_REASON)
class LiveCliTests(unittest.TestCase):
    def test_cli_full_test(self):
        """The CLI's full workflow (including write/edit/verify) works live."""
        result = run_cli(
            "full-test",
            LIVE_REPO,
            LIVE_REF,
            base_url=LIVE_BASE_URL,
            token=LIVE_TOKEN,
            timeout=LIVE_TIMEOUT,
        )
        self.assertEqual(result.returncode, 0, output_of(result))
        self.assertIn("FULL TEST PASSED", result.stdout)
        self.assertIn("edited and verified", result.stdout)
