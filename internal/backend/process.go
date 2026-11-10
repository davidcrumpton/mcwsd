package backend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/jobs"
)

// jobRuntimeDir is where background-job state lives inside every runtime: the
// supervisor script plus one directory per job. /tmp is writable on every
// supported runtime (Linux containers, FreeBSD jails, OpenBSD VMs) and sits
// outside the repo, so job files never appear in git status.
const jobRuntimeDir = "/tmp/.wsd-jobs"

// supervisorPath is the absolute path of the supervisor script inside the
// runtime.
const supervisorPath = jobRuntimeDir + "/supervisor.sh"

// jobStartTimeout is how long a job may sit with no pid file before it is
// treated as failed to start (e.g. the shim could not exec).
const jobStartTimeout = 15 * time.Second

// jobKillGrace is how long KillProcess waits after SIGTERM before sending
// SIGKILL to the job's process group.
const jobKillGrace = 3 * time.Second

// Conventional exit codes reported for jobs that the daemon terminated itself
// rather than one that exited on its own (see signal(7) and timeout(1)).
const (
	exitCodeTimedOut  = 124
	exitCodeCancelled = 137
)

// exitCodeForTerminal returns the exit code to record when the daemon forces a
// job into the given terminal status.
func exitCodeForTerminal(status internal.ProcessStatus) int {
	switch status {
	case internal.ProcessTimedOut:
		return exitCodeTimedOut
	case internal.ProcessCancelled:
		return exitCodeCancelled
	default:
		return 1
	}
}

// maxProcessReadBytes caps how much output a single status read pulls out of
// the runtime, so one poll cannot return an unbounded amount. Polling
// continues from the returned cursor.
const maxProcessReadBytes = 256 * 1024

// jobSupervisorScript runs a background command detached, capturing its
// stdout/stderr to files and recording its pid, exit code and terminal
// status. It is deliberately POSIX sh so it runs unmodified on Linux (busybox
// or GNU userland), FreeBSD and OpenBSD.
//
// Usage: supervisor.sh JOB_DIR COMMAND DETACHED LOG_LIMIT
//
// DETACHED is "1" when the caller started this script in its own session via
// setsid(1), in which case the script's own PID is its process-group ID and is
// recorded in "pgid" so the whole group can be signalled at once. When
// setsid(1) is unavailable the script enables job control so the command leads
// its own process group and records that group in "pgid" too; if the shell
// cannot do that it records no pgid and cancellation falls back to signalling
// the recorded pid.
//
// LOG_LIMIT is the maximum bytes written per stream (0 = unlimited). Output is
// passed through a tiny reader that keeps draining the pipe after the limit so
// a runaway job can neither fill the filesystem nor block on a full pipe.
const jobSupervisorScript = `#!/bin/sh
# mcwsd background job supervisor. Runs COMMAND detached, capturing its stdout
# and stderr, then records the exit code and terminal status. POSIX sh only.
jobdir=$1
cmd=$2
detached=$3
limit=${4:-0}

mkdir -p "$jobdir" 2>/dev/null
: > "$jobdir/stdout"
: > "$jobdir/stderr"
rm -f "$jobdir/exit_code" "$jobdir/status" "$jobdir/pid" "$jobdir/pgid" "$jobdir/shim" "$jobdir/out.fifo" "$jobdir/err.fifo" 2>/dev/null

# The supervisor's own PID is written first so a probe can tell "still
# supervising" from "supervisor died without recording an exit code". Using
# the supervisor (not the child) for liveness closes the race where the child
# is momentarily dead before the exit code is written.
echo "$$" > "$jobdir/shim"

outfifo="$jobdir/out.fifo"
errfifo="$jobdir/err.fifo"
mkfifo "$outfifo" "$errfifo" 2>/dev/null

cap () {
    dest=$1
    n=0
    while IFS= read -r line || [ -n "$line" ]; do
        if [ "$limit" -le 0 ] || [ "$n" -lt "$limit" ]; then
            printf '%s\n' "$line"
            n=$(( n + ${#line} + 1 ))
        fi
    done > "$dest"
}

cap "$jobdir/stdout" < "$outfifo" &
outreader=$!
cap "$jobdir/stderr" < "$errfifo" &
errreader=$!

# On runtimes without setsid(1) (OpenBSD has only the setsid(2) syscall) the
# launcher falls back to nohup, so no process group is set up for us. Enable
# job control so the command is placed in its own process group; without it,
# "sh -c CMD" forks a wrapper (OpenBSD sh does not exec a sole command) and
# cancelling by PID would kill only the wrapper, leaving the real command
# running as an orphan. This only runs on the no-setsid path; when we were
# started with setsid we are already a session/group leader.
if [ "$detached" != "1" ]; then
    set -m 2>/dev/null
fi

sh -c "$cmd" </dev/null > "$outfifo" 2> "$errfifo" &
child=$!
echo "$child" > "$jobdir/pid"
if [ "$detached" = "1" ]; then
    # The supervisor is the session leader, so its own PID is the group ID.
    echo "$$" > "$jobdir/pgid"
else
    # Record the child's group only if job control actually made it a group
    # leader. If it shares our group, record nothing and fall back to a plain
    # pid kill rather than risk signalling an unrelated process group.
    childpgid=$(ps -o pgid= -p "$child" 2>/dev/null | tr -d ' ')
    if [ -n "$childpgid" ] && [ "$childpgid" = "$child" ]; then
        echo "$childpgid" > "$jobdir/pgid"
    fi
fi

wait "$child"
rc=$?
# The stream readers exit only on EOF. A descendant that inherited a pipe and
# outlives the direct child (typical on runtimes without setsid(1), e.g.
# OpenBSD) keeps the write end open, so a bare wait would block forever and
# the job would look like it is still running. Bound the drain with a
# watchdog that kills the readers after a short grace period, so recording
# the terminal state never depends on every descendant closing its fds.
( sleep 2; kill "$outreader" "$errreader" 2>/dev/null ) &
watchdog=$!
wait "$outreader" "$errreader" 2>/dev/null
kill "$watchdog" 2>/dev/null
wait "$watchdog" 2>/dev/null
rm -f "$outfifo" "$errfifo"

echo "$rc" > "$jobdir/exit_code"
if [ "$rc" -eq 0 ]; then
    echo succeeded > "$jobdir/status"
else
    echo failed > "$jobdir/status"
fi
`

// managedProcess holds the in-memory state for a background job. The job's
// actual output lives inside the runtime (see jobRuntimeDir) and is read back
// incrementally via a cursor; nothing here buffers unbounded output.
type managedProcess struct {
	mu             sync.Mutex
	id             string
	workspaceID    string
	jobDir         string
	command        string
	status         internal.ProcessStatus
	exitCode       int
	startedAt      time.Time
	endedAt        time.Time
	allowNet       bool
	timeoutSeconds int
	// foreground marks a job promoted from a synchronous Exec. Foreground
	// jobs are tracked and pollable like any other, but do not count against
	// the background job limits.
	foreground bool
	timer      *time.Timer
	// finalized guards one-time teardown (stopping the timeout timer and
	// detaching network) so it happens exactly once per job.
	finalized bool
	// logTail is the capped final output captured host-side for retention,
	// and tailCaptured guards capturing it exactly once.
	logTail      string
	tailCaptured bool
}

// processKey returns the map key used to store a process for a given workspace.
func processKey(workspaceID, processID string) string {
	return workspaceID + "/" + processID
}

// removeProcessesForWorkspace drops every tracked background process for a
// workspace. Called when a workspace is destroyed or pruned — the runtime is
// going away, so any jobs inside it die with it and there is nothing left to
// poll. Timeout timers are stopped so they cannot fire for a dead workspace.
func (b *Backend) removeProcessesForWorkspace(workspaceID string) {
	prefix := workspaceID + "/"
	b.procMu.Lock()
	defer b.procMu.Unlock()
	for key, proc := range b.processes {
		if strings.HasPrefix(key, prefix) {
			proc.mu.Lock()
			if proc.timer != nil {
				proc.timer.Stop()
			}
			proc.mu.Unlock()
			delete(b.processes, key)
		}
	}
}

// isTerminalProcessStatus reports whether a status is final.
func isTerminalProcessStatus(s internal.ProcessStatus) bool {
	switch s {
	case internal.ProcessSucceeded, internal.ProcessFailed,
		internal.ProcessCancelled, internal.ProcessTimedOut:
		return true
	}
	return false
}

// buildJobLaunchCommand returns the shell snippet that starts the supervisor
// script, detached, and returns immediately. All standard descriptors are
// redirected so the surrounding exec (docker exec / jexec / ssh) sees EOF and
// returns at once instead of waiting for the job. logLimit is the per-stream
// byte cap handed to the supervisor (0 = unlimited).
func buildJobLaunchCommand(jobDir, command string, logLimit int64) string {
	base := fmt.Sprintf("sh %s %s %s", shellQuote(supervisorPath), shellQuote(jobDir), shellQuote(command))
	return fmt.Sprintf(`mkdir -p %s
if command -v setsid >/dev/null 2>&1; then
  setsid %s 1 %d </dev/null >/dev/null 2>&1 &
else
  nohup %s 0 %d </dev/null >/dev/null 2>&1 &
fi
exit 0`, shellQuote(jobRuntimeDir), base, logLimit, base, logLimit)
}

// buildKillCommand returns the shell snippet that terminates a job's process
// group: SIGTERM, a bounded grace period, then SIGKILL. It is best-effort and
// always exits 0.
func buildKillCommand(jobDir string) string {
	return fmt.Sprintf(`d=%s
pid=$(cat "$d/pid" 2>/dev/null)
pgid=$(cat "$d/pgid" 2>/dev/null)
if [ -n "$pgid" ]; then kill -TERM -"$pgid" 2>/dev/null; fi
if [ -n "$pid" ]; then kill -TERM "$pid" 2>/dev/null; fi
i=0
while [ $i -lt %d ]; do
  if [ -z "$pid" ] || ! kill -0 "$pid" 2>/dev/null; then break; fi
  sleep 1
  i=$((i+1))
done
if [ -n "$pgid" ]; then kill -KILL -"$pgid" 2>/dev/null; fi
if [ -n "$pid" ]; then kill -KILL "$pid" 2>/dev/null; fi
exit 0`, shellQuote(jobDir), int(jobKillGrace/time.Second))
}

// processCursor is the decoded form of the opaque cursor handed back to
// callers. It tracks how many bytes of each raw stream have already been
// consumed, plus a terminal marker.
type processCursor struct {
	outOffset int64
	errOffset int64
	done      bool
}

// parseProcessCursor decodes a cursor. Anything malformed is treated as the
// beginning so a bad client value degrades to "replay everything" rather than
// failing.
func parseProcessCursor(s string) processCursor {
	s = strings.TrimSpace(s)
	if s == "" {
		return processCursor{}
	}
	if s == "done" {
		return processCursor{done: true}
	}
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return processCursor{}
	}
	out, err1 := strconv.ParseInt(parts[0], 10, 64)
	errBytes, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || out < 0 || errBytes < 0 {
		return processCursor{}
	}
	return processCursor{outOffset: out, errOffset: errBytes}
}

// String encodes the cursor for the wire. "done" is a sentinel meaning the
// terminal record has already been delivered.
func (c processCursor) String() string {
	if c.done {
		return "done"
	}
	return fmt.Sprintf("%d:%d", c.outOffset, c.errOffset)
}

// processLogRecord is one line of a job's combined JSON log.
type processLogRecord struct {
	TS       string `json:"ts"`
	Stream   string `json:"stream,omitempty"`
	Line     string `json:"line,omitempty"`
	Event    string `json:"event,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

// streamToLog converts a chunk of raw stdout/stderr into JSON-lines records,
// stopping at the last complete line. It returns the encoded records and the
// number of bytes consumed, so a trailing partial line is re-read on the next
// poll instead of being lost or split.
func streamToLog(stream string, chunk []byte) (string, int64) {
	idx := bytes.LastIndexByte(chunk, '\n')
	if idx < 0 {
		return "", 0
	}
	complete := chunk[:idx]
	var b strings.Builder
	for _, ln := range bytes.Split(complete, []byte{'\n'}) {
		ln = bytes.TrimSuffix(ln, []byte{'\r'})
		rec := processLogRecord{
			TS:     time.Now().UTC().Format(time.RFC3339Nano),
			Stream: stream,
			Line:   string(ln),
		}
		if j, err := json.Marshal(rec); err == nil {
			b.Write(j)
			b.WriteByte('\n')
		}
	}
	return b.String(), int64(idx + 1)
}

// exitLogRecord renders the terminal record appended to a job's JSON log.
func exitLogRecord(code int) string {
	c := code
	rec := processLogRecord{
		TS:       time.Now().UTC().Format(time.RFC3339Nano),
		Event:    "exit",
		ExitCode: &c,
	}
	j, err := json.Marshal(rec)
	if err != nil {
		return ""
	}
	return string(j) + "\n"
}

// processState is the coarse state a runtime probe reports.
type processState int

const (
	processStateStarting processState = iota
	processStateRunning
	processStateDone
	processStateGone
)

// probeProcess inspects a job's state files inside the runtime. It is the
// source of truth for completion: the presence of "exit_code" means the job is
// done; otherwise a live pid means running, a dead pid means gone, and the
// absence of a pid file means the job has not started yet.
func (b *Backend) probeProcess(ctx context.Context, wsState *workspaceState, jobDir string) (processState, int, error) {
	script := fmt.Sprintf(
		`d=%s
if [ -f "$d/exit_code" ]; then
  printf 'done %%s\n' "$(cat "$d/exit_code" 2>/dev/null)"
elif [ -f "$d/shim" ]; then
  if kill -0 "$(cat "$d/shim" 2>/dev/null)" 2>/dev/null; then printf 'running\n'; else printf 'gone\n'; fi
else
  printf 'starting\n'
fi`, shellQuote(jobDir))

	res, err := wsState.handle.Transport.Exec(ctx, []string{"sh", "-c", script}, wsState.handle.WorkDir, nil)
	if err != nil {
		return processStateStarting, 0, err
	}
	fields := strings.Fields(strings.TrimSpace(res.Stdout))
	if len(fields) == 0 {
		return processStateStarting, 0, nil
	}
	switch fields[0] {
	case "done":
		code := 0
		if len(fields) > 1 {
			code, _ = strconv.Atoi(fields[1])
		}
		return processStateDone, code, nil
	case "running":
		return processStateRunning, 0, nil
	case "gone":
		return processStateGone, 0, nil
	default:
		return processStateStarting, 0, nil
	}
}

// reconcileProcess probes the runtime and folds the result into proc. It
// returns true once the job is terminal.
func (b *Backend) reconcileProcess(ctx context.Context, wsState *workspaceState, proc *managedProcess) (bool, error) {
	proc.mu.Lock()
	if isTerminalProcessStatus(proc.status) {
		proc.mu.Unlock()
		return true, nil
	}
	startedAt := proc.startedAt
	jobDir := proc.jobDir
	proc.mu.Unlock()

	state, code, err := b.probeProcess(ctx, wsState, jobDir)
	if err != nil {
		return false, err
	}

	proc.mu.Lock()
	// A cancel may have won the race between the probe and this lock.
	if isTerminalProcessStatus(proc.status) {
		proc.mu.Unlock()
		return true, nil
	}
	terminal := false
	switch state {
	case processStateDone:
		switch proc.status {
		case internal.ProcessCancelled, internal.ProcessTimedOut:
			// Keep the explicit terminal reason and the conventional exit
			// code set by cancelProcess rather than overwriting them with the
			// shell's signal-derived rc.
		default:
			proc.exitCode = code
			if code == 0 {
				proc.status = internal.ProcessSucceeded
			} else {
				proc.status = internal.ProcessFailed
			}
		}
		terminal = true
	case processStateGone:
		switch proc.status {
		case internal.ProcessCancelled, internal.ProcessTimedOut:
		default:
			proc.status = internal.ProcessFailed
		}
		terminal = true
	case processStateStarting:
		if time.Since(startedAt) > jobStartTimeout {
			proc.status = internal.ProcessFailed
			terminal = true
		} else {
			proc.status = internal.ProcessQueued
		}
	case processStateRunning:
		proc.status = internal.ProcessRunning
	}

	needDetach := false
	if terminal {
		proc.endedAt = time.Now()
		needDetach = proc.markFinalizedLocked()
	}
	proc.mu.Unlock()

	if terminal {
		b.finalizeJobRecord(ctx, wsState, proc)
		if needDetach {
			detachProcessNetwork(ctx, wsState, proc)
		}
	}
	return terminal, nil
}

// finalizeJobRecord captures the job's capped output tail and persists its
// terminal state to the host-side store, so the result survives the runtime
// being destroyed.
func (b *Backend) finalizeJobRecord(ctx context.Context, wsState *workspaceState, proc *managedProcess) {
	if b.jobStore == nil {
		return
	}
	b.captureJobTail(ctx, wsState, proc)
	b.saveJob(proc)
}

// captureJobTail reads the last MaxLogBytes of each raw stream and stores it
// host-side as combined JSON lines. It runs at most once per job.
func (b *Backend) captureJobTail(ctx context.Context, wsState *workspaceState, proc *managedProcess) {
	proc.mu.Lock()
	if proc.tailCaptured {
		proc.mu.Unlock()
		return
	}
	proc.tailCaptured = true
	jobDir := proc.jobDir
	proc.mu.Unlock()

	limit := b.jobLimits.MaxLogBytes
	if limit <= 0 {
		limit = maxProcessReadBytes
	}
	// Tail capture is cleanup: it must still run if the triggering request was
	// cancelled (client disconnect, timeout, workspace teardown).
	tailCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	outTail := b.readStreamTail(tailCtx, wsState, jobDir, "stdout", limit)
	errTail := b.readStreamTail(tailCtx, wsState, jobDir, "stderr", limit)
	tail := tailToLog("stdout", outTail) + tailToLog("stderr", errTail)

	proc.mu.Lock()
	proc.logTail = tail
	proc.mu.Unlock()
}

// readStreamTail returns up to limit bytes from the end of a raw job stream.
func (b *Backend) readStreamTail(ctx context.Context, wsState *workspaceState, jobDir, name string, limit int64) []byte {
	if limit <= 0 {
		limit = maxProcessReadBytes
	}
	path := jobDir + "/" + name
	// A positive -c count selects the last N bytes on both GNU and BSD tail.
	script := fmt.Sprintf("tail -c %d %s 2>/dev/null", limit, shellQuote(path))
	res, err := wsState.handle.Transport.Exec(ctx, []string{"sh", "-c", script}, wsState.handle.WorkDir, nil)
	if err != nil || res == nil {
		return nil
	}
	return []byte(res.Stdout)
}

// tailToLog drops a leading partial line from a byte tail (which may have
// started mid-line) and encodes the rest as JSON records.
func tailToLog(stream string, chunk []byte) string {
	i := bytes.IndexByte(chunk, '\n')
	if i < 0 {
		return ""
	}
	out, _ := streamToLog(stream, chunk[i+1:])
	return out
}

// saveJob writes a job's current state to the host-side store.
func (b *Backend) saveJob(proc *managedProcess) {
	if b.jobStore == nil {
		return
	}
	proc.mu.Lock()
	e := jobs.Entry{
		ID:             proc.id,
		WorkspaceID:    proc.workspaceID,
		Command:        proc.command,
		SessionID:      b.sessionID,
		Status:         proc.status,
		ExitCode:       proc.exitCode,
		AllowNetwork:   proc.allowNet,
		Foreground:     proc.foreground,
		StartedAt:      proc.startedAt,
		EndedAt:        proc.endedAt,
		TimeoutSeconds: proc.timeoutSeconds,
		LogTail:        proc.logTail,
	}
	proc.mu.Unlock()
	if err := b.jobStore.Put(e); err != nil {
		logger.Warn("save_job_failed", "job_id", e.ID, "error", err)
	}
}

// markFinalizedLocked performs one-time teardown for a terminal job and
// reports whether the caller should detach the workspace network. Caller must
// hold proc.mu.
func (p *managedProcess) markFinalizedLocked() bool {
	if p.finalized {
		return false
	}
	p.finalized = true
	if p.timer != nil {
		p.timer.Stop()
	}
	return p.allowNet
}

// detachProcessNetwork returns a network-enabled workspace to its network-off
// resting state after a job finishes.
func detachProcessNetwork(ctx context.Context, wsState *workspaceState, proc *managedProcess) {
	proc.mu.Lock()
	allow := proc.allowNet
	proc.mu.Unlock()
	if !allow {
		return
	}
	wsState.releaseNetwork(ctx, wsState.handle.Transport, wsState.workspace.ID)
}

// StartProcess launches a command inside the workspace as a tracked background
// job and returns immediately. Output is captured inside the runtime and read
// back incrementally via GetProcessStatus.
func (b *Backend) StartProcess(ctx context.Context, ws *internal.Workspace, req internal.StartProcessRequest) (*internal.ProcessInfo, error) {
	return b.startJob(ctx, ws, req, false)
}

// startJob is the shared implementation behind StartProcess and Exec's
// wait-then-promote path. foreground jobs (promoted from a synchronous Exec)
// are tracked but exempt from the background job limits, so a full background
// queue can never strand a running foreground command.
func (b *Backend) startJob(ctx context.Context, ws *internal.Workspace, req internal.StartProcessRequest, foreground bool) (*internal.ProcessInfo, error) {
	b.mu.RLock()
	wsState, ok := b.workspaces[ws.ID]
	b.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("workspace %s not found", ws.ID)
	}

	b.touch(ws.ID)

	// Enforce daemon job limits before starting any work, so a rejected job
	// costs nothing. Foreground execs are exempt.
	if !foreground {
		if err := b.checkJobLimits(ws.ID); err != nil {
			return nil, err
		}
	}

	pid := fmt.Sprintf("proc-%x", sha256.Sum256([]byte(fmt.Sprintf("%s-%d", ws.ID, time.Now().UnixNano()))))[:12]
	jobDir := jobRuntimeDir + "/" + pid
	transport := wsState.handle.Transport

	// Create the job directory and (re)install the supervisor script. The
	// script is written every launch so a changed script takes effect without
	// re-provisioning.
	if _, err := transport.Exec(ctx, []string{"sh", "-c", "mkdir -p " + shellQuote(jobDir)}, wsState.handle.WorkDir, nil); err != nil {
		return nil, fmt.Errorf("failed to create job directory: %w", err)
	}
	if err := transport.WriteFile(ctx, supervisorPath, []byte(jobSupervisorScript), 0755); err != nil {
		return nil, fmt.Errorf("failed to install job supervisor: %w", err)
	}

	// A network-enabled job needs the workspace attached for its whole
	// lifetime, not just for the launch call; it is detached when the job
	// reaches a terminal state. The reference is held until then so a
	// concurrent Exec can't detach it early.
	if req.AllowNetwork {
		if err := wsState.acquireNetwork(ctx, transport, ws.ID); err != nil {
			return nil, fmt.Errorf("failed to attach network for job: %w", err)
		}
	}

	proc := &managedProcess{
		id:             pid,
		workspaceID:    ws.ID,
		jobDir:         jobDir,
		command:        req.Command,
		status:         internal.ProcessQueued,
		startedAt:      time.Now(),
		allowNet:       req.AllowNetwork,
		timeoutSeconds: b.effectiveTimeoutSeconds(req.TimeoutSeconds),
		foreground:     foreground,
	}

	logger.Info("process_start", "workspace_id", ws.ID, "process_id", pid,
		"allow_network", req.AllowNetwork, "timeout_seconds", proc.timeoutSeconds,
		"foreground", foreground, "command", fmt.Sprintf("%q", req.Command))

	launch := buildJobLaunchCommand(jobDir, req.Command, b.jobLimits.MaxLogBytes)
	if _, err := transport.Exec(ctx, []string{"sh", "-c", launch}, wsState.handle.WorkDir, nil); err != nil {
		logger.Warn("process_start_failed", "workspace_id", ws.ID, "process_id", pid, "error", err)
		if req.AllowNetwork {
			wsState.releaseNetwork(ctx, transport, ws.ID)
		}
		return nil, fmt.Errorf("failed to launch job: %w", err)
	}

	key := processKey(ws.ID, pid)
	b.procMu.Lock()
	b.processes[key] = proc
	b.procMu.Unlock()

	logger.Info("process_started", "workspace_id", ws.ID, "process_id", pid)

	// Persist the job so it can be reconciled after a daemon restart.
	b.saveJob(proc)

	if proc.timeoutSeconds > 0 {
		d := time.Duration(proc.timeoutSeconds) * time.Second
		proc.timer = time.AfterFunc(d, func() {
			b.cancelProcess(wsState, proc, internal.ProcessTimedOut)
		})
	}

	// Watch network-enabled jobs so the workspace's network is detached as
	// soon as the job finishes, without waiting for a status poll.
	if req.AllowNetwork {
		go b.watchProcess(wsState, key, proc)
	}

	return &internal.ProcessInfo{
		ID:      pid,
		Command: req.Command,
		Status:  internal.ProcessQueued,
	}, nil
}

// checkJobLimits refuses a new job when the daemon or the workspace is at its
// configured concurrency limit. Only active (queued or running) jobs count.
func (b *Backend) checkJobLimits(workspaceID string) error {
	if b.jobLimits.MaxPerWorkspace <= 0 && b.jobLimits.MaxTotal <= 0 {
		return nil
	}
	total, perWorkspace := b.activeJobCounts()
	if b.jobLimits.MaxPerWorkspace > 0 && perWorkspace[workspaceID] >= b.jobLimits.MaxPerWorkspace {
		return fmt.Errorf("workspace %s already has %d active job(s); limit is %d",
			workspaceID, perWorkspace[workspaceID], b.jobLimits.MaxPerWorkspace)
	}
	if b.jobLimits.MaxTotal > 0 && total >= b.jobLimits.MaxTotal {
		return fmt.Errorf("daemon already has %d active job(s); limit is %d", total, b.jobLimits.MaxTotal)
	}
	return nil
}

// activeJobCounts returns the number of active jobs daemon-wide and per
// workspace.
func (b *Backend) activeJobCounts() (total int, perWorkspace map[string]int) {
	perWorkspace = map[string]int{}
	type snap struct {
		wsID       string
		status     internal.ProcessStatus
		foreground bool
	}
	b.procMu.Lock()
	snaps := make([]snap, 0, len(b.processes))
	for _, proc := range b.processes {
		proc.mu.Lock()
		snaps = append(snaps, snap{wsID: proc.workspaceID, status: proc.status, foreground: proc.foreground})
		proc.mu.Unlock()
	}
	b.procMu.Unlock()
	for _, s := range snaps {
		if isTerminalProcessStatus(s.status) || s.foreground {
			continue
		}
		total++
		perWorkspace[s.wsID]++
	}
	return total, perWorkspace
}

// effectiveTimeoutSeconds combines a per-request timeout with the daemon's
// MaxRuntime cap, returning the smaller of the two (0 means no timeout).
func (b *Backend) effectiveTimeoutSeconds(requested int) int {
	if b.jobLimits.MaxRuntime <= 0 {
		return requested
	}
	maxSeconds := int(b.jobLimits.MaxRuntime / time.Second)
	if maxSeconds <= 0 {
		return requested
	}
	if requested <= 0 || maxSeconds < requested {
		return maxSeconds
	}
	return requested
}

// execPromotePollInterval is how often a wait-then-promote Exec checks whether
// its command has finished before the wait elapses.
const execPromotePollInterval = 250 * time.Millisecond

// execAsJob runs a synchronous Exec request through the background-job
// machinery. With req.Async it returns a job_id at once (enforcing the
// background limits). Otherwise it waits up to waitSeconds: if the job
// finishes, it returns the inline output; if not, it returns the job_id so the
// caller can poll. Promoted foreground jobs are exempt from the limits.
func (b *Backend) execAsJob(ctx context.Context, wsState *workspaceState, ws *internal.Workspace, req internal.ExecRequest, waitSeconds int) (*internal.ExecResult, error) {
	info, err := b.startJob(ctx, ws, internal.StartProcessRequest{
		Command:        req.Command,
		AllowNetwork:   req.AllowNetwork,
		TimeoutSeconds: req.TimeoutSeconds,
	}, !req.Async)
	if err != nil {
		return nil, err
	}

	if req.Async {
		logger.Info("exec_async", "workspace_id", ws.ID, "job_id", info.ID)
		return &internal.ExecResult{JobID: info.ID, Status: string(info.Status)}, nil
	}

	key := processKey(ws.ID, info.ID)
	b.procMu.Lock()
	proc := b.processes[key]
	b.procMu.Unlock()
	if proc == nil {
		return nil, fmt.Errorf("job %s vanished after launch", info.ID)
	}

	start := time.Now()
	deadline := start.Add(time.Duration(waitSeconds) * time.Second)
	for {
		terminal, err := b.reconcileProcess(ctx, wsState, proc)
		if err != nil {
			return nil, err
		}

		if terminal {
			proc.mu.Lock()
			exitCode := proc.exitCode
			jobDir := proc.jobDir
			proc.mu.Unlock()

			result := &internal.ExecResult{
				Stdout:     string(b.readProcessStream(ctx, wsState, jobDir, "stdout", 0)),
				Stderr:     string(b.readProcessStream(ctx, wsState, jobDir, "stderr", 0)),
				ExitCode:   exitCode,
				DurationMs: time.Since(start).Milliseconds(),
			}
			result.Stdout = b.scrubSecrets(result.Stdout, req.Secrets)
			result.Stderr = b.scrubSecrets(result.Stderr, req.Secrets)
			logger.Info("exec_complete", "workspace_id", ws.ID, "job_id", info.ID,
				"exit_code", exitCode, "promoted", false, "duration_ms", result.DurationMs)
			return result, nil
		}

		if !time.Now().Before(deadline) {
			proc.mu.Lock()
			status := proc.status
			proc.mu.Unlock()
			logger.Info("exec_promoted", "workspace_id", ws.ID, "job_id", info.ID,
				"status", string(status), "wait_seconds", waitSeconds)
			return &internal.ExecResult{JobID: info.ID, Status: string(status)}, nil
		}

		select {
		case <-ctx.Done():
			// The caller went away before the foreground job finished. Kill
			// it rather than leaving an orphan that is exempt from the job
			// limits and may run forever.
			b.cancelProcess(wsState, proc, internal.ProcessCancelled)
			return nil, ctx.Err()
		case <-time.After(execPromotePollInterval):
		}
	}
}

// watchProcess polls a network-enabled job until it reaches a terminal state
// or its record is removed (e.g. the workspace was destroyed).
func (b *Backend) watchProcess(wsState *workspaceState, key string, proc *managedProcess) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range ticker.C {
		b.procMu.Lock()
		_, exists := b.processes[key]
		b.procMu.Unlock()
		if !exists {
			return
		}
		done, err := b.reconcileProcess(context.Background(), wsState, proc)
		if err != nil {
			logger.Warn("process_watch_failed", "process_id", proc.id, "error", err)
			continue
		}
		if done {
			return
		}
	}
}

// ListProcesses returns all tracked background processes for a workspace,
// refreshing each from the runtime first so the reported status is current.
func (b *Backend) ListProcesses(ctx context.Context, ws *internal.Workspace) ([]internal.ProcessInfo, error) {
	b.mu.RLock()
	wsState, ok := b.workspaces[ws.ID]
	b.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("workspace %s not found", ws.ID)
	}

	b.touch(ws.ID)

	prefix := ws.ID + "/"
	b.procMu.Lock()
	procs := make([]*managedProcess, 0, len(b.processes))
	for key, proc := range b.processes {
		if strings.HasPrefix(key, prefix) {
			procs = append(procs, proc)
		}
	}
	b.procMu.Unlock()

	infos := make([]internal.ProcessInfo, 0, len(procs))
	for _, proc := range procs {
		if _, err := b.reconcileProcess(ctx, wsState, proc); err != nil {
			logger.Warn("process_reconcile_failed", "workspace_id", ws.ID, "process_id", proc.id, "error", err)
		}
		proc.mu.Lock()
		infos = append(infos, internal.ProcessInfo{
			ID:       proc.id,
			Command:  proc.command,
			Status:   proc.status,
			ExitCode: proc.exitCode,
		})
		proc.mu.Unlock()
	}

	// Provision jobs for this workspace share the job API even though they
	// are daemon-side orchestration rather than in-runtime processes.
	b.provMu.Lock()
	provJobs := make([]*provisionJob, 0)
	for _, job := range b.provisionJobs {
		if job.workspaceID == ws.ID {
			provJobs = append(provJobs, job)
		}
	}
	b.provMu.Unlock()
	for _, job := range provJobs {
		job.mu.Lock()
		infos = append(infos, internal.ProcessInfo{
			ID:       job.id,
			Command:  job.command(),
			Status:   job.status,
			ExitCode: job.exitCodeLocked(),
		})
		job.mu.Unlock()
	}
	return infos, nil
}

// GetProcessStatus returns the job's status and the output produced since
// cursor. Reading is derived entirely from the runtime's state files, so no
// output is buffered in the daemon.
func (b *Backend) GetProcessStatus(ctx context.Context, ws *internal.Workspace, processID, cursor string) (*internal.ProcessStatusResult, error) {
	// Provision jobs are daemon-side orchestration, not in-runtime shell
	// jobs, and they exist before their workspace is registered. Serve them
	// first so a not-yet-provisioned workspace can still be polled.
	if job, ok := b.lookupProvisionJob(processID); ok && job.workspaceID == ws.ID {
		return b.provisionJobStatus(job, cursor), nil
	}

	b.mu.RLock()
	wsState, ok := b.workspaces[ws.ID]
	b.mu.RUnlock()
	if !ok {
		// The workspace may have been destroyed while a retained job record
		// still exists; fall back to the host-side store.
		return b.storedJobStatus(ws.ID, processID, cursor)
	}

	b.touch(ws.ID)

	key := processKey(ws.ID, processID)
	b.procMu.Lock()
	proc, exists := b.processes[key]
	b.procMu.Unlock()
	if !exists {
		return b.storedJobStatus(ws.ID, processID, cursor)
	}

	if _, err := b.reconcileProcess(ctx, wsState, proc); err != nil {
		logger.Warn("process_reconcile_failed", "workspace_id", ws.ID, "process_id", processID, "error", err)
	}

	cur := parseProcessCursor(cursor)

	var stdout, stderr, output string
	if !cur.done {
		stdoutData := b.readProcessStream(ctx, wsState, proc.jobDir, "stdout", cur.outOffset)
		stderrData := b.readProcessStream(ctx, wsState, proc.jobDir, "stderr", cur.errOffset)
		stdout = string(stdoutData)
		stderr = string(stderrData)

		if len(stdoutData) > 0 {
			records, consumed := streamToLog("stdout", stdoutData)
			output += records
			cur.outOffset += consumed
		}
		if len(stderrData) > 0 {
			records, consumed := streamToLog("stderr", stderrData)
			output += records
			cur.errOffset += consumed
		}
	}

	proc.mu.Lock()
	status := proc.status
	exitCode := proc.exitCode
	proc.mu.Unlock()

	if !cur.done && isTerminalProcessStatus(status) {
		output += exitLogRecord(exitCode)
		cur = processCursor{done: true}
	}

	return &internal.ProcessStatusResult{
		ProcessInfo: internal.ProcessInfo{
			ID:       proc.id,
			Command:  proc.command,
			Status:   status,
			ExitCode: exitCode,
		},
		Stdout: stdout,
		Stderr: stderr,
		Output: output,
		Cursor: cur.String(),
	}, nil
}

// readProcessStream reads up to maxProcessReadBytes of a job's raw stdout or
// stderr starting at the given byte offset. A missing file or read failure
// yields no data; the job is still probed separately for status.
func (b *Backend) readProcessStream(ctx context.Context, wsState *workspaceState, jobDir, name string, offset int64) []byte {
	if offset < 0 {
		offset = 0
	}
	path := jobDir + "/" + name
	// tail -c +N is 1-based, so +1 starts at the beginning. Cap the read with
	// dd (POSIX bs/count) rather than head -c: OpenBSD's head(1) has no -c.
	script := fmt.Sprintf("tail -c +%d %s 2>/dev/null | dd bs=%d count=1 2>/dev/null",
		offset+1, shellQuote(path), maxProcessReadBytes)
	res, err := wsState.handle.Transport.Exec(ctx, []string{"sh", "-c", script}, wsState.handle.WorkDir, nil)
	if err != nil || res == nil {
		return nil
	}
	return []byte(res.Stdout)
}

// KillProcess terminates a job's process group and marks it cancelled. The
// record and its output stay queryable. It is idempotent.
func (b *Backend) KillProcess(ctx context.Context, ws *internal.Workspace, processID string) error {
	b.mu.RLock()
	wsState, ok := b.workspaces[ws.ID]
	b.mu.RUnlock()
	if !ok {
		return fmt.Errorf("workspace %s not found", ws.ID)
	}

	b.touch(ws.ID)

	key := processKey(ws.ID, processID)
	b.procMu.Lock()
	proc, exists := b.processes[key]
	b.procMu.Unlock()
	if !exists {
		return fmt.Errorf("process %s not found in workspace %s", processID, ws.ID)
	}

	b.cancelProcess(wsState, proc, internal.ProcessCancelled)
	return nil
}

// cancelProcess signals a job's process group and marks it with the given
// terminal status. It is safe to call more than once; the first caller wins.
func (b *Backend) cancelProcess(wsState *workspaceState, proc *managedProcess, terminal internal.ProcessStatus) {
	// Probe before overriding: a job may have already finished (successfully)
	// between the last status poll and this cancel/timeout. Recording it as
	// cancelled or timed out would destroy a real result.
	probeCtx, probeCancel := context.WithTimeout(context.Background(), jobKillGrace+30*time.Second)
	done, probeErr := b.reconcileProcess(probeCtx, wsState, proc)
	probeCancel()
	if probeErr != nil {
		logger.Warn("process_cancel_probe_failed", "process_id", proc.id, "error", probeErr)
	}
	if done {
		return
	}

	proc.mu.Lock()
	// A probe or another canceller may have won the race while we probed.
	if isTerminalProcessStatus(proc.status) {
		proc.mu.Unlock()
		return
	}
	proc.status = terminal
	proc.exitCode = exitCodeForTerminal(terminal)
	proc.endedAt = time.Now()
	jobDir := proc.jobDir
	needDetach := proc.markFinalizedLocked()
	proc.mu.Unlock()

	// Signal the group using a context detached from the caller's so
	// cancellation still completes if the triggering request goes away.
	killCtx, cancel := context.WithTimeout(context.Background(), jobKillGrace+30*time.Second)
	defer cancel()
	if _, err := wsState.handle.Transport.Exec(killCtx, []string{"sh", "-c", buildKillCommand(jobDir)}, wsState.handle.WorkDir, nil); err != nil {
		logger.Warn("process_kill_failed", "process_id", proc.id, "error", err)
	}

	// Capture whatever output the job produced and persist the terminal state
	// before (optionally) returning the workspace to network-off.
	b.finalizeJobRecord(killCtx, wsState, proc)
	if needDetach {
		detachProcessNetwork(killCtx, wsState, proc)
	}
}

// storedJobStatus serves a job from the host-side store. It is the fallback
// used when the workspace (or its in-memory process) is gone but the job
// record is still retained. The capped log tail is delivered once, then the
// cursor reports "done".
func (b *Backend) storedJobStatus(workspaceID, processID, cursor string) (*internal.ProcessStatusResult, error) {
	if b.jobStore == nil {
		return nil, fmt.Errorf("workspace %s not found", workspaceID)
	}
	e, ok := b.jobStore.Get(processID)
	if !ok || e.WorkspaceID != workspaceID {
		return nil, fmt.Errorf("process %s not found in workspace %s", processID, workspaceID)
	}

	cur := parseProcessCursor(cursor)
	output := ""
	if !cur.done {
		output = e.LogTail
		if output != "" && !strings.HasSuffix(output, "\n") {
			output += "\n"
		}
		if isTerminalProcessStatus(e.Status) {
			output += exitLogRecord(e.ExitCode)
		}
		cur = processCursor{done: true}
	}

	return &internal.ProcessStatusResult{
		ProcessInfo: internal.ProcessInfo{
			ID:       e.ID,
			Command:  e.Command,
			Status:   e.Status,
			ExitCode: e.ExitCode,
		},
		Output: output,
		Cursor: cur.String(),
	}, nil
}

// RecoverJobs reconciles persisted jobs after a daemon restart. Jobs whose
// workspace was recovered are adopted into memory and probed against the
// runtime's state files (the source of truth); jobs whose workspace is gone
// are marked interrupted. It never trusts the persisted status alone.
func (b *Backend) RecoverJobs(ctx context.Context) {
	if b.jobStore == nil {
		return
	}
	recovered := 0
	for _, e := range b.jobStore.All() {
		// A provision job is daemon-side orchestration and cannot survive a
		// restart (its goroutine is gone), so any non-terminal one is
		// interrupted rather than probed or adopted.
		if e.Kind == jobs.KindProvision {
			if !isTerminalProcessStatus(e.Status) {
				e.Status = internal.ProcessFailed
				if e.EndedAt.IsZero() {
					e.EndedAt = time.Now()
				}
				if err := b.jobStore.Put(e); err != nil {
					logger.Warn("recover_job_failed", "job_id", e.ID, "error", err)
				}
			}
			continue
		}

		key := processKey(e.WorkspaceID, e.ID)
		b.procMu.Lock()
		_, inMem := b.processes[key]
		b.procMu.Unlock()
		if inMem {
			continue
		}

		b.mu.RLock()
		wsState, ok := b.workspaces[e.WorkspaceID]
		b.mu.RUnlock()

		if !ok {
			// The workspace could not be recovered, so its job cannot be
			// running on this daemon. Mark it interrupted.
			if !isTerminalProcessStatus(e.Status) {
				e.Status = internal.ProcessFailed
				if e.EndedAt.IsZero() {
					e.EndedAt = time.Now()
				}
				if err := b.jobStore.Put(e); err != nil {
					logger.Warn("recover_job_failed", "job_id", e.ID, "error", err)
				}
			}
			continue
		}

		proc := &managedProcess{
			id:             e.ID,
			workspaceID:    e.WorkspaceID,
			jobDir:         jobRuntimeDir + "/" + e.ID,
			command:        e.Command,
			status:         e.Status,
			exitCode:       e.ExitCode,
			startedAt:      e.StartedAt,
			allowNet:       e.AllowNetwork,
			timeoutSeconds: e.TimeoutSeconds,
			foreground:     e.Foreground,
			logTail:        e.LogTail,
		}
		b.procMu.Lock()
		b.processes[key] = proc
		b.procMu.Unlock()

		if _, err := b.reconcileProcess(ctx, wsState, proc); err != nil {
			logger.Warn("recover_job_reconcile_failed", "job_id", e.ID, "error", err)
			continue
		}

		// Re-arm the timeout for a job that is genuinely still running so a
		// recovered job stays bounded.
		proc.mu.Lock()
		rearm := proc.timeoutSeconds > 0 && !isTerminalProcessStatus(proc.status) && proc.timer == nil
		proc.mu.Unlock()
		if rearm {
			d := time.Duration(proc.timeoutSeconds) * time.Second
			proc.timer = time.AfterFunc(d, func() {
				b.cancelProcess(wsState, proc, internal.ProcessTimedOut)
			})
		}
		recovered++
	}
	if recovered > 0 {
		logger.Info("jobs_recovered", "count", recovered)
	}
}

// PruneJobs removes persisted job records (and their retained log tails) older
// than the configured retention. It returns the number removed.
func (b *Backend) PruneJobs(now time.Time) int {
	b.pruneProvisionJobs(now)
	if b.jobStore == nil || b.jobLimits.Retention <= 0 {
		return 0
	}
	cutoff := now.Add(-b.jobLimits.Retention)
	removed := 0
	for _, e := range b.jobStore.FinishedBefore(cutoff) {
		if err := b.jobStore.Remove(e.ID); err != nil {
			logger.Warn("prune_job_failed", "job_id", e.ID, "error", err)
			continue
		}
		removed++
	}
	return removed
}

// pruneProvisionJobs drops finished in-memory provision jobs older than the
// retention window, so a long-running daemon does not accumulate them forever.
func (b *Backend) pruneProvisionJobs(now time.Time) {
	if b.jobLimits.Retention <= 0 {
		return
	}
	cutoff := now.Add(-b.jobLimits.Retention)
	b.provMu.Lock()
	defer b.provMu.Unlock()
	for id, job := range b.provisionJobs {
		job.mu.Lock()
		terminal := isTerminalProcessStatus(job.status)
		end := job.endedAt
		job.mu.Unlock()
		if terminal && !end.IsZero() && end.Before(cutoff) {
			delete(b.provisionJobs, id)
		}
	}
}

// interruptJobsForWorkspace marks every non-terminal persisted job for a
// workspace as failed, for the case where the workspace's runtime is already
// gone (a leftover destroyed by ID, with no in-memory process to probe).
func (b *Backend) interruptJobsForWorkspace(workspaceID string, now time.Time) {
	if b.jobStore == nil {
		return
	}
	for _, e := range b.jobStore.ListForWorkspace(workspaceID) {
		if isTerminalProcessStatus(e.Status) {
			continue
		}
		e.Status = internal.ProcessFailed
		if e.EndedAt.IsZero() {
			e.EndedAt = now
		}
		if err := b.jobStore.Put(e); err != nil {
			logger.Warn("interrupt_job_failed", "job_id", e.ID, "error", err)
		}
	}
}

// finalizeJobsForWorkspace marks any still-active jobs for a workspace as
// interrupted and captures their output tails before the workspace's runtime
// is torn down. Persisted records are retained for the retention window.
func (b *Backend) finalizeJobsForWorkspace(ctx context.Context, wsState *workspaceState) {
	prefix := wsState.workspace.ID + "/"
	b.procMu.Lock()
	var procs []*managedProcess
	for key, proc := range b.processes {
		if strings.HasPrefix(key, prefix) {
			procs = append(procs, proc)
		}
	}
	b.procMu.Unlock()

	// Probe before finalizing: a job that succeeded but was never polled must
	// be recorded as succeeded, not interrupted. The runtime is still up at
	// this point (callers finalize before tearing it down); use a context not
	// tied to the caller so a cancelled request still gets the real result.
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	for _, proc := range procs {
		if done, err := b.reconcileProcess(probeCtx, wsState, proc); err != nil {
			logger.Warn("process_finalize_probe_failed", "process_id", proc.id, "error", err)
		} else if done {
			// reconcileProcess already captured and persisted the real result.
			continue
		}
		proc.mu.Lock()
		if !isTerminalProcessStatus(proc.status) {
			proc.status = internal.ProcessFailed
			proc.exitCode = exitCodeForTerminal(internal.ProcessFailed)
			proc.endedAt = time.Now()
			if proc.timer != nil {
				proc.timer.Stop()
			}
			proc.finalized = true
		}
		proc.mu.Unlock()
		b.finalizeJobRecord(probeCtx, wsState, proc)
	}
}
