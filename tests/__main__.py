"""Run the whole mcwsd Python test suite.

    cd <repo root>
    python -m tests

Or, from any working directory::

    python /path/to/repo/tests/__main__.py

Both are equivalent to ``python -m unittest discover -s tests -t .`` run from
the repo root. Live tests skip themselves unless a daemon is configured.

The suite imports ``mcwsd.py``, which needs ``requests`` and ``pydantic``. The
repo's own interpreter already has them::

    venv/bin/python -m tests

Running under an interpreter without those dependencies would otherwise fail
as ~80 confusing errors, so it is checked up front.
"""

import sys
import unittest
from pathlib import Path

TESTS_DIR = Path(__file__).resolve().parent
REPO_ROOT = TESTS_DIR.parent

# Running this file directly puts tests/ on sys.path; discovery imports the
# modules as `tests.*`, so the repo root has to be importable instead.
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

REQUIRED = ("requests", "pydantic")


def missing_dependencies():
    """Return the names of required third-party modules that cannot be imported."""
    missing = []
    for name in REQUIRED:
        try:
            __import__(name)
        except ImportError:
            missing.append(name)
    return missing


def main():
    missing = missing_dependencies()
    if missing:
        print(
            f"error: {sys.executable} cannot import {', '.join(missing)}.\n"
            f"       These come from tools/openwebui/mcwsd/requirements.txt.\n"
            f"       The repo's venv already has them:  venv/bin/python -m tests",
            file=sys.stderr,
        )
        return 2

    suite = unittest.TestLoader().discover(
        start_dir=str(TESTS_DIR),
        pattern="test_*.py",
        top_level_dir=str(REPO_ROOT),
    )
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    return 0 if result.wasSuccessful() else 1


if __name__ == "__main__":
    sys.exit(main())
