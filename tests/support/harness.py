"""Shared helpers for the mcwsd Python test suite.

Everything the tests need to talk to the code under test lives here so the
individual test modules stay about behaviour, not plumbing.
"""

import os
import subprocess
import sys
from pathlib import Path

TOOLS_DIR = Path(__file__).resolve().parents[2] / "tools" / "openwebui" / "mcwsd"
CLI_SCRIPT = TOOLS_DIR / "test_mcwsd_cli.py"
FUNCTIONS_SCRIPT = TOOLS_DIR / "test_mcwsd_functions.py"
MCWSD_MODULE = TOOLS_DIR / "mcwsd.py"
REPO_ROOT = Path(__file__).resolve().parents[2]

# Any MCWSD_* variable in the developer's shell would silently redirect the
# scripts at a real daemon, so the harness always starts from a clean slate.
_MCWSD_ENV_VARS = (
    "MCWSD_BASE_URL",
    "MCWSD_TOKEN",
    "MCWSD_WORKSPACE_TOKEN",
    "MCWSD_TIMEOUT",
    "MCWSD_PROVISION_TIMEOUT",
)

if str(TOOLS_DIR) not in sys.path:
    sys.path.insert(0, str(TOOLS_DIR))

# The code under test needs requests + pydantic. Without this check a wrong
# interpreter surfaces as dozens of unrelated import errors; with it, the
# message names the interpreter and the fix.
for _dependency in ("requests", "pydantic"):
    try:
        __import__(_dependency)
    except ImportError as _exc:  # pragma: no cover - depends on the interpreter
        raise ImportError(
            f"the mcwsd test suite needs {_dependency!r} but {sys.executable} cannot "
            f"import it ({_exc}). Install tools/openwebui/mcwsd/requirements.txt, or run "
            f"the suite with the repo's venv: venv/bin/python -m tests"
        ) from _exc


def clean_env():
    """Return os.environ minus every MCWSD_* variable."""
    env = {k: v for k, v in os.environ.items() if k not in _MCWSD_ENV_VARS}
    env["PYTHONUNBUFFERED"] = "1"
    return env


def run_script(script, *args, **kwargs):
    """Run one of the test scripts as a subprocess and capture its output.

    Running the real script (not an imported function) is what makes these
    tests end-to-end: argparse, the environment handling, the exit code and the
    printed report are all part of the contract.
    """
    daemon = kwargs.pop("daemon", None)
    base_url = kwargs.pop("base_url", None)
    token = kwargs.pop("token", None)
    timeout = kwargs.pop("timeout", 120)
    extra_env = kwargs.pop("extra_env", None) or {}
    cwd = kwargs.pop("cwd", str(TOOLS_DIR))
    if kwargs:
        raise TypeError(f"unexpected keyword arguments: {sorted(kwargs)}")

    env = clean_env()
    if daemon is not None:
        env["MCWSD_BASE_URL"] = daemon.base_url
        if token is None:
            token = daemon.admin_token
    elif base_url is not None:
        env["MCWSD_BASE_URL"] = base_url
    if token is not None:
        env["MCWSD_TOKEN"] = token
    env.update({k: str(v) for k, v in extra_env.items()})

    return subprocess.run(
        [sys.executable, str(script)] + [str(a) for a in args],
        env=env,
        cwd=cwd,
        capture_output=True,
        text=True,
        timeout=timeout,
    )


def run_cli(*args, **kwargs):
    """Run test_mcwsd_cli.py."""
    return run_script(CLI_SCRIPT, *args, **kwargs)


def run_functions(*args, **kwargs):
    """Run test_mcwsd_functions.py."""
    return run_script(FUNCTIONS_SCRIPT, *args, **kwargs)


def tools_client(daemon, token=None, workspace_token=None, timeout=10):
    """Return a Tools instance pointed at a fake daemon.

    `tools_client` deliberately goes through the public valves, exactly like an
    OpenWebUI deployment would, rather than patching the module.
    """
    from mcwsd import Tools

    tools = Tools()
    tools.valves.base_url = daemon.base_url
    tools.valves.token = daemon.admin_token if token is None else token
    tools.valves.timeout_seconds = timeout
    tools.valves.provision_timeout_seconds = 30
    if workspace_token:
        tools._bind_workspace_token(workspace_token[0], workspace_token[1])
    return tools


def output_of(result):
    """Combined stdout+stderr, for assertion messages that need context."""
    return f"--- stdout ---\n{result.stdout}\n--- stderr ---\n{result.stderr}"
