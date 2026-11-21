"""The inventory of record: every behaviour the suite claims to cover.

This is deliberately plain data in Python rather than a YAML fixture, for the
same reason the Go e2e suite keeps its checklist in Go source: there is no
fixture directory to lose, no generated artefact to regenerate, and a missing
entry is a failing test rather than a silent skip.

Each case names the section it belongs to and whether it is a positive case
(the documented behaviour happens) or a negative case (the failure is reported
in the documented way). ``test_inventory.py`` cross-checks this list against
the ``CASES`` mapping declared by each test module.
"""

SECTIONS = {
    1: "Provisioning & workspace lifecycle",
    2: "Auth & workspace tokens",
    3: "Command execution",
    4: "File tools (write / read / edit / list)",
    5: "Artifacts",
    6: "Background jobs",
    7: "commit_and_pr",
    8: "CLI script contract (test_mcwsd_cli.py)",
    9: "Function-test script contract (test_mcwsd_functions.py)",
    10: "Transport & error reporting",
}

POSITIVE = "positive"
NEGATIVE = "negative"

# Sections that are negative by design: error reporting has no happy path, so
# the gate must not demand a positive case there.
NEGATIVE_ONLY_SECTIONS = {10}

# (case id, section, kind, what the case asserts)
CASES = [
    # --- 1. Provisioning & workspace lifecycle ---------------------------
    ("prep.blank", 1, POSITIVE, "A blank workspace (no repo_url) is created and its token is bound."),
    ("prep.repo", 1, POSITIVE, "A repo workspace returns os/work_dir/profile/cache_hit."),
    ("prep.async-poll", 1, POSITIVE, "An async provision is polled to completion and returns the workspace JSON."),
    ("prep.inline", 1, POSITIVE, "run_async=False returns the inline body unchanged."),
    ("prep.env-overrides", 1, POSITIVE, "env_overrides are sent and reflected in the profile."),
    ("prep.token-bound", 1, POSITIVE, "After prepare, workspace calls carry the workspace token, not the admin token."),
    ("prep.provision-failed", 1, NEGATIVE, "A failed provision job returns an Error string, not a workspace."),
    ("life.status", 1, POSITIVE, "workspace_status reports the workspace as alive."),
    ("life.destroy", 1, POSITIVE, "destroy_workspace reports the workspace destroyed."),
    ("life.destroy-then-status", 1, NEGATIVE, "After destroy, status reports a 404 error."),
    ("life.destroy-twice", 1, NEGATIVE, "Destroying an unknown workspace reports the backend error."),

    # --- 2. Auth & tokens ------------------------------------------------
    ("auth.admin-scoped", 2, POSITIVE, "In scoped mode the admin token works on workspace routes (break-glass)."),
    ("auth.workspace-token", 2, POSITIVE, "In strict mode the workspace token works on workspace routes."),
    ("auth.header-shape", 2, POSITIVE, "Requests carry 'Bearer <token>' and a JSON content type."),
    ("auth.no-token", 2, NEGATIVE, "No token is rejected with 401."),
    ("auth.bad-token", 2, NEGATIVE, "A wrong token is rejected with 401."),
    ("auth.strict-admin-rejected", 2, NEGATIVE, "In strict mode the admin token is refused on workspace routes."),

    # --- 3. Command execution --------------------------------------------
    ("exec.exit-zero", 3, POSITIVE, "exec_command returns stdout/stderr/exit_code/duration_ms."),
    ("exec.exit-nonzero", 3, POSITIVE, "A non-zero exit code is reported, not raised."),
    ("exec.payload", 3, POSITIVE, "allow_network, timeout_seconds and secrets are forwarded."),
    ("exec.default-timeout", 3, POSITIVE, "timeout_seconds=0 falls back to the configured valve."),
    ("exec.promoted-job", 3, POSITIVE, "A promoted command returns a job_id instead of output."),
    ("exec.empty-command", 3, NEGATIVE, "An empty command is rejected by the daemon and surfaced as an error."),

    # --- 4. File tools ---------------------------------------------------
    ("files.write-read", 4, POSITIVE, "A written file reads back byte for byte."),
    ("files.overwrite", 4, POSITIVE, "Writing the same path twice replaces the content."),
    ("files.edit", 4, POSITIVE, "safe_edit replaces the literal pattern and reports success."),
    ("files.edit-all-occurrences", 4, POSITIVE, "safe_edit replaces every occurrence of the pattern."),
    ("files.list", 4, POSITIVE, "list_files returns name/path/is_dir/size entries."),
    ("files.list-relative", 4, POSITIVE, "A relative path resolves against work_dir."),
    ("files.absolute-path", 4, POSITIVE, "An absolute path inside the repo is accepted."),
    ("files.rerooted-path", 4, POSITIVE, "An absolute path at work_dir's parent is re-rooted inside the repo."),
    ("files.read-missing", 4, NEGATIVE, "Reading a missing file returns an error string."),
    ("files.edit-missing-pattern", 4, NEGATIVE, "safe_edit with an absent pattern returns an error."),
    ("files.write-outside", 4, NEGATIVE, "A path that escapes the workspace is rejected."),
    ("files.list-no-path", 4, NEGATIVE, "list_files with an empty path is rejected with 400."),
    ("files.write-no-path", 4, NEGATIVE, "write_file with an empty path is rejected with 400."),

    # --- 5. Artifacts ----------------------------------------------------
    ("artifacts.empty", 5, POSITIVE, "An unchanged workspace reports no artifacts."),
    ("artifacts.after-write", 5, POSITIVE, "A new file appears as an artifact, alongside changes.patch."),
    ("artifacts.scoped", 5, POSITIVE, "The paths argument is forwarded as a query parameter."),
    ("artifacts.expect-path-missing", 5, NEGATIVE, "Asking for an absent artifact path fails the check."),

    # --- 6. Background jobs ----------------------------------------------
    ("jobs.start", 6, POSITIVE, "start_job returns job_id/id/command/status."),
    ("jobs.list", 6, POSITIVE, "list_jobs returns jobs and the processes alias."),
    ("jobs.get", 6, POSITIVE, "get_job returns status, exit code and output."),
    ("jobs.cursor", 6, POSITIVE, "A cursor is forwarded and suppresses already-seen output."),
    ("jobs.cancel", 6, POSITIVE, "cancel_job cancels the job and reports it."),
    ("jobs.stdin-unsupported", 6, NEGATIVE, "send_process_input reports that stdin is unsupported."),
    ("jobs.unknown", 6, NEGATIVE, "get_job on an unknown id returns an error."),

    # --- 7. commit_and_pr ------------------------------------------------
    ("commit.pr", 7, POSITIVE, "commit_and_pr returns a branch and an MR URL."),
    ("commit.no-changes", 7, NEGATIVE, "An unchanged workspace reports 'no changes to commit' with no MR URL."),
    ("commit.no-forge", 7, NEGATIVE, "A daemon without forge auth reports the backend error."),
    ("commit.repo-mismatch", 7, NEGATIVE, "A repo_url that differs from the pinned one is rejected."),

    # --- 8. CLI script ---------------------------------------------------
    ("cli.help", 8, POSITIVE, "--help lists every sub-command and exits 0."),
    ("cli.no-command", 8, NEGATIVE, "No sub-command prints usage and exits non-zero."),
    ("cli.unknown-command", 8, NEGATIVE, "An unknown sub-command is a usage error (exit 2)."),
    ("cli.missing-argument", 8, NEGATIVE, "A missing positional argument is a usage error (exit 2)."),
    ("cli.prepare", 8, POSITIVE, "prepare creates a workspace and exits 0."),
    ("cli.prepare-blank", 8, POSITIVE, "prepare with no repo_url creates a blank workspace."),
    ("cli.exec", 8, POSITIVE, "exec runs a command and exits 0."),
    ("cli.exec-expect-exit", 8, POSITIVE, "--expect-exit makes a non-zero command exit code a success."),
    ("cli.write-verify", 8, POSITIVE, "write-file reads the file back and verifies the content."),
    ("cli.edit-verify", 8, POSITIVE, "edit-file verifies the replacement landed."),
    ("cli.file-roundtrip", 8, POSITIVE, "file-roundtrip writes, edits and verifies one file."),
    ("cli.artifacts-expect-path", 8, POSITIVE, "artifacts --expect-path passes when the file is present."),
    ("cli.full-test", 8, POSITIVE, "full-test runs every step, reports a summary and destroys the workspace."),
    ("cli.full-test-keep", 8, POSITIVE, "--keep leaves the workspace running."),
    ("cli.write-verify-mismatch", 8, NEGATIVE, "A write the daemon silently drops fails the read-back check."),
    ("cli.full-test-dead-daemon", 8, NEGATIVE, "full-test against a dead daemon exits 1 with a failure summary."),
    ("cli.bad-token", 8, NEGATIVE, "A bad admin token makes prepare exit 1."),

    # --- 9. Function-test script ----------------------------------------
    ("fn.prepare", 9, POSITIVE, "prepare prints the workspace id and exits 0."),
    ("fn.exec-expect-exit", 9, POSITIVE, "--expect-exit makes a non-zero command exit code a success."),
    ("fn.write-verify", 9, POSITIVE, "write_file reads the file back and verifies the content."),
    ("fn.safe-edit", 9, POSITIVE, "safe_edit verifies the replacement landed."),
    ("fn.file-roundtrip", 9, POSITIVE, "file_roundtrip exercises write -> read -> edit -> verify."),
    ("fn.all", 9, POSITIVE, "all runs every function test and leaves the workspace alive."),
    ("fn.all-destroy", 9, POSITIVE, "all --destroy tears the workspace down."),
    ("fn.write-verify-mismatch", 9, NEGATIVE, "A dropped write fails the read-back check."),
    ("fn.unknown-workspace", 9, NEGATIVE, "A command against an unknown workspace exits 1."),
    ("fn.commit-and-pr-no-forge", 9, NEGATIVE, "commit_and_pr without forge auth exits 1."),

    # --- 10. Transport & error reporting --------------------------------
    ("transport.connection-refused", 10, NEGATIVE, "A dead daemon produces an Error string, not a traceback."),
    ("transport.http-500", 10, NEGATIVE, "A 500 is surfaced with its status code."),
    ("transport.unauthorized", 10, NEGATIVE, "A 401 is surfaced with its status code."),
    ("transport.non-json-body", 10, NEGATIVE, "A non-JSON 200 body is returned raw instead of raising."),
]

CASE_IDS = [case[0] for case in CASES]
