package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/jobs"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
	"github.com/davidcrumpton/mcwsd/internal/state"
)

// localTransport is a runtime.ExecTransport that runs commands with the host
// shell, rooted at a temp directory. It lets the background-job machinery be
// exercised end to end (supervisor shim, detached launch, probes, stream
// reads and cancellation) without a container runtime. Absolute job-runtime
// paths are rewritten under root so tests never touch the host's /tmp.
type localTransport struct {
	root string

	mu         sync.Mutex
	netEnabled []bool
}

func (t *localTransport) rewrite(s string) string {
	return strings.ReplaceAll(s, jobRuntimeDir, t.root)
}

func (t *localTransport) Exec(ctx context.Context, cmd []string, workDir string, env []string) (*internal.ExecResult, error) {
	argv := make([]string, len(cmd))
	copy(argv, cmd)
	// cmd is expected to be ["sh","-c",script]; rewrite any job paths.
	for i, a := range argv {
		argv[i] = t.rewrite(a)
	}
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if workDir != "" {
		c.Dir = workDir
	}
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			return nil, err
		}
		code = exitErr.ExitCode()
	}
	return &internal.ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code}, nil
}

func (t *localTransport) SetNetwork(_ context.Context, enabled bool) error {
	t.mu.Lock()
	t.netEnabled = append(t.netEnabled, enabled)
	t.mu.Unlock()
	return nil
}

func (t *localTransport) networkCalls() []bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]bool(nil), t.netEnabled...)
}

func (t *localTransport) CopyDirIn(context.Context, string, string) error { return nil }

func (t *localTransport) ReadFile(_ context.Context, path string) ([]byte, error) {
	return os.ReadFile(t.rewrite(path))
}

func (t *localTransport) WriteFile(_ context.Context, path string, content []byte, mode int64) error {
	path = t.rewrite(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, content, os.FileMode(mode))
}

// newJobTestBackend builds a Backend with one workspace backed by a
// localTransport rooted at a temp directory.
func newJobTestBackend(t *testing.T) (*Backend, *internal.Workspace) {
	t.Helper()
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	store, err := state.NewStore(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatalf("state store: %v", err)
	}
	ws := &internal.Workspace{ID: "ws-test", WorkDir: repoDir}
	handle := &runtime.RuntimeHandle{
		OS:        runtime.OSLinux,
		Isolation: runtime.IsolationContainer,
		WorkDir:   repoDir,
		Transport: &localTransport{root: root},
	}
	b := &Backend{
		workspaces: map[string]*workspaceState{
			"ws-test": {workspace: ws, handle: handle, secrets: map[string]string{}},
		},
		processes:  make(map[string]*managedProcess),
		stateStore: store,
	}
	return b, ws
}

// newJobTestBackendWithStore is newJobTestBackend plus a host-side jobs store
// rooted in the same temp directory, and the given limits.
func newJobTestBackendWithStore(t *testing.T, limits jobs.Limits) (*Backend, *internal.Workspace) {
	t.Helper()
	b, ws := newJobTestBackend(t)
	root := b.workspaces["ws-test"].handle.Transport.(*localTransport).root
	store, err := jobs.NewStore(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatalf("jobs store: %v", err)
	}
	b.jobStore = store
	b.jobLimits = limits
	return b, ws
}

// pollProcess polls until the job is terminal or the deadline passes.
func pollProcess(t *testing.T, b *Backend, ws *internal.Workspace, id, cursor string) (internal.ProcessStatus, string, string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		res, err := b.GetProcessStatus(context.Background(), ws, id, cursor)
		if err != nil {
			t.Fatalf("GetProcessStatus: %v", err)
		}
		cursor = res.Cursor
		if isTerminalProcessStatus(res.Status) {
			return res.Status, cursor, res.Output
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not finish; status=%s output=%q", res.Status, res.Output)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestBackgroundJobLifecycle(t *testing.T) {
	b, ws := newJobTestBackend(t)
	ctx := context.Background()

	info, err := b.StartProcess(ctx, ws, internal.StartProcessRequest{
		Command: `printf 'hello\n'; printf 'oops\n' 1>&2; sleep 0.3; printf 'bye\n'`,
	})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	if info.Status != internal.ProcessQueued {
		t.Fatalf("new job status = %q, want queued", info.Status)
	}

	// Collect output incrementally using the returned cursor; polling twice
	// must not replay the same bytes.
	var collected strings.Builder
	cursor := ""
	var lastStatus internal.ProcessStatus
	deadline := time.Now().Add(15 * time.Second)
	for {
		res, err := b.GetProcessStatus(ctx, ws, info.ID, cursor)
		if err != nil {
			t.Fatalf("GetProcessStatus: %v", err)
		}
		collected.WriteString(res.Output)
		cursor = res.Cursor
		lastStatus = res.Status
		if isTerminalProcessStatus(res.Status) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not finish; status=%s", res.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastStatus != internal.ProcessSucceeded {
		t.Fatalf("final status = %q, want succeeded", lastStatus)
	}

	got := collected.String()
	for _, want := range []string{
		`"stream":"stdout"`, `"line":"hello"`,
		`"stream":"stderr"`, `"line":"oops"`,
		`"line":"bye"`, `"event":"exit"`, `"exit_code":0`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("combined log missing %q:\n%s", want, got)
		}
	}

	// Reading again after completion with the terminal cursor is a no-op,
	// and the record is still queryable.
	final, err := b.GetProcessStatus(ctx, ws, info.ID, cursor)
	if err != nil {
		t.Fatalf("GetProcessStatus after completion: %v", err)
	}
	if final.Output != "" {
		t.Errorf("expected no new output after terminal cursor, got %q", final.Output)
	}
	if !isTerminalProcessStatus(final.Status) {
		t.Errorf("record no longer queryable: status=%q", final.Status)
	}
}

func TestBackgroundJobCancelKeepsRecord(t *testing.T) {
	b, ws := newJobTestBackend(t)
	ctx := context.Background()

	info, err := b.StartProcess(ctx, ws, internal.StartProcessRequest{Command: "sleep 30"})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}

	// Wait until it is actually running.
	running := false
	for i := 0; i < 100; i++ {
		res, err := b.GetProcessStatus(ctx, ws, info.ID, "")
		if err != nil {
			t.Fatalf("GetProcessStatus: %v", err)
		}
		if res.Status == internal.ProcessRunning {
			running = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !running {
		t.Fatalf("job never reached running state")
	}

	if err := b.KillProcess(ctx, ws, info.ID); err != nil {
		t.Fatalf("KillProcess: %v", err)
	}

	status, cursor, _ := pollProcess(t, b, ws, info.ID, "")
	if status != internal.ProcessCancelled {
		t.Fatalf("status after kill = %q, want cancelled", status)
	}
	if cursor != "done" {
		t.Fatalf("cursor after terminal = %q, want done", cursor)
	}
	cancelled, err := b.GetProcessStatus(ctx, ws, info.ID, cursor)
	if err != nil {
		t.Fatalf("GetProcessStatus after cancel: %v", err)
	}
	if cancelled.ExitCode != exitCodeCancelled {
		t.Fatalf("cancelled exit code = %d, want %d", cancelled.ExitCode, exitCodeCancelled)
	}

	// The record must remain queryable after cancellation.
	res, err := b.GetProcessStatus(ctx, ws, info.ID, cursor)
	if err != nil {
		t.Fatalf("GetProcessStatus after kill: %v", err)
	}
	if res.Status != internal.ProcessCancelled {
		t.Fatalf("record lost after kill: status=%q", res.Status)
	}
}

func TestBackgroundJobCompletesDespiteOrphanedOutputHolder(t *testing.T) {
	b, ws := newJobTestBackend(t)
	ctx := context.Background()

	// The direct command exits but leaves a descendant that inherited
	// stdout/stderr and briefly outlives it. Recording the terminal state must
	// not depend on every descendant closing its fds, or the job would hang in
	// "running" forever (the OpenBSD no-setsid case). The supervisor bounds
	// the wait for its stream readers, so this job must still finish.
	info, err := b.StartProcess(ctx, ws, internal.StartProcessRequest{
		Command: "sleep 4 & echo done",
	})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}

	status, _, _ := pollProcess(t, b, ws, info.ID, "")
	if status != internal.ProcessSucceeded {
		t.Fatalf("status = %q, want succeeded", status)
	}

	// Re-read the whole log from the start: pollProcess only returns the final
	// poll's delta, and "done" may have been consumed by an earlier poll.
	final, err := b.GetProcessStatus(ctx, ws, info.ID, "")
	if err != nil {
		t.Fatalf("GetProcessStatus: %v", err)
	}
	if !strings.Contains(final.Output, `"line":"done"`) {
		t.Fatalf("output missing done: %q", final.Output)
	}
}

func TestBackgroundJobKillReapsForkedDescendants(t *testing.T) {
	b, ws := newJobTestBackend(t)
	ctx := context.Background()

	// The command forks a descendant that outlives the wrapper shell and would
	// write a marker after a short delay. Cancelling must signal the whole
	// process group, not just the wrapper, or the descendant survives and the
	// marker appears. On OpenBSD, "sh -c CMD" forks a wrapper and there is no
	// setsid(1), so this is the case the supervisor's job-control fallback
	// exists to handle.
	marker := filepath.Join(ws.WorkDir, "survived")
	info, err := b.StartProcess(ctx, ws, internal.StartProcessRequest{
		Command: fmt.Sprintf("( sleep 3; : > %s ) & sleep 30", shellQuote(marker)),
	})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}

	// Wait until it is actually running before cancelling.
	for i := 0; i < 100; i++ {
		res, err := b.GetProcessStatus(ctx, ws, info.ID, "")
		if err != nil {
			t.Fatalf("GetProcessStatus: %v", err)
		}
		if res.Status == internal.ProcessRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := b.KillProcess(ctx, ws, info.ID); err != nil {
		t.Fatalf("KillProcess: %v", err)
	}
	pollProcess(t, b, ws, info.ID, "")

	// Give the forked descendant enough time to have written the marker if it
	// had survived the cancel.
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("forked descendant survived cancellation and wrote its marker")
	}
}

func TestBackgroundJobCursorIsIncremental(t *testing.T) {
	b, ws := newJobTestBackend(t)
	ctx := context.Background()

	info, err := b.StartProcess(ctx, ws, internal.StartProcessRequest{
		Command: "printf 'alpha\\n'; sleep 0.6; printf 'beta\\n'",
	})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}

	var all strings.Builder
	cursor := ""
	deadline := time.Now().Add(15 * time.Second)
	for {
		res, err := b.GetProcessStatus(ctx, ws, info.ID, cursor)
		if err != nil {
			t.Fatalf("GetProcessStatus: %v", err)
		}
		// Each poll must return only new records, never repeat earlier ones.
		if strings.Count(res.Output, `"line":"alpha"`) > 1 {
			t.Fatalf("alpha was replayed within a single poll: %q", res.Output)
		}
		all.WriteString(res.Output)
		cursor = res.Cursor
		if isTerminalProcessStatus(res.Status) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not finish; status=%s", res.Status)
		}
		time.Sleep(100 * time.Millisecond)
	}

	got := all.String()
	if n := strings.Count(got, `"line":"alpha"`); n != 1 {
		t.Errorf("alpha appeared %d times across polls, want exactly 1:\n%s", n, got)
	}
	if n := strings.Count(got, `"line":"beta"`); n != 1 {
		t.Errorf("beta appeared %d times across polls, want exactly 1:\n%s", n, got)
	}
}

func TestBackgroundJobAllowNetwork(t *testing.T) {
	b, ws := newJobTestBackend(t)
	ft := b.workspaces["ws-test"].handle.Transport.(*localTransport)
	ctx := context.Background()

	info, err := b.StartProcess(ctx, ws, internal.StartProcessRequest{
		Command:      "printf 'x\\n'",
		AllowNetwork: true,
	})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	pollProcess(t, b, ws, info.ID, "")

	calls := ft.networkCalls()
	// Network must have been attached for the job and detached when it
	// finished.
	if len(calls) == 0 || calls[0] != true {
		t.Fatalf("expected network attach first, got %v", calls)
	}
	last := calls[len(calls)-1]
	if last != false {
		t.Fatalf("expected network detached after job, got %v", calls)
	}
}

func TestBackgroundJobTimeout(t *testing.T) {
	b, ws := newJobTestBackend(t)
	ctx := context.Background()

	info, err := b.StartProcess(ctx, ws, internal.StartProcessRequest{
		Command:        "sleep 30",
		TimeoutSeconds: 1,
	})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}

	status, cursor, _ := pollProcess(t, b, ws, info.ID, "")
	if status != internal.ProcessTimedOut {
		t.Fatalf("status after timeout = %q, want timed_out", status)
	}
	res, err := b.GetProcessStatus(ctx, ws, info.ID, cursor)
	if err != nil {
		t.Fatalf("GetProcessStatus after timeout: %v", err)
	}
	if res.ExitCode != exitCodeTimedOut {
		t.Fatalf("timed-out exit code = %d, want %d", res.ExitCode, exitCodeTimedOut)
	}
}

func TestParseProcessCursor(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want processCursor
	}{
		{name: "empty means beginning", in: "", want: processCursor{}},
		{name: "whitespace", in: "   ", want: processCursor{}},
		{name: "done", in: "done", want: processCursor{done: true}},
		{name: "offsets", in: "12:34", want: processCursor{outOffset: 12, errOffset: 34}},
		{name: "zero offsets", in: "0:0", want: processCursor{}},
		{name: "malformed", in: "nope", want: processCursor{}},
		{name: "one field", in: "12", want: processCursor{}},
		{name: "negative", in: "-1:5", want: processCursor{}},
		{name: "non-numeric", in: "a:b", want: processCursor{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseProcessCursor(tt.in); got != tt.want {
				t.Fatalf("parseProcessCursor(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestProcessCursorRoundTrip(t *testing.T) {
	for _, c := range []processCursor{
		{},
		{outOffset: 5, errOffset: 7},
		{done: true},
	} {
		if got := parseProcessCursor(c.String()); got != c {
			t.Fatalf("round trip of %+v through %q gave %+v", c, c.String(), got)
		}
	}
}

func TestStreamToLogCompleteLines(t *testing.T) {
	out, consumed := streamToLog("stdout", []byte("hello\nworld\n"))
	if consumed != int64(len("hello\nworld\n")) {
		t.Fatalf("consumed = %d, want %d", consumed, len("hello\nworld\n"))
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d records, want 2: %q", len(lines), out)
	}
	for i, want := range []string{"hello", "world"} {
		var rec processLogRecord
		if err := json.Unmarshal([]byte(lines[i]), &rec); err != nil {
			t.Fatalf("record %d not valid JSON: %v", i, err)
		}
		if rec.Stream != "stdout" || rec.Line != want {
			t.Fatalf("record %d = %+v, want stream=stdout line=%q", i, rec, want)
		}
		if rec.TS == "" {
			t.Fatalf("record %d has empty ts", i)
		}
	}
}

func TestStreamToLogPartialLineIsNotConsumed(t *testing.T) {
	// A trailing partial line must be left for the next poll so it is never
	// split across records.
	out, consumed := streamToLog("stderr", []byte("done\npartial"))
	if consumed != int64(len("done\n")) {
		t.Fatalf("consumed = %d, want %d", consumed, len("done\n"))
	}
	if !strings.Contains(out, `"line":"done"`) {
		t.Fatalf("missing complete line record: %q", out)
	}
	if strings.Contains(out, "partial") {
		t.Fatalf("partial line should not be emitted yet: %q", out)
	}
}

func TestStreamToLogNoNewline(t *testing.T) {
	out, consumed := streamToLog("stdout", []byte("no newline yet"))
	if out != "" || consumed != 0 {
		t.Fatalf("streamToLog without newline = (%q, %d), want empty and 0", out, consumed)
	}
}

func TestStreamToLogStripsCarriageReturn(t *testing.T) {
	out, _ := streamToLog("stdout", []byte("hi\r\n"))
	if strings.Contains(out, `\r`) {
		t.Fatalf("carriage return not stripped: %q", out)
	}
}

func TestExitLogRecord(t *testing.T) {
	var rec processLogRecord
	if err := json.Unmarshal([]byte(exitLogRecord(3)), &rec); err != nil {
		t.Fatalf("exit record not valid JSON: %v", err)
	}
	if rec.Event != "exit" || rec.ExitCode == nil || *rec.ExitCode != 3 {
		t.Fatalf("exit record = %+v, want event=exit exit_code=3", rec)
	}
}

func TestBuildJobLaunchCommand(t *testing.T) {
	cmd := buildJobLaunchCommand("/tmp/.wsd-jobs/proc-abc", `echo "hello; rm -rf /"`, 4096)

	// Must detach (setsid preferred, nohup fallback) and return immediately.
	for _, want := range []string{"setsid", "nohup", "exit 0", "</dev/null", ">/dev/null", " 2>&1 &"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("launch command missing %q:\n%s", want, cmd)
		}
	}
	// The job directory and supervisor path must be referenced.
	if !strings.Contains(cmd, "/tmp/.wsd-jobs/proc-abc") {
		t.Errorf("launch command missing job dir:\n%s", cmd)
	}
	if !strings.Contains(cmd, supervisorPath) {
		t.Errorf("launch command missing supervisor path:\n%s", cmd)
	}
	// The user command and the detached flag must be passed through as
	// separate, safely quoted arguments.
	if !strings.Contains(cmd, `'echo "hello; rm -rf /"'`) {
		t.Errorf("user command not single-quoted:\n%s", cmd)
	}
	// Detached flag and per-stream log limit are passed as separate args.
	if !strings.Contains(cmd, " 1 4096 </dev/null") {
		t.Errorf("setsid branch should pass detached flag 1 and limit 4096:\n%s", cmd)
	}
	if !strings.Contains(cmd, " 0 4096 </dev/null") {
		t.Errorf("nohup branch should pass detached flag 0 and limit 4096:\n%s", cmd)
	}
}

func TestBuildKillCommand(t *testing.T) {
	cmd := buildKillCommand("/tmp/.wsd-jobs/proc-abc")
	for _, want := range []string{"kill -TERM", "kill -KILL", "$d/pid", "$d/pgid", "exit 0"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("kill command missing %q:\n%s", want, cmd)
		}
	}
	// Group cancellation is attempted before the plain pid kill.
	if strings.Index(cmd, `kill -TERM -"$pgid"`) > strings.Index(cmd, `kill -TERM "$pid"`) {
		t.Errorf("expected group TERM before pid TERM:\n%s", cmd)
	}
}

func TestSupervisorScriptIsPosixAndRecordsState(t *testing.T) {
	for _, want := range []string{
		"#!/bin/sh",
		`"$jobdir/stdout"`,
		`"$jobdir/stderr"`,
		`"$jobdir/pid"`,
		`"$jobdir/exit_code"`,
		`"$jobdir/status"`,
		`"$jobdir/pgid"`,
		`"$jobdir/shim"`,
		"mkfifo",
		"out.fifo",
		"err.fifo",
		"wait",
	} {
		if !strings.Contains(jobSupervisorScript, want) {
			t.Errorf("supervisor script missing %q", want)
		}
	}
	// No bash-only constructs.
	for _, bad := range []string{"[[", "function ", "arr=("} {
		if strings.Contains(jobSupervisorScript, bad) {
			t.Errorf("supervisor script uses non-POSIX construct %q", bad)
		}
	}
}

func TestIsTerminalProcessStatus(t *testing.T) {
	terminal := []internal.ProcessStatus{
		internal.ProcessSucceeded, internal.ProcessFailed,
		internal.ProcessCancelled, internal.ProcessTimedOut,
	}
	for _, s := range terminal {
		if !isTerminalProcessStatus(s) {
			t.Errorf("isTerminalProcessStatus(%q) = false, want true", s)
		}
	}
	for _, s := range []internal.ProcessStatus{internal.ProcessQueued, internal.ProcessRunning} {
		if isTerminalProcessStatus(s) {
			t.Errorf("isTerminalProcessStatus(%q) = true, want false", s)
		}
	}
}

// --- Phase 2: durability, recovery and limits ---

func TestJobPerWorkspaceLimit(t *testing.T) {
	b, ws := newJobTestBackendWithStore(t, jobs.Limits{MaxPerWorkspace: 1})
	ctx := context.Background()

	info, err := b.StartProcess(ctx, ws, internal.StartProcessRequest{Command: "sleep 30"})
	if err != nil {
		t.Fatalf("first StartProcess: %v", err)
	}
	defer b.KillProcess(ctx, ws, info.ID)

	_, err = b.StartProcess(ctx, ws, internal.StartProcessRequest{Command: "echo second"})
	if err == nil {
		t.Fatal("second job should have been rejected by the per-workspace limit")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("rejection error should mention the limit, got: %v", err)
	}
}

func TestJobTotalLimit(t *testing.T) {
	b, ws := newJobTestBackendWithStore(t, jobs.Limits{MaxTotal: 1})
	ctx := context.Background()

	info, err := b.StartProcess(ctx, ws, internal.StartProcessRequest{Command: "sleep 30"})
	if err != nil {
		t.Fatalf("first StartProcess: %v", err)
	}
	defer b.KillProcess(ctx, ws, info.ID)

	if _, err := b.StartProcess(ctx, ws, internal.StartProcessRequest{Command: "echo second"}); err == nil {
		t.Fatal("second job should have been rejected by the total limit")
	}
}

func TestJobLogCap(t *testing.T) {
	const limit = 64
	b, ws := newJobTestBackendWithStore(t, jobs.Limits{MaxLogBytes: limit})
	ctx := context.Background()

	info, err := b.StartProcess(ctx, ws, internal.StartProcessRequest{
		Command: "i=0; while [ $i -lt 500 ]; do printf 'line-%03d-abcdefghij\\n' $i; i=$((i+1)); done",
	})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	pollProcess(t, b, ws, info.ID, "")

	// Inspect the on-disk raw stream: it must be capped (allowing for one
	// line of overshoot, since the reader stops after the line that crosses
	// the limit).
	ft := b.workspaces["ws-test"].handle.Transport.(*localTransport)
	raw := filepath.Join(ft.root, info.ID, "stdout")
	st, err := os.Stat(raw)
	if err != nil {
		t.Fatalf("stat capped stdout: %v", err)
	}
	if st.Size() > limit+64 {
		t.Fatalf("capped stdout is %d bytes, want <= %d", st.Size(), limit+64)
	}
}

func TestRecoverJobsMarksOrphanInterrupted(t *testing.T) {
	b, _ := newJobTestBackendWithStore(t, jobs.Limits{})
	ctx := context.Background()

	// A running job whose workspace was never recovered (not in memory).
	if err := b.jobStore.Put(jobs.Entry{
		ID: "proc-orphan", WorkspaceID: "ws-gone",
		Status: internal.ProcessRunning, StartedAt: time.Now(), SessionID: "old",
	}); err != nil {
		t.Fatal(err)
	}

	b.RecoverJobs(ctx)

	e, ok := b.jobStore.Get("proc-orphan")
	if !ok {
		t.Fatal("orphan entry vanished")
	}
	if e.Status != internal.ProcessFailed {
		t.Fatalf("orphan status = %q, want failed (interrupted)", e.Status)
	}
	if e.EndedAt.IsZero() {
		t.Fatal("interrupted job should have an end time")
	}
}

func TestRecoverJobsAdoptsRunningJob(t *testing.T) {
	b, ws := newJobTestBackendWithStore(t, jobs.Limits{MaxPerWorkspace: 1})
	ctx := context.Background()

	info, err := b.StartProcess(ctx, ws, internal.StartProcessRequest{Command: "sleep 30"})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	defer b.KillProcess(ctx, ws, info.ID)

	// Wait until it is actually running.
	for i := 0; i < 100; i++ {
		res, err := b.GetProcessStatus(ctx, ws, info.ID, "")
		if err != nil {
			t.Fatalf("GetProcessStatus: %v", err)
		}
		if res.Status == internal.ProcessRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Simulate a daemon restart: forget all in-memory processes but keep the
	// persisted record and the still-running runtime.
	b.procMu.Lock()
	b.processes = make(map[string]*managedProcess)
	b.procMu.Unlock()

	b.RecoverJobs(ctx)

	key := processKey(ws.ID, info.ID)
	b.procMu.Lock()
	_, adopted := b.processes[key]
	b.procMu.Unlock()
	if !adopted {
		t.Fatal("running job was not adopted on recovery")
	}
	res, err := b.GetProcessStatus(ctx, ws, info.ID, "")
	if err != nil {
		t.Fatalf("GetProcessStatus after recovery: %v", err)
	}
	if res.Status != internal.ProcessRunning {
		t.Fatalf("recovered job status = %q, want running", res.Status)
	}
}

func TestPruneJobsRetention(t *testing.T) {
	b, _ := newJobTestBackendWithStore(t, jobs.Limits{Retention: time.Hour})

	old := time.Now().Add(-2 * time.Hour)
	_ = b.jobStore.Put(jobs.Entry{ID: "proc-old", WorkspaceID: "ws", Status: internal.ProcessSucceeded, StartedAt: old, EndedAt: old})
	_ = b.jobStore.Put(jobs.Entry{ID: "proc-new", WorkspaceID: "ws", Status: internal.ProcessSucceeded, StartedAt: time.Now(), EndedAt: time.Now()})

	if n := b.PruneJobs(time.Now()); n != 1 {
		t.Fatalf("PruneJobs removed %d, want 1", n)
	}
	if _, ok := b.jobStore.Get("proc-old"); ok {
		t.Error("old job should have been pruned")
	}
	if _, ok := b.jobStore.Get("proc-new"); !ok {
		t.Error("recent job should have been kept")
	}
}

func TestStoredJobStatusAfterWorkspaceGone(t *testing.T) {
	b, ws := newJobTestBackendWithStore(t, jobs.Limits{})
	ctx := context.Background()

	logTail := `{"ts":"t","stream":"stdout","line":"hi"}` + "\n"
	if err := b.jobStore.Put(jobs.Entry{
		ID: "proc-r", WorkspaceID: ws.ID, Command: "echo hi",
		Status: internal.ProcessSucceeded, ExitCode: 0, LogTail: logTail,
	}); err != nil {
		t.Fatal(err)
	}

	// Workspace is gone from memory (e.g. destroyed), but the record remains.
	b.mu.Lock()
	delete(b.workspaces, ws.ID)
	b.mu.Unlock()

	res, err := b.GetProcessStatus(ctx, ws, "proc-r", "")
	if err != nil {
		t.Fatalf("GetProcessStatus from store: %v", err)
	}
	if !strings.Contains(res.Output, `"line":"hi"`) || !strings.Contains(res.Output, `"event":"exit"`) {
		t.Fatalf("retained output missing content: %q", res.Output)
	}
	if res.Cursor != "done" {
		t.Fatalf("cursor = %q, want done", res.Cursor)
	}
	if res.Status != internal.ProcessSucceeded {
		t.Fatalf("status = %q, want succeeded", res.Status)
	}
}

// --- Phase 4: Exec wait-then-promote ---

func TestExecReturnsInlineForFastCommand(t *testing.T) {
	b, ws := newJobTestBackendWithStore(t, jobs.Limits{})
	ctx := context.Background()

	res, err := b.Exec(ctx, ws, internal.ExecRequest{Command: "echo hello", WaitSeconds: 5})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.JobID != "" {
		t.Fatalf("fast command should not promote, got job %s", res.JobID)
	}
	if !strings.Contains(res.Stdout, "hello") {
		t.Fatalf("stdout = %q, want it to contain hello", res.Stdout)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0", res.ExitCode)
	}
}

func TestExecPromotesSlowCommand(t *testing.T) {
	b, ws := newJobTestBackendWithStore(t, jobs.Limits{})
	ctx := context.Background()

	res, err := b.Exec(ctx, ws, internal.ExecRequest{Command: "sleep 30", WaitSeconds: 1})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.JobID == "" {
		t.Fatalf("slow command should promote to a job, got %+v", res)
	}
	defer b.KillProcess(ctx, ws, res.JobID)

	// The promoted job must be pollable through the normal job API.
	st, err := b.GetProcessStatus(ctx, ws, res.JobID, "")
	if err != nil {
		t.Fatalf("GetProcessStatus(promoted): %v", err)
	}
	if isTerminalProcessStatus(st.Status) {
		t.Fatalf("promoted job unexpectedly terminal: %s", st.Status)
	}
}

func TestExecAsyncReturnsJobImmediately(t *testing.T) {
	b, ws := newJobTestBackendWithStore(t, jobs.Limits{})
	ctx := context.Background()

	res, err := b.Exec(ctx, ws, internal.ExecRequest{Command: "sleep 30", Async: true})
	if err != nil {
		t.Fatalf("Exec(async): %v", err)
	}
	if res.JobID == "" {
		t.Fatalf("async exec should return a job id, got %+v", res)
	}
	defer b.KillProcess(ctx, ws, res.JobID)
}

func TestExecAsyncRejectedAtCap(t *testing.T) {
	b, ws := newJobTestBackendWithStore(t, jobs.Limits{MaxPerWorkspace: 1})
	ctx := context.Background()

	info, err := b.StartProcess(ctx, ws, internal.StartProcessRequest{Command: "sleep 30"})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	defer b.KillProcess(ctx, ws, info.ID)

	if _, err := b.Exec(ctx, ws, internal.ExecRequest{Command: "echo hi", Async: true}); err == nil {
		t.Fatal("async exec should be rejected when the background cap is full")
	}
}

func TestExecAutoPromoteExemptFromCap(t *testing.T) {
	b, ws := newJobTestBackendWithStore(t, jobs.Limits{MaxPerWorkspace: 1})
	ctx := context.Background()

	// Fill the background cap with an explicit job.
	info, err := b.StartProcess(ctx, ws, internal.StartProcessRequest{Command: "sleep 30"})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	defer b.KillProcess(ctx, ws, info.ID)

	// A foreground exec must still be admitted and promoted despite the cap.
	res, err := b.Exec(ctx, ws, internal.ExecRequest{Command: "sleep 30", WaitSeconds: 1})
	if err != nil {
		t.Fatalf("foreground exec should be exempt from the cap: %v", err)
	}
	if res.JobID == "" {
		t.Fatalf("expected promotion, got %+v", res)
	}
	defer b.KillProcess(ctx, ws, res.JobID)
}

func TestExecAsyncWithSecretsRejected(t *testing.T) {
	b, ws := newJobTestBackendWithStore(t, jobs.Limits{})
	ctx := context.Background()

	_, err := b.Exec(ctx, ws, internal.ExecRequest{
		Command: "echo hi", Async: true, Secrets: []string{"TOKEN"},
	})
	if err == nil {
		t.Fatal("async exec with secrets should be rejected")
	}
}

func TestUnpolledFinishedJobFinalizedSucceeded(t *testing.T) {
	b, ws := newJobTestBackendWithStore(t, jobs.Limits{})
	ctx := context.Background()
	ft := b.workspaces["ws-test"].handle.Transport.(*localTransport)

	info, err := b.StartProcess(ctx, ws, internal.StartProcessRequest{Command: "printf 'ok\\n'"})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}

	// Wait for the job to finish without polling it through the API, so the
	// in-memory status never leaves "queued"/"running".
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(ft.root, info.ID, "exit_code")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job did not finish in time")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Destroy/prune calls finalizeJobsForWorkspace before tearing the runtime
	// down. It must probe first and preserve the real result.
	b.finalizeJobsForWorkspace(ctx, b.workspaces[ws.ID])

	e, ok := b.jobStore.Get(info.ID)
	if !ok {
		t.Fatal("job record missing after finalize")
	}
	if e.Status != internal.ProcessSucceeded {
		t.Fatalf("unpolled finished job recorded as %q, want succeeded", e.Status)
	}
	if e.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", e.ExitCode)
	}
}

func TestExecCancelKillsForegroundJob(t *testing.T) {
	b, ws := newJobTestBackendWithStore(t, jobs.Limits{})
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		_, err := b.Exec(ctx, ws, internal.ExecRequest{
			Command:     "sleep 30",
			WaitSeconds: 60,
		})
		errCh <- err
	}()

	// Wait until the promoted foreground job is registered.
	var id string
	deadline := time.Now().Add(15 * time.Second)
	for {
		b.procMu.Lock()
		for key := range b.processes {
			id = strings.TrimPrefix(key, ws.ID+"/")
		}
		b.procMu.Unlock()
		if id != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job was never registered")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Exec returned success after the context was cancelled")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Exec did not return after cancellation")
	}

	status, _, _ := pollProcess(t, b, ws, id, "")
	if status != internal.ProcessCancelled {
		t.Fatalf("status after client cancel = %q, want cancelled", status)
	}
}

func TestNetworkReferenceCounting(t *testing.T) {
	b, ws := newJobTestBackend(t)
	ft := b.workspaces["ws-test"].handle.Transport.(*localTransport)
	ctx := context.Background()

	info, err := b.StartProcess(ctx, ws, internal.StartProcessRequest{
		Command:      "sleep 30",
		AllowNetwork: true,
	})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	defer b.KillProcess(context.Background(), ws, info.ID)

	for i := 0; i < 100; i++ {
		res, err := b.GetProcessStatus(ctx, ws, info.ID, "")
		if err != nil {
			t.Fatalf("GetProcessStatus: %v", err)
		}
		if res.Status == internal.ProcessRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// A concurrent network-enabled exec finishing must not detach the network
	// that the background job still needs.
	if _, err := b.Exec(ctx, ws, internal.ExecRequest{Command: "printf hi", AllowNetwork: true}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	calls := ft.networkCalls()
	if len(calls) == 0 || !calls[len(calls)-1] {
		t.Fatalf("network was detached while a background job still held it: %v", calls)
	}

	// Once the job is cancelled and the last reference is released, the
	// network is detached exactly once.
	if err := b.KillProcess(ctx, ws, info.ID); err != nil {
		t.Fatalf("KillProcess: %v", err)
	}
	pollProcess(t, b, ws, info.ID, "")
	calls = ft.networkCalls()
	if len(calls) == 0 || calls[len(calls)-1] {
		t.Fatalf("network not detached after the last reference was released: %v", calls)
	}
}
