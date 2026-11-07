package backend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/cache"
	"github.com/davidcrumpton/mcwsd/internal/capacity"
	"github.com/davidcrumpton/mcwsd/internal/gitpush"
	"github.com/davidcrumpton/mcwsd/internal/jobs"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
	"github.com/davidcrumpton/mcwsd/internal/secrets"
	"github.com/davidcrumpton/mcwsd/internal/state"
)

// logger keeps backend lifecycle messages behind --debug while preserving
// warnings in normal logs.
var logger = &backendLogger{}

type backendLogger struct{}

func (l *backendLogger) Info(msg string, keysAndValues ...interface{}) {
	internal.Log.Debug(msg, keysAndValues...)
}

func (l *backendLogger) Warn(msg string, keysAndValues ...interface{}) {
	internal.Log.Warn(msg, keysAndValues...)
}

// Backend implements internal.WorkspaceBackend by orchestrating a
// runtime.Provisioner (Docker, Podman, FreeBSD jail, OpenBSD VMD, illumos
// zones, or Kubernetes) together with caching, environment detection, secrets, and git push logic
// that apply regardless of which runtime provisioned the workspace.
//
// Extension point: every runtime-specific API call has been moved into the
// respective Provisioner (container/jail/VM lifecycle) or ExecTransport (exec
// and file I/O) implementations. A future backend for a different runtime
// follows this same shape, swapping only the concrete provisioner.
type Backend struct {
	provisioner       runtime.Provisioner
	cacheManager      *cache.Manager
	secretBroker      *secrets.Broker
	workspaces        map[string]*workspaceState
	workspaceBasePath string
	// gitPushConfig holds the daemon-startup-time git auth configuration
	// used by CommitAndPR. It is set once, in NewBackend, from validated
	// config assembled by main.go — never read from per-request input and
	// never exposed to the container.
	gitPushConfig gitpush.Config
	// sessionID identifies this daemon run. It's generated once at
	// startup (see main.go) and stamped on every state-file entry this
	// backend creates, so entries from a previous, not-cleanly-shut-down
	// run can be told apart from the current one's.
	sessionID string
	// commitEmail and commitName are the git identity recorded on commits
	// created by CommitAndPR. They are daemon-startup-time config, set from
	// main.go, and never read from per-request input.
	commitEmail string
	commitName  string
	// stateStore persists a record of every container created, keyed by
	// workspace ID, independent of the in-memory workspaces map above.
	// It's what survives a killed/aborted process and makes leftover
	// containers findable and destroyable after a restart.
	stateStore *state.Store
	// processes tracks long-running background processes launched via
	// StartProcess, keyed by process ID. The key is workspace-scoped
	// ("<wsID>/<pid>") so the same short PID can be reused across
	// different workspaces without collision.
	processes map[string]*managedProcess
	procMu    sync.Mutex
	mu        sync.RWMutex
	// jobStore persists a record of every background job, keyed by job ID,
	// independent of the in-memory processes map. It is what lets jobs be
	// reconciled after a daemon restart and retained after their workspace
	// is destroyed. nil disables all job persistence (used by tests and any
	// embedding that does not want it).
	jobStore *jobs.Store
	// jobLimits bounds concurrent jobs and caps runtime/log size. The zero
	// value has no effect except where main.go supplies defaults.
	jobLimits jobs.Limits
	// execWaitSeconds is the daemon-wide default for how long Exec blocks
	// before promoting a command to a background job (--exec-wait-seconds).
	// 0 means never promote: Exec blocks as it always has unless the request
	// sets wait_seconds or async.
	execWaitSeconds int
	// provisionJobs tracks background provisioning pipelines, keyed by job
	// ID. They are daemon-side orchestration and share the host-side jobs
	// store and the GetProcessStatus API with in-runtime shell jobs.
	provisionJobs map[string]*provisionJob
	provMu        sync.Mutex
	// provisionWaitSeconds is the daemon-wide default for how long Provision
	// blocks before promoting the whole pipeline to a background job
	// (--provision-wait-seconds). 0 means never promote.
	provisionWaitSeconds int
	// provisionTimeout, when > 0, bounds every provisioning pipeline
	// (--provision-timeout). 0 leaves it bounded only by per-step timeouts
	// (e.g. a VM's boot timeout).
	provisionTimeout time.Duration
	// capacityChecker, when non-nil, gates provisioning on host resource
	// usage. It is consulted at the top of Provision; a failed check
	// refuses the workspace before any clone or container work begins. It
	// is set once via WithCapacityCheck, from daemon-startup-time config.
	capacityChecker *capacity.Checker
	// provisioningHints is daemon-level, administrator-authored guidance
	// about the provisioned environment (privilege escalation policy,
	// preinstalled tooling, ...) that is handed to the agent on every
	// workspace via EnvironmentProfile.Notes. It is the same for every
	// runtime, so it is not provisioner-specific. Empty means no hints.
	provisioningHints string
}

type workspaceState struct {
	workspace *internal.Workspace
	// handle is the normalized runtime.RuntimeHandle returned by the
	// provisioner at Provision time. All exec/file operations for this
	// workspace go through handle.Transport rather than any Docker-specific
	// state kept here.
	handle  *runtime.RuntimeHandle
	secrets map[string]string // granted secrets for this workspace
	// netMu serializes connect/disconnect calls for this workspace so two
	// concurrent network-requesting exec calls can't race each other into
	// leaving the container attached when it should end up disconnected.
	netMu sync.Mutex
}

// NewBackend creates a new backend. gitCfg is the daemon-startup-time
// git auth configuration used for CommitAndPR — assembled and validated by
// main.go, not read from the environment here, so there's exactly one place
// that decides how the daemon authenticates to the remote. commitEmail and
// commitName are the git identity recorded on those commits; empty values
// fall back to the built-in defaults.
func NewBackend(provisioner runtime.Provisioner, cacheDir string, maxCacheSizeGB int, gitCfg gitpush.Config, sessionID string, stateFilePath string, workspaceBasePath string, commitEmail string, commitName string) (*Backend, error) {
	cacheMgr, err := cache.NewManager(cacheDir, maxCacheSizeGB)
	if err != nil {
		return nil, fmt.Errorf("failed to create cache manager: %w", err)
	}

	secretBroker := secrets.NewBroker()

	stateStore, err := state.NewStore(stateFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open state store: %w", err)
	}

	if commitEmail == "" {
		commitEmail = "mcwsd-agent@localhost"
	}
	if commitName == "" {
		commitName = "mcwsd-agent"
	}

	return &Backend{
		provisioner:       provisioner,
		cacheManager:      cacheMgr,
		secretBroker:      secretBroker,
		workspaces:        make(map[string]*workspaceState),
		workspaceBasePath: workspaceBasePath,
		gitPushConfig:     gitCfg,
		sessionID:         sessionID,
		commitEmail:       commitEmail,
		commitName:        commitName,
		stateStore:        stateStore,
		processes:         make(map[string]*managedProcess),
		provisionJobs:     make(map[string]*provisionJob),
	}, nil
}

// WithCapacityCheck attaches a capacity checker that gates every future
// Provision call. It is a no-op when cfg has no thresholds configured.
// Returns the receiver so it can be chained after NewBackend.
func (b *Backend) WithCapacityCheck(cfg capacity.Config) *Backend {
	if cfg.Enabled() {
		b.capacityChecker = capacity.NewChecker(cfg)
	}
	return b
}

// WithProvisioningHints attaches administrator-authored guidance that is
// appended to every workspace's EnvironmentProfile.Notes, so the agent learns
// environment-specific facts it could not detect on its own (how to escalate
// privileges, which tools are preinstalled, ...). hints is the raw text loaded
// from the provisioning-hints file; empty disables it. Returns the receiver so
// it can be chained after NewBackend.
func (b *Backend) WithProvisioningHints(hints string) *Backend {
	b.provisioningHints = strings.TrimSpace(hints)
	return b
}

// WithExecWait sets the daemon-wide default for Exec's wait-then-promote
// behavior. 0 (the default) disables auto-promotion, preserving the original
// blocking behavior unless a request opts in with wait_seconds or async.
// Returns the receiver so it can be chained after NewBackend.
func (b *Backend) WithExecWait(seconds int) *Backend {
	if seconds < 0 {
		seconds = 0
	}
	b.execWaitSeconds = seconds
	return b
}

// WithProvisionWait sets the daemon-wide default for Provision's
// wait-then-promote behavior (--provision-wait-seconds). 0 (the default)
// disables auto-promotion: Provision blocks unless the request opts in with
// wait_seconds or async. Returns the receiver so it can be chained.
func (b *Backend) WithProvisionWait(seconds int) *Backend {
	if seconds < 0 {
		seconds = 0
	}
	b.provisionWaitSeconds = seconds
	return b
}

// WithProvisionTimeout bounds every provisioning pipeline
// (--provision-timeout). 0 (the default) leaves provisioning bounded only by
// per-step timeouts. Returns the receiver so it can be chained.
func (b *Backend) WithProvisionTimeout(d time.Duration) *Backend {
	if d < 0 {
		d = 0
	}
	b.provisionTimeout = d
	return b
}

// WithJobs attaches a host-side jobs store and the daemon's job limits. It
// opens (or creates) the store at path and returns the receiver, so it can be
// chained after NewBackend. An empty path disables job persistence entirely.
func (b *Backend) WithJobs(path string, limits jobs.Limits) (*Backend, error) {
	if strings.TrimSpace(path) == "" {
		return b, nil
	}
	store, err := jobs.NewStore(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open jobs store: %w", err)
	}
	b.jobStore = store
	b.jobLimits = limits
	return b, nil
}

// CleanupOrphanResources asks the provisioner to remove resources it created
// that are no longer tracked in the state file (e.g. a VM disk whose start
// crashed before a state entry was written). It is a no-op for provisioners
// that do not implement runtime.OrphanCleaner. Call it after DestroyLeftover
// has cleared the state entries of removed leftovers, so the keep-set contains
// only genuinely live workspaces.
func (b *Backend) CleanupOrphanResources(ctx context.Context) (int, error) {
	cleaner, ok := b.provisioner.(runtime.OrphanCleaner)
	if !ok {
		return 0, nil
	}
	keep := make(map[string]bool)
	for _, e := range b.stateStore.List() {
		keep[e.ContainerID] = true
	}
	return cleaner.CleanupOrphans(ctx, keep)
}

// Leftovers returns state-file entries whose session_id doesn't match this
// backend's current session — i.e. containers created by a previous daemon
// run that were never destroyed. Call this right after NewBackend, before
// any new workspaces are provisioned, to report or clean up what an
// aborted previous run left behind.
func (b *Backend) Leftovers() []state.Entry {
	return b.stateStore.Orphans(b.sessionID)
}

// DestroyLeftover force-removes a container tracked in the state file by
// container ID and clears its entry, without requiring the workspace to be
// registered in this process's in-memory map. This is what makes cleanup
// possible across a daemon restart: normal Destroy only knows about
// workspaces provisioned by the currently running process, but a leftover
// from a previous, killed run was never in this process's memory at all.
func (b *Backend) DestroyLeftover(ctx context.Context, workspaceID string) error {
	entry, ok := b.stateStore.Get(workspaceID)
	if !ok {
		return fmt.Errorf("no tracked entry for workspace %s", workspaceID)
	}

	if err := b.provisioner.DestroyByID(ctx, entry.ContainerID); err != nil {
		return fmt.Errorf("failed to remove leftover container %s: %w", entry.ContainerName, err)
	}

	// Any job record for this workspace can no longer be running; mark the
	// non-terminal ones interrupted so they are not reported as live.
	b.interruptJobsForWorkspace(workspaceID, time.Now())

	return b.stateStore.Remove(workspaceID)
}

// touch records that a workspace was used, bumping its last-accessed time
// in the state file so the pruner doesn't reap a workspace that's still in
// use. Called at the top of every workspace accessor; a failed write is
// non-fatal (the workspace just looks a little older than it is).
func (b *Backend) touch(workspaceID string) {
	if err := b.stateStore.Touch(workspaceID); err != nil {
		logger.Warn("state_store_touch_failed", "workspace_id", workspaceID, "error", err)
	}
}

// Prune destroys and untracks every workspace that hasn't been accessed
// within ttl. It's the daemon's garbage collector for workspaces the agent
// forgot to destroy: containers are force-removed (via DestroyLeftover, so
// it works across a daemon restart too) and their state-file entries are
// cleared. Returns the number removed and the number that failed to remove.
func (b *Backend) Prune(ctx context.Context, ttl time.Duration) (removed, failed int) {
	cutoff := time.Now().Add(-ttl)
	entries := b.stateStore.Stale(cutoff)
	for _, e := range entries {
		// Drop any in-memory state first so a pruned workspace no longer
		// appears live even if container removal below hits a hiccup. Any
		// leftover state-file entry that survives a failed removal is still
		// tracked and will be retried on the next prune pass. Job results are
		// captured before the container goes away.
		b.mu.Lock()
		wsState, hadWs := b.workspaces[e.WorkspaceID]
		delete(b.workspaces, e.WorkspaceID)
		b.mu.Unlock()
		if hadWs {
			b.finalizeJobsForWorkspace(ctx, wsState)
		}
		b.removeProcessesForWorkspace(e.WorkspaceID)

		if err := b.DestroyLeftover(ctx, e.WorkspaceID); err != nil {
			logger.Warn("prune_failed", "workspace_id", e.WorkspaceID, "container_name", e.ContainerName, "error", err)
			failed++
			continue
		}
		removed++
		lastUsed := e.LastAccessedAt
		if lastUsed.IsZero() {
			lastUsed = e.CreatedAt
		}
		logger.Info("pruned_workspace", "workspace_id", e.WorkspaceID, "container_name", e.ContainerName,
			"created_at", e.CreatedAt.Format(time.RFC3339), "last_accessed_at", lastUsed.Format(time.RFC3339))
	}
	return removed, failed
}

// RecoverWorkspaces recovers workspaces from a previous daemon session that still
// have their containers/workspaces running. These workspaces are added to the
// in-memory workspaces map and their state file entries are updated with the
// current session ID so they won't be treated as leftovers in the future.
func (b *Backend) RecoverWorkspaces(ctx context.Context) {
	// Get all entries from the state file
	entries := b.stateStore.List()

	for _, e := range entries {
		// Skip entries that already belong to this session
		if e.SessionID == b.sessionID {
			continue
		}

		// Try to recover the workspace using the provisioner
		recoverer, ok := b.provisioner.(runtime.WorkspaceRecoverer)
		if !ok {
			logger.Warn("workspace_recovery_not_supported", "workspace_id", e.WorkspaceID, "provisioner", b.provisioner.Name())
			continue
		}

		handle, err := recoverer.RecoverHandle(ctx, e.ContainerID, e.WorkDir)
		if err != nil {
			// Workspace no longer exists, skip it
			logger.Info("workspace_recovery_skipped", "workspace_id", e.WorkspaceID, "container_id", e.ContainerID, "reason", err.Error())
			continue
		}

		// Create a minimal Workspace object from the state entry
		ws := &internal.Workspace{
			ID:        e.WorkspaceID,
			Spec:      internal.WorkspaceSpec{RepoURL: e.RepoURL},
			OS:        e.OS,
			WorkDir:   e.WorkDir,
			CreatedAt: e.CreatedAt,
		}

		// Create workspaceState and add to the map
		wsState := &workspaceState{
			workspace: ws,
			handle:    handle,
			secrets:   make(map[string]string),
		}

		b.mu.Lock()
		b.workspaces[e.WorkspaceID] = wsState
		b.mu.Unlock()

		// Update the state file entry with the current session ID
		// We need to update it to mark it as adopted
		newEntry := e
		newEntry.SessionID = b.sessionID
		if err := b.stateStore.Add(newEntry); err != nil {
			logger.Warn("workspace_recovery_state_update_failed", "workspace_id", e.WorkspaceID, "error", err)
			// Workspace is in memory but state file wasn't updated.
			// It will be lost on next restart but works for this session.
		}

		logger.Info("workspace_recovered", "workspace_id", e.WorkspaceID, "container_id", e.ContainerID, "os", e.OS)
	}
}

func isPublicRepo(repoURL string) bool {
	// Check repoURL for http or HTTPS instead of list of known public hosts
	// AFAIK, you can't grab a repo with the SSH login without authentication
	return strings.HasPrefix(repoURL, "https://") || strings.HasPrefix(repoURL, "http://")
}

// detachNetwork disconnects a workspace's network using a context that is
// not tied to the caller's request. Network teardown is cleanup: it must
// still run when the triggering request was cancelled (client timeout or
// disconnect). Reusing the request context here used to skip the disconnect
// entirely on cancellation, leaving the container attached to the bridge so
// every later SetNetwork(true) failed with "endpoint ... already exists".
func detachNetwork(ctx context.Context, transport runtime.ExecTransport, wsID string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := transport.SetNetwork(cleanupCtx, false); err != nil {
		logger.Warn("network_disconnect_failed", "workspace_id", wsID, "error", err)
	}
}

// Exec runs a command in the workspace.
func (b *Backend) Exec(ctx context.Context, ws *internal.Workspace, req internal.ExecRequest) (*internal.ExecResult, error) {
	logger.Info("exec_start", "workspace_id", ws.ID, "command", fmt.Sprintf("%q", req.Command),
		"allow_network", req.AllowNetwork, "timeout_seconds", req.TimeoutSeconds)

	b.mu.RLock()
	wsState, ok := b.workspaces[ws.ID]
	b.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("workspace %s not found", ws.ID)
	}

	b.touch(ws.ID)

	// Decide whether to run this command as a background job.
	//   - async: explicit background start; returns a job_id immediately and
	//     counts against the background limits (rejected before launch if
	//     full).
	//   - wait_seconds (or --exec-wait-seconds) > 0: run as a foreground job,
	//     wait that long, and return inline output if it finishes; otherwise
	//     return the job_id. Foreground jobs are exempt from the limits, so a
	//     full background queue can never strand one.
	// Secrets are synchronous-only: their lifetime is tied to the call, so a
	// request carrying secrets always blocks. Async with secrets is rejected.
	if len(req.Secrets) > 0 {
		if req.Async {
			return nil, fmt.Errorf("async exec is not supported for secret-bearing commands")
		}
	} else {
		if req.Async {
			return b.execAsJob(ctx, wsState, ws, req, 0)
		}
		wait := req.WaitSeconds
		if wait <= 0 {
			wait = b.execWaitSeconds
		}
		if wait > 0 {
			return b.execAsJob(ctx, wsState, ws, req, wait)
		}
	}

	// Grant secrets
	for _, secretRef := range req.Secrets {
		if err := b.secretBroker.Grant(ctx, ws.ID, secretRef, time.Duration(req.TimeoutSeconds)*time.Second); err != nil {
			return nil, fmt.Errorf("failed to grant secret %s: %w", secretRef, err)
		}
		wsState.secrets[secretRef] = "granted"
	}

	// Defer revoke
	defer func() {
		for secretRef := range wsState.secrets {
			if err := b.secretBroker.Revoke(ctx, ws.ID, secretRef); err != nil {
				logger.Warn("secret_revoke_failed", "workspace_id", ws.ID, "secret", secretRef, "error", err)
			}
		}
	}()

	// Network is off by default (disconnected at the end of Provision).
	// Only reattach for the duration of this exec call if explicitly
	// requested, and always disconnect again afterward so the workspace
	// returns to its network-off resting state.
	if req.AllowNetwork {
		wsState.netMu.Lock()
		if err := wsState.handle.Transport.SetNetwork(ctx, true); err != nil {
			wsState.netMu.Unlock()
			return nil, fmt.Errorf("failed to attach network for exec: %w", err)
		}
		defer func() {
			detachNetwork(ctx, wsState.handle.Transport, ws.ID)
			wsState.netMu.Unlock()
		}()
	}

	// Bound the command with a deadline when the caller supplied one. Without
	// this the command runs until the HTTP client gives up and drops the
	// connection; enforcing it here kills the process at the intended time and
	// lets us return a real error instead of a client-side read timeout.
	execCtx := ctx
	if req.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	startTime := time.Now()
	result, err := wsState.handle.Transport.Exec(execCtx, []string{"sh", "-c", req.Command}, wsState.handle.WorkDir, nil)
	durationMs := time.Since(startTime).Milliseconds()

	if err != nil {
		logger.Info("exec_complete", "workspace_id", ws.ID, "success", false, "duration_ms", durationMs)
		return nil, err
	}

	// Scrub secrets from output
	result.Stdout = b.scrubSecrets(result.Stdout, req.Secrets)
	result.Stderr = b.scrubSecrets(result.Stderr, req.Secrets)

	result.DurationMs = durationMs
	logger.Info("exec_complete", "workspace_id", ws.ID, "exit_code", result.ExitCode, "duration_ms", durationMs)
	return result, nil
}

// Artifacts returns diffs and new files from the workspace.
func (b *Backend) Artifacts(ctx context.Context, ws *internal.Workspace, paths []string) ([]internal.Artifact, error) {
	b.mu.RLock()
	wsState, ok := b.workspaces[ws.ID]
	b.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("workspace %s not found", ws.ID)
	}

	b.touch(ws.ID)

	var artifacts []internal.Artifact

	// Get unstaged diff
	diffArgs := []string{"git", "diff"}
	diffArgs = append(diffArgs, paths...)
	diffResult, err := wsState.handle.Transport.Exec(ctx, diffArgs, wsState.handle.WorkDir, nil)
	if err == nil && diffResult.ExitCode == 0 && diffResult.Stdout != "" {
		artifacts = append(artifacts, internal.Artifact{
			Name:    "changes.patch",
			Content: []byte(diffResult.Stdout),
			Type:    "patch",
		})
	}

	// Get staged diff
	stagedResult, err := wsState.handle.Transport.Exec(ctx, []string{"git", "diff", "--staged"}, wsState.handle.WorkDir, nil)
	if err == nil && stagedResult.ExitCode == 0 && stagedResult.Stdout != "" {
		artifacts = append(artifacts, internal.Artifact{
			Name:    "staged.patch",
			Content: []byte(stagedResult.Stdout),
			Type:    "patch",
		})
	}

	// Get untracked files
	untrackedResult, err := wsState.handle.Transport.Exec(ctx, []string{"git", "ls-files", "--others", "--exclude-standard"}, wsState.handle.WorkDir, nil)
	if err == nil && untrackedResult.ExitCode == 0 && untrackedResult.Stdout != "" {
		files := strings.Split(strings.TrimSpace(untrackedResult.Stdout), "\n")
		for _, file := range files {
			if file == "" {
				continue
			}
			content, err := wsState.handle.Transport.ReadFile(ctx, filepath.Join(wsState.handle.WorkDir, file))
			if err == nil {
				artifacts = append(artifacts, internal.Artifact{
					Name:    file,
					Content: content,
					Type:    "file",
				})
			}
		}
	}

	return artifacts, nil
}

// CommitAndPR commits the working tree inside the container to a
// server-chosen branch, bundles that branch, and hands the bundle off to
// the host-side gitpush package to push and open a merge request. The
// container never gets network access or credentials for this operation —
// everything it does here is local git object manipulation.
func (b *Backend) CommitAndPR(ctx context.Context, ws *internal.Workspace, req internal.CommitAndPRRequest) (*internal.CommitAndPRResult, error) {
	b.mu.RLock()
	wsState, ok := b.workspaces[ws.ID]
	b.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("workspace %s not found", ws.ID)
	}

	b.touch(ws.ID)

	if req.CommitMessage == "" {
		return nil, fmt.Errorf("commit_message is required")
	}
	if req.PRTitle == "" {
		return nil, fmt.Errorf("pr_title is required")
	}
	if b.gitPushConfig.AuthMode == "" {
		return nil, fmt.Errorf("commit_and_pr is unavailable: no forge authentication is configured")
	}

	// The branch name is generated here, server-side. The agent has no way
	// to influence it beyond a short slug derived from the title, so it
	// can never target an existing branch (including main/master) or
	// collide with another workspace.
	branch := fmt.Sprintf("agent/%s/%s", ws.ID, slugify(req.PRTitle))

	// Mark the workspace repo as a safe git directory before touching it.
	//
	// On Docker the repo is a bind mount of a host directory owned by the
	// daemon's unprivileged user, while git runs as root inside the
	// container, so git refuses with "detected dubious ownership" (its
	// CVE-2022-24765 check). Docker workspaces get this from the transport's
	// Bootstrap at provision time, but a workspace recovered after a daemon
	// restart — or one provisioned before that bootstrap existed — never runs
	// it. Setting it here makes CommitAndPR work regardless. Failure is
	// non-fatal: on runtimes where the check does not apply (for example a
	// checkout owned by the invoking user) this is unnecessary and may be
	// unwritable, and the git steps below will still work.
	if res, err := wsState.handle.Transport.Exec(ctx,
		[]string{"git", "config", "--system", "--add", "safe.directory", "*"}, wsState.handle.WorkDir, nil); err != nil {
		logger.Warn("safe_directory_config_failed", "workspace_id", ws.ID, "error", err)
	} else if res != nil && res.ExitCode != 0 {
		logger.Warn("safe_directory_config_failed", "workspace_id", ws.ID, "exit_code", res.ExitCode, "stderr", strings.TrimSpace(res.Stderr))
	}

	// Give commits a fixed, non-spoofable identity rather than trusting
	// whatever (if anything) is configured inside the container. The
	// identity is daemon-startup-time config, not per-request input.
	identityCmds := [][]string{
		{"git", "config", "user.email", b.commitEmail},
		{"git", "config", "user.name", b.commitName},
	}
	for _, c := range identityCmds {
		if _, err := wsState.handle.Transport.Exec(ctx, c, wsState.handle.WorkDir, nil); err != nil {
			return nil, fmt.Errorf("failed to set git identity: %w", err)
		}
	}

	// All git operations below use argv form (no "sh -c"), so a commit
	// message or title containing shell metacharacters can't do anything
	// beyond being a literal argument to git.
	if _, err := wsState.handle.Transport.Exec(ctx,
		[]string{"git", "checkout", "-b", branch}, wsState.handle.WorkDir, nil); err != nil {
		return nil, fmt.Errorf("failed to create branch: %w", err)
	}

	if _, err := wsState.handle.Transport.Exec(ctx,
		[]string{"git", "add", "-A"}, wsState.handle.WorkDir, nil); err != nil {
		return nil, fmt.Errorf("failed to stage changes: %w", err)
	}

	statusResult, err := wsState.handle.Transport.Exec(ctx,
		[]string{"git", "status", "--porcelain"}, wsState.handle.WorkDir, nil)
	if err == nil && statusResult.ExitCode == 0 && statusResult.Stdout == "" {
		// Don't fail hard here, just return an error message that can be displayed in the UI.
		// We have to do something to indicate that the PR couldn't be created
		// without blowing up the whole request. Accompany it with a factual
		// snapshot of the working tree (including ignored files) so the caller
		// can see *why* nothing was staged — most often a newly created file
		// that .gitignore excludes — and decide what to do, rather than the
		// daemon force-adding files on their behalf.
		pushResult := internal.CommitAndPRResult{
			MRURL:           "",
			Branch:          "",
			Error:           "no changes to commit",
			NoChangesDetail: b.gitStatusDetail(ctx, wsState),
		}
		return &pushResult, nil
	}

	commitResult, err := wsState.handle.Transport.Exec(ctx,
		[]string{"git", "commit", "-m", req.CommitMessage}, wsState.handle.WorkDir, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to commit: %w", err)
	}
	if commitResult.ExitCode != 0 {
		return nil, fmt.Errorf("git commit failed (exit %d): %s", commitResult.ExitCode, commitResult.Stderr)
	}

	const bundlePath = "/tmp/wsd-out.bundle"
	bundleResult, err := wsState.handle.Transport.Exec(ctx,
		[]string{"git", "bundle", "create", bundlePath, branch}, wsState.handle.WorkDir, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create bundle: %w", err)
	}
	if bundleResult.ExitCode != 0 {
		return nil, fmt.Errorf("git bundle create failed (exit %d): %s", bundleResult.ExitCode, bundleResult.Stderr)
	}

	bundleBytes, err := wsState.handle.Transport.ReadFile(ctx, bundlePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read bundle from container: %w", err)
	}

	// Host-side from here on. No container involvement, no container
	// credentials. Config was assembled and validated once, at daemon
	// startup, by main.go — sourced from GKM there.
	//
	// The host-side clone/ls-remote/push below run in the daemon process and
	// need working name resolution and outbound network. On the non-VNET
	// FreeBSD jail model the workspace shares the host's network stack, and a
	// workspace left network-disabled (Provision/Exec end with
	// SetNetwork(false)) can leave the host unable to resolve until the
	// jail's IPv4 is re-enabled. Reattach it for the duration of the push,
	// then return the workspace to its network-off resting state. Containers
	// are isolated from the host, so their network state has no bearing on
	// the daemon's own push and is left untouched.
	if wsState.handle.Isolation == runtime.IsolationJail {
		wsState.netMu.Lock()
		defer wsState.netMu.Unlock()
		if err := wsState.handle.Transport.SetNetwork(ctx, true); err != nil {
			return nil, fmt.Errorf("failed to attach network for commit and PR: %w", err)
		}
		defer func() {
			detachNetwork(ctx, wsState.handle.Transport, ws.ID)
		}()
	}

	// Use req.RepoURL if provided, otherwise fall back to workspace's RepoURL
	repoURL := req.RepoURL
	if repoURL == "" {
		repoURL = wsState.workspace.Spec.RepoURL
	}

	projectPath := req.ProjectPath
	if projectPath == "" {
		projectPath = req.GitLabProjectPath
	}
	pushResult, err := gitpush.PushAndOpenMR(ctx, b.gitPushConfig, gitpush.Request{
		RepoURL:           repoURL,
		BaseBranch:        wsState.workspace.Spec.Ref,
		Branch:            branch,
		Bundle:            bundleBytes,
		Title:             req.PRTitle,
		Description:       req.PRDescription,
		ProjectPath:       projectPath,
		GitLabProjectPath: req.GitLabProjectPath,
	})
	if err != nil {
		return nil, err
	}

	return &internal.CommitAndPRResult{
		Branch: pushResult.Branch,
		MRURL:  pushResult.MRURL,
	}, nil
}

// gitStatusDetail returns a short factual snapshot of the working tree to
// accompany a "no changes to commit" result. It includes ignored files (via
// --ignored) because those are the most common reason a change appears to be
// missing: a newly created file that the repo's .gitignore excludes is
// invisible to `git add -A` and therefore to this commit. An empty string is
// returned when git can't produce a status (the caller still gets the plain
// "no changes to commit" error).
func (b *Backend) gitStatusDetail(ctx context.Context, wsState *workspaceState) string {
	res, err := wsState.handle.Transport.Exec(ctx,
		[]string{"git", "status", "--porcelain", "--ignored"}, wsState.handle.WorkDir, nil)
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

// Destroy tears down a workspace.
func (b *Backend) Destroy(ctx context.Context, ws *internal.Workspace) error {
	logger.Info("destroy", "workspace_id", ws.ID)

	b.mu.Lock()
	wsState, ok := b.workspaces[ws.ID]
	if ok {
		delete(b.workspaces, ws.ID)
	}
	b.mu.Unlock()

	// Capture job results and mark still-active jobs interrupted before the
	// container is torn down, then drop them from memory. Persisted records
	// are retained for the configured retention window.
	if ok {
		b.finalizeJobsForWorkspace(ctx, wsState)
	}
	b.removeProcessesForWorkspace(ws.ID)

	if !ok {
		// Not in this process's memory. Either it was already destroyed,
		// it never existed, or — the case this matters for — it's a
		// leftover from a previous daemon run that this process never
		// provisioned itself. Check the state file before giving up, so
		// destroy calls against a leftover workspace ID (e.g. from a
		// stale agent session, or manual cleanup after a restart) still
		// actually remove the container instead of silently no-oping.
		if _, tracked := b.stateStore.Get(ws.ID); tracked {
			return b.DestroyLeftover(ctx, ws.ID)
		}
		if destroyer, ok := b.provisioner.(runtime.WorkspaceIDDestroyer); ok {
			return destroyer.DestroyWorkspaceByID(ctx, ws.ID)
		}
		return nil // Already destroyed or never existed
	}

	// Revoke all secrets
	for secretRef := range wsState.secrets {
		if err := b.secretBroker.Revoke(ctx, ws.ID, secretRef); err != nil {
			logger.Warn("secret_revoke_failed", "workspace_id", ws.ID, "secret", secretRef, "error", err)
		}
	}

	// Remove container. Only clear the state-file entry once this
	// actually succeeds — if it fails, the entry stays behind as a
	// record of a container that still needs cleanup.
	if err := b.provisioner.Destroy(ctx, wsState.handle); err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}
	if err := b.stateStore.Remove(ws.ID); err != nil {
		logger.Warn("state_store_remove_failed", "workspace_id", ws.ID, "error", err)
	}

	logger.Info("destroy_complete", "workspace_id", ws.ID)
	return nil
}

// SafeEdit performs a literal string replacement of pattern with replacement
// inside filePath within the workspace's runtime. The operation is done
// entirely in Go memory via the runtime's ExecTransport — no sed, no shell,
// no escaping hazards.
//
// Returns an error if the pattern is not found in the file (so the caller
// always knows whether the edit actually took effect).
func (b *Backend) SafeEdit(ctx context.Context, ws *internal.Workspace, filePath, pattern, replacement string) error {
	b.mu.RLock()
	wsState, ok := b.workspaces[ws.ID]
	b.mu.RUnlock()
	if !ok {
		return fmt.Errorf("workspace %s not found", ws.ID)
	}

	b.touch(ws.ID)

	resolved, err := resolveWorkspacePath(wsState.handle.WorkDir, filePath)
	if err != nil {
		return err
	}

	// 1. Pull the file out of the runtime.
	original, err := wsState.handle.Transport.ReadFile(ctx, resolved)
	if err != nil {
		return fmt.Errorf("SafeEdit: read %s: %w", resolved, err)
	}

	// 2. Perform the replacement in Go — handles any bytes including newlines,
	//    forward slashes, quotes, or special regex characters without escaping.
	originalStr := string(original)
	if !strings.Contains(originalStr, pattern) {
		return fmt.Errorf("SafeEdit: pattern not found in %s", resolved)
	}
	modified := strings.ReplaceAll(originalStr, pattern, replacement)

	// 3. Write the modified content back, preserving a sensible file mode.
	if err := wsState.handle.Transport.WriteFile(ctx, resolved, []byte(modified), 0644); err != nil {
		return fmt.Errorf("SafeEdit: write %s: %w", resolved, err)
	}

	return nil
}

// Helper functions

func generateWorkspaceID() string {
	return fmt.Sprintf("ws-%x", sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano()))))[:12]
}

// slugify produces a short, filesystem/branch-name-safe slug from
// arbitrary agent-supplied text. It is deliberately lossy: anything not
// alphanumeric becomes a dash, and the result is capped in length so a
// pathological title can't be used to build an oversized or malformed ref.
func slugify(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "change"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

func cloneRepo(ctx context.Context, dir, url, ref string, gitCfg gitpush.Config) error {
	args := append([]string{}, gitpush.GitAuthArgs(gitCfg, url)...)
	args = append(args, "clone", "--depth", "1")
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, url, dir)

	cmd := execCommandContext(ctx, "git", args...)
	cmd.Env = gitpush.GitEnv(gitCfg)
	// Capture stdout and stderr so a failed clone reports git's own
	// diagnostics (auth failure, bad ref, unreachable host, ...) instead of a
	// bare "exit status 128". The argv is echoed too, with any token-mode
	// Authorization header redacted, so the failing command is reproducible.
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		output := strings.TrimSpace(out.String())
		if output == "" {
			return fmt.Errorf("git %s failed: %w", strings.Join(gitpush.RedactArgs(args), " "), err)
		}
		return fmt.Errorf("git %s failed: %w: %s", strings.Join(gitpush.RedactArgs(args), " "), err, output)
	}
	return nil
}

// systemPackageCommands maps a runtime OS (and, for Linux, its base image)
// to the package manager and install-command prefix used to add system-level
// tools. This is what tells an agent that "package_manager: none" does not
// mean "nothing can be installed": a FreeBSD jail still has pkg(8), a Debian
// container still has apt, and so on.
func systemPackageCommands(os runtime.OS, baseImage string) (manager, install string) {
	switch os {
	case runtime.OSFreeBSD:
		return "pkg", "pkg install -y"
	case runtime.OSIllumos, runtime.OSOpenIndiana:
		// OpenIndiana and Illumos use pkg(5) but the install command is just "pkg install" without "-y"
		return "pkg", "pkg install"
	case runtime.OSOpenBSD:
		return "pkg_add", "pkg_add"
	case runtime.OSNetBSD, runtime.OSMinix:
		return "pkgin", "pkgin -y install"
	case runtime.OSDarwin:
		return "brew", "brew install"
	case runtime.OSLinux:
		if strings.Contains(strings.ToLower(baseImage), "alpine") {
			return "apk", "apk add"
		}
		return "apt", "apt-get install -y"
	default:
		return "", ""
	}
}

func computeCacheKey(repoPath string, profile *internal.EnvironmentProfile) (string, error) {
	var files []string

	switch profile.Language {
	case "go":
		files = []string{"go.mod", "go.sum"}
	case "rust":
		files = []string{"Cargo.toml", "Cargo.lock", "rust-toolchain.toml"}
	case "python":
		if profile.PackageManager == "poetry" {
			files = []string{"pyproject.toml", "poetry.lock"}
		} else {
			files = []string{"requirements.txt"}
		}
	case "node":
		files = []string{"package.json", "pnpm-lock.yaml", "yarn.lock", "package-lock.json"}
	default:
		return "", fmt.Errorf("unknown language for caching: %s", profile.Language)
	}

	h := sha256.New()
	for _, f := range files {
		path := filepath.Join(repoPath, f)
		data, err := os.ReadFile(path)
		if err == nil {
			h.Write(data)
		}
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

func (b *Backend) saveCache(cacheKey string, profile *internal.EnvironmentProfile) error {
	// Determine what to cache based on language
	var cachePaths []string
	switch profile.Language {
	case "go":
		cachePaths = []string{"/go/pkg/mod"}
	case "rust":
		cachePaths = []string{"/root/.cargo/registry", "/root/.cargo/git"}
	case "python":
		cachePaths = []string{"/root/.cache/pip"}
	case "node":
		cachePaths = []string{"/root/.npm"}
	}

	for _, cp := range cachePaths {
		// Extract and save to cache manager
		// Simplified: just mark as cached for now
		b.cacheManager.Save(cacheKey, cp)
	}
	return nil
}

func (b *Backend) scrubSecrets(output string, secretRefs []string) string {
	// In a real implementation, this would look up actual secret values
	// and redact them. For now, return as-is since broker handles injection.
	return output
}

// execCommandContext is a helper to run commands with context.
func execCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = "/"
	return cmd
}

// --- File System Tools ---

// resolveWorkspacePath anchors an agent-supplied path to the workspace's
// repo directory. Relative paths are resolved against workDir — the same
// directory the Exec tool uses as its shell working directory — so a bare
// "test_run.sh" names the same file to both the file tools and the shell.
//
// Absolute paths are accepted if they stay inside workDir. Models frequently
// assume the repo root is the workspace base directory ("/workspace") rather
// than "/workspace/repo", so an absolute path that sits at or under workDir's
// parent is re-rooted inside the workspace instead of failing. Anything else
// that escapes is rejected rather than silently landing outside the repo
// (e.g. a stray "../../test_run.sh" at the runtime root).
func resolveWorkspacePath(workDir, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path is required")
	}
	root := filepath.Clean(workDir)

	if !filepath.IsAbs(path) {
		resolved := filepath.Clean(filepath.Join(root, path))
		if withinDir(root, resolved) {
			return resolved, nil
		}
		return "", outsideWorkspaceErr(path, root)
	}

	resolved := filepath.Clean(path)
	if withinDir(root, resolved) {
		return resolved, nil
	}

	parent := filepath.Dir(root)
	if resolved == parent {
		return root, nil
	}
	if rel, err := filepath.Rel(parent, resolved); err == nil &&
		rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		candidate := filepath.Clean(filepath.Join(root, rel))
		if withinDir(root, candidate) {
			return candidate, nil
		}
	}

	return "", outsideWorkspaceErr(path, root)
}

// withinDir reports whether path is root itself or lives beneath it.
func withinDir(root, path string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

func outsideWorkspaceErr(path, root string) error {
	return fmt.Errorf("path %q is outside the workspace; the workspace root is %s (use a path relative to it, or an absolute path under it)", path, root)
}

// shellQuote wraps s in single quotes for safe interpolation into an
// sh -c command, escaping any embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildListFilesCommand returns the shell command ListFiles runs to enumerate
// the immediate children of dir. It deliberately avoids GNU find's -printf
// (absent on OpenBSD, incomplete on FreeBSD) and uses the portable
// `-exec ... {} +` form with a POSIX sh snippet instead. Output is one
// tab-separated line per entry: <type>\t<size>\t<name>, where type is "d" for
// a directory and "f" otherwise.
func buildListFilesCommand(dir string) string {
	return fmt.Sprintf(
		`find %s -maxdepth 1 -mindepth 1 -exec sh -c 'for f do if [ -d "$f" ]; then t=d; else t=f; fi; sz=$(wc -c < "$f" 2>/dev/null | tr -d "[:space:]"); printf "%%s\t%%s\t%%s\n" "$t" "${sz:-0}" "${f##*/}"; done' sh {} +`,
		shellQuote(dir),
	)
}

// ListFiles returns a structured listing of files and directories at path
// inside the workspace. It runs `find` with -maxdepth 1 to list only the
// immediate children, then stat-queries size for files.
func (b *Backend) ListFiles(ctx context.Context, ws *internal.Workspace, path string) (*internal.ListFilesResult, error) {
	b.mu.RLock()
	wsState, ok := b.workspaces[ws.ID]
	b.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("workspace %s not found", ws.ID)
	}

	b.touch(ws.ID)

	resolved, err := resolveWorkspacePath(wsState.handle.WorkDir, path)
	if err != nil {
		return nil, fmt.Errorf("ListFiles: %w", err)
	}

	// Use find(1) to enumerate immediate children, but not GNU's -printf:
	// OpenBSD's find(1) has no -printf at all, and FreeBSD's does not
	// implement every directive. Instead, hand each match to a tiny POSIX sh
	// snippet via `-exec ... {} +`, which GNU, FreeBSD and OpenBSD find(1)
	// all support, and let that print a portable tab-separated line:
	//   <type>\t<size>\t<name>
	// where type is 'd' for directory and 'f' for file. The basename is taken
	// with ${f##*/} (not basename(1), and not GNU's %f) so one command works
	// on every target OS.
	findCmd := buildListFilesCommand(resolved)
	result, err := wsState.handle.Transport.Exec(ctx, []string{"sh", "-c", findCmd}, wsState.handle.WorkDir, nil)
	if err != nil {
		return nil, fmt.Errorf("ListFiles: exec failed: %w", err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("ListFiles: find failed (exit %d): %s", result.ExitCode, result.Stderr)
	}

	var entries []internal.FileEntry
	for _, line := range strings.Split(strings.TrimSpace(result.Stdout), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		fType, sizeStr, name := parts[0], parts[1], parts[2]
		size, _ := strconv.ParseInt(sizeStr, 10, 64)
		entries = append(entries, internal.FileEntry{
			Name:  name,
			Path:  filepath.Join(resolved, name),
			IsDir: fType == "d",
			Size:  size,
		})
	}

	return &internal.ListFilesResult{Entries: entries}, nil
}

// ReadFile reads the contents of a specific file inside the workspace.
func (b *Backend) ReadFile(ctx context.Context, ws *internal.Workspace, path string) (*internal.ReadFileResult, error) {
	b.mu.RLock()
	wsState, ok := b.workspaces[ws.ID]
	b.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("workspace %s not found", ws.ID)
	}

	b.touch(ws.ID)

	resolved, err := resolveWorkspacePath(wsState.handle.WorkDir, path)
	if err != nil {
		return nil, fmt.Errorf("ReadFile: %w", err)
	}

	content, err := wsState.handle.Transport.ReadFile(ctx, resolved)
	if err != nil {
		return nil, fmt.Errorf("ReadFile: %w", err)
	}
	return &internal.ReadFileResult{Content: string(content)}, nil
}

// WriteFile writes text content to a file inside the workspace, creating
// or overwriting it. Uses a sensible default mode of 0644.
func (b *Backend) WriteFile(ctx context.Context, ws *internal.Workspace, path string, content string) error {
	b.mu.RLock()
	wsState, ok := b.workspaces[ws.ID]
	b.mu.RUnlock()
	if !ok {
		return fmt.Errorf("workspace %s not found", ws.ID)
	}

	b.touch(ws.ID)

	resolved, err := resolveWorkspacePath(wsState.handle.WorkDir, path)
	if err != nil {
		return fmt.Errorf("WriteFile: %w", err)
	}

	if err := wsState.handle.Transport.WriteFile(ctx, resolved, []byte(content), 0644); err != nil {
		return fmt.Errorf("WriteFile: %w", err)
	}
	return nil
}
