package backend

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/detect"
	"github.com/davidcrumpton/mcwsd/internal/jobs"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
	"github.com/davidcrumpton/mcwsd/internal/state"
)

// provisionJob tracks one in-flight (or finished) background provisioning
// pipeline. Provisioning is daemon-side orchestration — host-side clone,
// environment detection, runtime boot, repo copy, install and post-create —
// rather than a single shell command inside a runtime, so it is not a
// managedProcess and cannot be probed from runtime state files. It reuses the
// host-side jobs.Store and the same GetProcessStatus API so clients poll a
// provision job exactly like any other background job.
type provisionJob struct {
	mu          sync.Mutex
	id          string
	workspaceID string
	repoURL     string
	status      internal.ProcessStatus
	startedAt   time.Time
	endedAt     time.Time
	workspace   *internal.Workspace
	errMsg      string
	// log is the combined progress log in the same JSON-lines shape shell
	// jobs use, so the same client parser reads it.
	log       string
	finalized bool
}

// command renders the job's command field for display and the jobs store.
// repoURL is immutable after construction, so no lock is needed.
func (j *provisionJob) command() string {
	if strings.TrimSpace(j.repoURL) == "" {
		return "provision (blank workspace)"
	}
	return "provision " + j.repoURL
}

// appendLog adds one progress line to the job's combined log.
func (j *provisionJob) appendLog(line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.appendLogLocked(line)
}

// appendLogLocked adds one progress line. Caller must hold j.mu.
func (j *provisionJob) appendLogLocked(line string) {
	rec, err := json.Marshal(processLogRecord{
		TS:     time.Now().UTC().Format(time.RFC3339Nano),
		Stream: "stderr",
		Line:   line,
	})
	if err != nil {
		return
	}
	j.log += string(rec) + "\n"
}

// finish records the job's terminal state exactly once.
func (j *provisionJob) finish(ws *internal.Workspace, err error, status internal.ProcessStatus) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.finalized || isTerminalProcessStatus(j.status) {
		return
	}
	j.finalized = true
	j.status = status
	j.endedAt = time.Now()
	j.workspace = ws
	if err != nil {
		j.errMsg = err.Error()
		j.appendLogLocked("error: " + err.Error())
		return
	}
	if ws != nil {
		j.appendLogLocked(fmt.Sprintf("workspace %s ready (%s, %s)", ws.ID, ws.OS, ws.WorkDir))
	}
}

// exitCodeLocked reports the job's exit code by status. Caller must hold j.mu.
func (j *provisionJob) exitCodeLocked() int {
	if j.status == internal.ProcessSucceeded {
		return 0
	}
	if isTerminalProcessStatus(j.status) {
		return 1
	}
	return 0
}

// lookupProvisionJob returns the tracked provision job for id, if any.
func (b *Backend) lookupProvisionJob(jobID string) (*provisionJob, bool) {
	b.provMu.Lock()
	defer b.provMu.Unlock()
	job, ok := b.provisionJobs[jobID]
	return job, ok
}

// saveProvisionEntry persists a provision job to the host-side store.
func (b *Backend) saveProvisionEntry(job *provisionJob) {
	if b.jobStore == nil {
		return
	}
	command := job.command()
	job.mu.Lock()
	e := jobs.Entry{
		ID:          job.id,
		WorkspaceID: job.workspaceID,
		Command:     command,
		SessionID:   b.sessionID,
		Status:      job.status,
		ExitCode:    job.exitCodeLocked(),
		StartedAt:   job.startedAt,
		EndedAt:     job.endedAt,
		LogTail:     job.log,
		Kind:        jobs.KindProvision,
	}
	job.mu.Unlock()
	if err := b.jobStore.Put(e); err != nil {
		logger.Warn("save_provision_job_failed", "job_id", e.ID, "error", err)
	}
}

// startProvisionJob generates the workspace ID up front, registers a tracked
// provision job, and runs the whole pipeline in a detached goroutine so it
// survives the calling request being cancelled or the client disconnecting.
// timeout, when > 0, bounds the pipeline and marks it timed_out if exceeded.
func (b *Backend) startProvisionJob(ctx context.Context, spec internal.WorkspaceSpec, timeout time.Duration) (*provisionJob, error) {
	wsID := generateWorkspaceID()
	jobID := fmt.Sprintf("prov-%x", sha256.Sum256([]byte(fmt.Sprintf("%s-%d", wsID, time.Now().UnixNano()))))[:12]

	job := &provisionJob{
		id:          jobID,
		workspaceID: wsID,
		repoURL:     spec.RepoURL,
		status:      internal.ProcessQueued,
		startedAt:   time.Now(),
	}

	// Provisioning pipelines count against the daemon-wide job limit (they did
	// not before, so async provisions could spawn unbounded runtimes). Check
	// and insert under provMu so two concurrent provisions cannot both slip
	// past the same slot. activeJobCounts only nests procMu under provMu;
	// no path takes them in the opposite order.
	b.provMu.Lock()
	if b.jobLimits.MaxTotal > 0 {
		shellActive, _ := b.activeJobCounts()
		provActive := b.activeProvisionJobsLocked()
		if shellActive+provActive >= b.jobLimits.MaxTotal {
			b.provMu.Unlock()
			return nil, fmt.Errorf("daemon already has %d active job(s); limit is %d", shellActive+provActive, b.jobLimits.MaxTotal)
		}
	}
	if b.provisionJobs == nil {
		b.provisionJobs = make(map[string]*provisionJob)
	}
	b.provisionJobs[jobID] = job
	b.provMu.Unlock()

	b.saveProvisionEntry(job)

	base := context.WithoutCancel(ctx)
	var jobCtx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		jobCtx, cancel = context.WithTimeout(base, timeout)
	} else {
		jobCtx, cancel = context.WithCancel(base)
	}

	go func() {
		defer cancel()
		job.mu.Lock()
		job.status = internal.ProcessRunning
		job.mu.Unlock()
		b.saveProvisionEntry(job)

		ws, err := b.runProvision(jobCtx, wsID, spec, job.appendLog)
		switch {
		case err == nil:
			job.finish(ws, nil, internal.ProcessSucceeded)
		case errors.Is(jobCtx.Err(), context.DeadlineExceeded):
			logger.Warn("provision_timed_out", "workspace_id", job.workspaceID, "timeout", timeout)
			job.finish(nil, fmt.Errorf("provisioning timed out after %s", timeout), internal.ProcessTimedOut)
		default:
			logger.Warn("provision_failed", "workspace_id", job.workspaceID, "error", err)
			job.finish(nil, err, internal.ProcessFailed)
		}
		b.saveProvisionEntry(job)
	}()

	return job, nil
}

// activeProvisionJobsLocked counts provision pipelines that have not reached a
// terminal state. Caller must hold provMu.
func (b *Backend) activeProvisionJobsLocked() int {
	n := 0
	for _, job := range b.provisionJobs {
		job.mu.Lock()
		active := !isTerminalProcessStatus(job.status)
		job.mu.Unlock()
		if active {
			n++
		}
	}
	return n
}

// awaitProvisionJob blocks until a provision job finishes or waitSeconds
// elapses. If it finishes inline it returns the workspace; otherwise it
// returns the job_id so the caller can poll.
func (b *Backend) awaitProvisionJob(ctx context.Context, job *provisionJob, waitSeconds int) (*internal.ProvisionResult, error) {
	deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
	for {
		job.mu.Lock()
		status := job.status
		ws := job.workspace
		errMsg := job.errMsg
		job.mu.Unlock()

		if isTerminalProcessStatus(status) {
			if status == internal.ProcessSucceeded && ws != nil {
				return &internal.ProvisionResult{WorkspaceID: ws.ID, Workspace: ws, Status: status}, nil
			}
			if errMsg == "" {
				errMsg = string(status)
			}
			return nil, fmt.Errorf("provisioning failed: %s", errMsg)
		}

		if !time.Now().Before(deadline) {
			logger.Info("provision_promoted", "workspace_id", job.workspaceID, "job_id", job.id, "wait_seconds", waitSeconds)
			return &internal.ProvisionResult{WorkspaceID: job.workspaceID, JobID: job.id, Status: status}, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(execPromotePollInterval):
		}
	}
}

// provisionJobStatus renders a provision job's status for the job API, in the
// same shape as a shell job. The workspace is attached once the job succeeds.
func (b *Backend) provisionJobStatus(job *provisionJob, cursor string) *internal.ProcessStatusResult {
	cur := parseProcessCursor(cursor)

	job.mu.Lock()
	log := job.log
	status := job.status
	ws := job.workspace
	exitCode := job.exitCodeLocked()
	command := job.command()
	job.mu.Unlock()

	var output string
	if !cur.done {
		if cur.outOffset < int64(len(log)) {
			output = log[cur.outOffset:]
			cur.outOffset = int64(len(log))
		}
		if isTerminalProcessStatus(status) {
			output += exitLogRecord(exitCode)
			cur = processCursor{done: true}
		}
	}

	result := &internal.ProcessStatusResult{
		ProcessInfo: internal.ProcessInfo{
			ID:       job.id,
			Command:  command,
			Status:   status,
			ExitCode: exitCode,
		},
		Output: output,
		Cursor: cur.String(),
	}
	if status == internal.ProcessSucceeded {
		result.Workspace = ws
	}
	return result
}

// effectiveProvisionTimeout combines a per-request timeout with the daemon
// default, returning the request's value when set, otherwise the daemon's. It
// also applies --job-max-runtime, which is documented as a daemon-wide cap on
// any background job (including provisioning pipelines).
func (b *Backend) effectiveProvisionTimeout(requested int) time.Duration {
	d := b.provisionTimeout
	if requested > 0 {
		d = time.Duration(requested) * time.Second
	}
	if b.jobLimits.MaxRuntime > 0 && (d <= 0 || b.jobLimits.MaxRuntime < d) {
		d = b.jobLimits.MaxRuntime
	}
	return d
}

// Provision creates a new workspace from the given request.
//
// With no backgrounding requested it blocks and returns the finished
// workspace, exactly as before. With wait_seconds (or --provision-wait-seconds)
// it blocks for that long and, if the pipeline is still running, returns a
// job_id. With async it returns a job_id immediately. In both backgrounded
// cases the caller polls GetProcessStatus (the job API), whose terminal
// response carries the finished workspace.
func (b *Backend) Provision(ctx context.Context, req internal.ProvisionRequest) (*internal.ProvisionResult, error) {
	if err := b.validateProvision(req.WorkspaceSpec); err != nil {
		return nil, err
	}

	wait := req.WaitSeconds
	if wait <= 0 {
		wait = b.provisionWaitSeconds
	}

	// No backgrounding requested: preserve the original blocking behavior.
	if !req.Async && wait <= 0 {
		wsID := generateWorkspaceID()
		ws, err := b.runProvision(ctx, wsID, req.WorkspaceSpec, nil)
		if err != nil {
			return nil, err
		}
		return &internal.ProvisionResult{WorkspaceID: ws.ID, Workspace: ws, Status: internal.ProcessSucceeded}, nil
	}

	job, err := b.startProvisionJob(ctx, req.WorkspaceSpec, b.effectiveProvisionTimeout(req.TimeoutSeconds))
	if err != nil {
		return nil, err
	}
	if req.Async {
		logger.Info("provision_async", "workspace_id", job.workspaceID, "job_id", job.id)
		job.mu.Lock()
		status := job.status
		job.mu.Unlock()
		return &internal.ProvisionResult{WorkspaceID: job.workspaceID, JobID: job.id, Status: status}, nil
	}
	return b.awaitProvisionJob(ctx, job, wait)
}

// validateProvision performs the cheap, request-time gates that must fail
// fast — before a job is started — rather than surfacing inside the
// background pipeline.
func (b *Backend) validateProvision(spec internal.WorkspaceSpec) error {
	// Max-workspaces gate: refuse once the number of live workspaces hits
	// the configured ceiling. This applies to every provision — blank or
	// otherwise — so it runs before the blank-repo early return below.
	// Best-effort like the host-resource checks: it is sampled at the top of
	// Provision, before any work is done, so a burst of concurrent requests
	// can briefly overshoot by the number of in-flight provisions.
	if b.maxWorkspaces > 0 {
		b.mu.RLock()
		live := len(b.workspaces)
		b.mu.RUnlock()
		if live >= b.maxWorkspaces {
			logger.Warn("max_workspaces_reached", "live", live, "limit", b.maxWorkspaces)
			return fmt.Errorf("provisioning refused: %d live workspaces >= %d limit", live, b.maxWorkspaces)
		}
	}

	// Capacity gate: refuse before cloning or touching any runtime so a
	// host under pressure does not accumulate more work. This is the single
	// choke point every provisioner (container, jail, VM, directory) goes
	// through, so it must run before the blank-repo early return below —
	// otherwise a blank workspace bypasses every capacity threshold.
	if b.capacityChecker != nil {
		if err := b.capacityChecker.Check(); err != nil {
			logger.Warn("capacity_check_failed", "ref", spec.Ref, "error", err)
			return err
		}
	}

	// A blank repo means there is nothing to clone, so no forge auth is needed.
	if strings.TrimSpace(spec.RepoURL) == "" {
		return nil
	}
	// Public repositories can be pulled without forge auth; anything else
	// (including an SSH URL) needs forge authentication to clone.
	// We should be able to provision a blank workspace without any forge auth,
	// so only check the repo URL.
	if b.gitPushConfig.AuthMode == "" && spec.RepoURL != "" && !isPublicRepo(spec.RepoURL) {
		return fmt.Errorf("repository provisioning is unavailable: no forge authentication is configured")
	}

	return nil
}

// runProvision is the provisioning pipeline itself. wsID is chosen by the
// caller so a background job can report the eventual workspace ID before the
// pipeline finishes. progress, when non-nil, receives human-readable stage
// updates for a background job's log.
func (b *Backend) runProvision(ctx context.Context, wsID string, spec internal.WorkspaceSpec, progress func(string)) (*internal.Workspace, error) {
	emit := func(stage, format string, args ...interface{}) {
		msg := fmt.Sprintf(format, args...)
		logger.Info("provision_progress", "workspace_id", wsID, "stage", stage)
		if progress != nil {
			progress(msg)
		}
	}

	logger.Info("provision_start", "workspace_id", wsID, "repo_configured", spec.RepoURL != "", "ref", spec.Ref)
	emit("start", "provisioning workspace %s", wsID)

	// Clone repo to temp directory for detection
	tmpDir, err := os.MkdirTemp("", "wsd-repo-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Clone only when a repository was requested. An empty directory is a
	// supported blank workspace and is detected as such below.
	if strings.TrimSpace(spec.RepoURL) != "" {
		emit("clone", "cloning %s (ref %q)", spec.RepoURL, spec.Ref)
		if err := cloneRepo(ctx, tmpDir, spec.RepoURL, spec.Ref, b.gitPushConfig); err != nil {
			return nil, fmt.Errorf("failed to clone repo: %w", err)
		}
		emit("clone_complete", "clone complete")
	} else {
		emit("blank_workspace", "blank workspace requested (no repository)")
	}

	// Detect environment
	detector := detect.NewDetector()
	profile, err := detector.Detect(tmpDir)
	if err != nil {
		return nil, fmt.Errorf("environment detection failed: %w", err)
	}

	// Caller-supplied overrides win over detection. This is the escape hatch
	// for repos that cannot be auto-detected (or should not be): the caller
	// pins the image/language/install command explicitly.
	if len(spec.EnvOverrides) > 0 {
		profile = detect.ApplyOverrides(profile, spec.EnvOverrides)
		logger.Info("env_overrides_applied", "workspace_id", wsID, "language", profile.Language)
	}

	// Append administrator-authored hints last, so the agent sees the
	// environment-specific facts (escalation policy, preinstalled tools, ...)
	// that detection cannot know.
	if b.provisioningHints != "" && profile != nil {
		if profile.Notes != "" {
			profile.Notes += "\n"
		}
		profile.Notes += b.provisioningHints
		logger.Info("provisioning_hints_applied", "workspace_id", wsID)
	}

	// Compute cache key from manifests
	cacheKey, err := computeCacheKey(tmpDir, profile)
	if err != nil {
		logger.Warn("cache_key_computation_failed", "workspace_id", wsID, "error", err)
		cacheKey = ""
	}

	var cacheHit bool
	var cacheMount string

	if cacheKey != "" {
		cachePath, hit := b.cacheManager.Get(cacheKey)
		if hit {
			cacheHit = true
			cacheMount = cachePath
			logger.Info("cache_hit", "workspace_id", wsID, "cache_key", cacheKey)
		}
	}

	logger.Info("pulling_base_image", "image", profile.BaseImage)
	emit("provisioner_start", "detected environment %q; provisioning %s runtime", profile.Language, b.provisioner.Name())

	// Provision the runtime. This is lifecycle-only: create+start the
	// container and hand back a normalized handle. Everything after this
	// (copying the repo in, running install/post-create, cutting network
	// access) is generic orchestration done through the handle's Transport.
	repoPath := "/workspace/repo"
	handle, err := b.provisioner.Provision(ctx, runtime.ProvisionSpec{
		WorkspaceID:       wsID,
		Image:             profile.BaseImage,
		WorkDir:           repoPath,
		CacheMountPath:    cacheMount,
		WorkspaceBasePath: b.workspaceBasePath,
		Interface:         spec.Interface,
		IP4Address:        spec.IP4Address,
	})
	if err != nil {
		return nil, err
	}
	emit("runtime_ready", "runtime %s ready", handle.OS)

	// The base image only means something to container runtimes. A jail,
	// zone, VM, or process runtime populates its environment from its own
	// source (release, template, disk image, or the host OS) and ignores
	// Image, so blank it rather than leak a container image reference to the
	// agent (which misleads it into thinking it runs in that image).
	if profile != nil && handle.Isolation != runtime.IsolationContainer {
		profile.BaseImage = ""
	}

	// The runtime OS is only known once we've provisioned it. Fill in the
	// system-level package manager now so the profile handed back to the
	// agent distinguishes "no project package manager" (PackageManager
	// "none") from "no way to install anything" (which is never true).
	if profile != nil && profile.SystemPackageManager == "" {
		profile.SystemPackageManager, profile.SystemInstallCommand = systemPackageCommands(handle.OS, profile.BaseImage)
	}

	// Record this container in the state file now, before any of the
	// slower steps below run. If this process gets killed or aborted
	// partway through provisioning, the container still ends up tracked
	// as a leftover on the next startup instead of vanishing into
	// `docker ps -a` with no link back to why it exists.
	containerName := fmt.Sprintf("wsd-%s", wsID)
	if err := b.stateStore.Add(state.Entry{
		WorkspaceID:    wsID,
		ContainerID:    handle.NativeID,
		ContainerName:  containerName,
		SessionID:      b.sessionID,
		RepoURL:        spec.RepoURL,
		CreatedAt:      time.Now(),
		LastAccessedAt: time.Now(),
		WorkDir:        handle.WorkDir,
		OS:             string(handle.OS),
	}); err != nil {
		logger.Warn("state_store_add_failed", "workspace_id", wsID, "error", err)
	}

	// Copy repo into the runtime
	emit("repository_copy", "copying repository into %s", handle.WorkDir)
	if err := handle.Transport.CopyDirIn(ctx, tmpDir, handle.WorkDir); err != nil {
		// Rollback is cleanup: it must run even though the request ctx that
		// caused this failure is often already cancelled. If the destroy
		// fails, keep the state entry so the container stays discoverable as
		// a leftover instead of leaking untracked.
		destroyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		destroyErr := b.provisioner.Destroy(destroyCtx, handle)
		cancel()
		if destroyErr != nil {
			logger.Warn("provision_rollback_destroy_failed", "workspace_id", wsID, "error", destroyErr)
			return nil, fmt.Errorf("failed to copy repo: %w (rollback destroy also failed: %v)", err, destroyErr)
		}
		if rmErr := b.stateStore.Remove(wsID); rmErr != nil {
			logger.Warn("state_store_remove_failed", "workspace_id", wsID, "error", rmErr)
		}
		return nil, fmt.Errorf("failed to copy repo: %w", err)
	}

	// Give runtimes whose base environment lacks a package manager or common
	// tooling a chance to prepare themselves (e.g. a FreeBSD jail bootstraps
	// pkg and installs git). Runs while the network is still attached; a
	// failure is non-fatal.
	if bs, ok := handle.Transport.(runtime.Bootstrapper); ok {
		emit("runtime_bootstrap", "bootstrapping runtime")
		if err := bs.Bootstrap(ctx, handle.WorkDir); err != nil {
			logger.Warn("runtime_bootstrap_failed", "workspace_id", wsID, "error", err)
		} else {
			logger.Info("runtime_bootstrap_complete", "workspace_id", wsID)
		}
	}

	// Run install command if not cache hit
	if !cacheHit && profile.InstallCommand != "" {
		emit("dependency_install", "installing dependencies: %s", profile.InstallCommand)
		if _, err := handle.Transport.Exec(ctx, []string{"sh", "-c", profile.InstallCommand}, handle.WorkDir, nil); err != nil {
			logger.Warn("install_failed", "workspace_id", wsID, "error", err)
			// Continue anyway - agent can handle install failures
		} else {
			// Save to cache
			if cacheKey != "" {
				if err := b.saveCache(cacheKey, profile); err != nil {
					logger.Warn("cache_save_failed", "workspace_id", wsID, "error", err)
				}
			}
			logger.Info("install_complete", "workspace_id", wsID)
		}
	}

	// Run post-create command if specified
	if profile.PostCreateCommand != "" {
		emit("post_create", "running post-create: %s", profile.PostCreateCommand)
		if _, err := handle.Transport.Exec(ctx, []string{"sh", "-c", profile.PostCreateCommand}, handle.WorkDir, nil); err != nil {
			logger.Warn("post_create_failed", "workspace_id", wsID, "error", err)
		}
	}

	// Provisioning (clone was done on the host; install/post-create ran
	// above with network attached) is complete, so cut network access now.
	// From this point on the workspace is network-off by default; Exec()
	// reattaches only for the duration of a call that explicitly sets
	// AllowNetwork.
	detachNetwork(ctx, handle.Transport, wsID)

	ws := &internal.Workspace{
		ID:        wsID,
		Spec:      spec,
		Profile:   profile,
		CacheHit:  cacheHit,
		OS:        string(handle.OS),
		WorkDir:   handle.WorkDir,
		CreatedAt: time.Now(),
	}

	b.mu.Lock()
	b.workspaces[wsID] = &workspaceState{
		workspace: ws,
		handle:    handle,
		secrets:   make(map[string]string),
	}
	b.mu.Unlock()

	logger.Info("provision_complete", "workspace_id", wsID, "cache_hit", cacheHit)
	return ws, nil
}
