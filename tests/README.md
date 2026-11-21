# tests/

Automated tests for the mcwsd Python tools: `mcwsd.py`, `test_mcwsd_cli.py` and
`test_mcwsd_functions.py`, which live in `tools/openwebui/mcwsd/`.

## Status

| Path | Status | Purpose |
|------|--------|---------|
| `tools/openwebui/mcwsd/mcwsd.py` | **current** | The OpenWebUI tool the tests exercise |
| `tools/openwebui/mcwsd/test_mcwsd_cli.py` | **current** | Operator CLI; also the subject of §8 |
| `tools/openwebui/mcwsd/test_mcwsd_functions.py` | **current** | Per-function script; also the subject of §9 |
| `tests/` (here) | **current** | Offline suite, run by `python -m tests` |
| `tests/test_live.py` | **opt-in** | Real daemon only; skips unless configured |
| `internal/**/*_test.go`, `pkg/**/*_test.go` | **current** | Go tests, unchanged — this directory holds no Go files |

One command runs everything that can be automated:

```sh
cd <repo root>
venv/bin/python -m tests        # ~13 s, offline, no daemon, no container runtime
```

`python -m unittest discover -s tests -t .` runs the same thing, as does
`python tests/__main__.py` from any directory. Use the repo's `venv/`
interpreter (or any Python with `tools/openwebui/mcwsd/requirements.txt`
installed); `python -m tests` says so plainly if the dependencies are missing
rather than failing with eighty import errors.

`go test ./...` is unaffected: `tests/` contains no Go files, so the Go tool
skips it.

## Coverage

82 recorded cases, each mapped to a test. `case_inventory.py` is the inventory
of record; `test_inventory.py` is the gate that keeps it honest.

| Section | Positive | Negative | Cases |
|---------|---------:|---------:|------:|
| 1. Provisioning & workspace lifecycle | 8 | 3 | **11** |
| 2. Auth & workspace tokens | 3 | 3 | **6** |
| 3. Command execution | 5 | 1 | **6** |
| 4. File tools (write / read / edit / list) | 8 | 5 | **13** |
| 5. Artifacts | 3 | 1 | **4** |
| 6. Background jobs | 5 | 2 | **7** |
| 7. `commit_and_pr` | 1 | 3 | **4** |
| 8. CLI script contract (`test_mcwsd_cli.py`) | 11 | 6 | **17** |
| 9. Function-test script contract (`test_mcwsd_functions.py`) | 7 | 3 | **10** |
| 10. Transport & error reporting | 0 | 4 | **4** |
| **Total** | **51** | **31** | **82** |

Section 10 is negative by design (error reporting has no happy path) and is
declared as such in `case_inventory.NEGATIVE_ONLY_SECTIONS`.

## Layout

```
tests/                                   # repo root, next to internal/ and pkg/
  case_inventory.py          inventory of record: (case id, section, kind, description)
  test_inventory.py          the coverage gate
  test_contract_positive.py  mcwsd.Tools happy paths         (sections 1-7)
  test_contract_negative.py  mcwsd.Tools failure paths       (sections 1-7, 10)
  test_cli_positive.py       test_mcwsd_cli.py via subprocess (section 8)
  test_cli_negative.py       test_mcwsd_cli.py failures      (section 8)
  test_functions_positive.py test_mcwsd_functions.py         (section 9)
  test_functions_negative.py test_mcwsd_functions.py         (section 9)
  test_live.py               opt-in, real daemon
  support/
    fake_daemon.py           in-process HTTP daemon speaking the real contract
    harness.py               subprocess runners + a Tools client factory

tools/openwebui/mcwsd/
  mcwsd.py                   the code under test
  test_mcwsd_cli.py          the CLI under test (run as a subprocess)
  test_mcwsd_functions.py    the function script under test (run as a subprocess)
  README_TESTING.md          operator docs for both scripts
```

`support/harness.py` resolves the tools directory from its own location, so the
suite works from any working directory and the two never drift apart silently.

## Why a fake daemon instead of mocking `requests`

The subject is a *client*, and the bugs live in the wire contract: the
Authorization header, the JSON field names, whether a 202 means "poll me", what
a 500 body looks like. A mock returns whatever the test author typed and proves
none of that. `support/fake_daemon.py` serves the same routes as
`internal/api/handlers.go` with the same auth rules as `internal/api/auth.go`,
so the CLI scripts are executed for real (as subprocesses) while the suite stays
offline.

Its limits, so nobody mistakes it for the daemon:

- files live in a dict; there is no git, and `artifacts` is a diff against the
  snapshot taken at prepare time;
- `exec` understands `echo`, `pwd`, `cat`, `ls`, `test -f`, `exit`, `whoami`,
  `sleep`, `true`/`false` — it is not a shell;
- provisioning is instantaneous; `async` jobs report `running` for
  `provision_polls` polls so the client's polling loop is exercised.

Anything the fake cannot express belongs in `test_live.py`.

## Rules for tests in this directory

> The suite must run on a random Tuesday, offline, in one command.

- **No network, no daemon, no container runtime** for the default path. Only
  `test_live.py` may reach outside, and it skips itself when it cannot.
- **Assert on exit codes, request payloads, and state on the daemon** — not on
  prose the tool prints. Prose is for humans; the exit code is the contract.
- **Every write is verified by reading it back.** `write_file` returning
  "written successfully" only means the daemon accepted the PUT. The scripts
  read the file again, and `cli.write-verify-mismatch` /
  `fn.write-verify-mismatch` prove a silently dropped write fails the run.
- **A negative case asserts the failure mode**, not just that something failed:
  401 vs 500 vs connection refused are different bugs.
- **Every test maps to a case, and every case maps to a test.** The gate fails
  otherwise — see below.

## The coverage gate

`test_inventory.py` cross-checks `case_inventory.CASES` against the `CASES`
mapping each test module declares. It fails when:

- a case in the inventory has no test,
- a mapped test method no longer exists,
- a mapped case id is not in the inventory,
- the inventory contains a duplicate case id,
- a test method exists that is mapped to no case,
- a case names an unknown section or kind, or has no description,
- a section (other than a declared negative-only one) lacks a positive or a
  negative case.

Two consequences worth remembering:

1. **Renaming a case to match a test defeats the gate.** Add or retire cases
   deliberately, in the inventory, in the same commit.
2. **The gate only scans `PORTED_MODULES`.** A new test module is unchecked
   until it is listed there — add it with the file.

## Adding a case

1. Add `("my.case", section, POSITIVE|NEGATIVE, "what it asserts")` to
   `case_inventory.CASES`.
2. Write the test in the matching positive/negative module.
3. Map it: `"MyTests.test_something": "my.case"` in that module's `CASES`.
4. Run `venv/bin/python -m tests` from the repo root.

## Live tests

```sh
cd <repo root>
export MCWSD_LIVE_BASE_URL=http://127.0.0.1:9080
export MCWSD_LIVE_TOKEN=<admin token>
export MCWSD_LIVE_REPO=https://github.com/example/demo.git   # optional
venv/bin/python -m unittest tests.test_live -v
```

Without `MCWSD_LIVE_REPO` the live tests use a blank workspace, which needs no
forge credentials. They provision real workspaces and destroy them in
`addCleanup`, including when an assertion fails.
