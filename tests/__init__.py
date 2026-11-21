"""Automated tests for the mcwsd Python tools.

The suite is split the way the rest of this repo splits tests: every case is
either *positive* (the contract holds) or *negative* (the failure is reported,
not swallowed). ``test_inventory.py`` is the coverage gate — a case listed in
``case_inventory.CASES`` without a test, or a test without a case, fails the
run.

Run it from the repository root::

    python -m tests            # or: python -m unittest discover -s tests -t .

Nothing here needs a daemon, a container runtime, a git remote, or network
access: ``support/fake_daemon.py`` speaks the real HTTP contract in-process.

The Go tests are separate and unaffected: this directory holds no Go files, so
``go test ./...`` skips it.
"""
