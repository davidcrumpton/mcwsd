package internal

import (
	"context"
	"time"
)

// WorkspaceSpec describes what to provision.
type WorkspaceSpec struct {
	RepoURL      string            `json:"repo_url"`
	Ref          string            `json:"ref,omitempty"`
	EnvOverrides map[string]string `json:"env_overrides,omitempty"`

	// Interface and IP4Address are only meaningful for runtimes that need
	// explicit network configuration (FreeBSD jails). Container runtimes
	// ignore them, so they are optional and never required by Docker or
	// Podman.
	Interface  string `json:"interface,omitempty"`
	IP4Address string `json:"ip4_address,omitempty"`
}

// ProvisionRequest asks the backend to create a workspace. It embeds
// WorkspaceSpec and adds the backgrounding controls that mirror ExecRequest:
// the whole provisioning pipeline (host-side clone, environment detection,
// runtime boot, repo copy, install and post-create) can run as a background
// job whose job_id the caller polls, so a slow VM boot or a large clone can no
// longer trip the client's HTTP timeout.
type ProvisionRequest struct {
	WorkspaceSpec

	// WaitSeconds is how long Provision blocks before promoting the whole
	// provisioning pipeline to a background job and returning its job_id
	// instead of the finished workspace. 0 uses the daemon default
	// (--provision-wait-seconds, itself 0 = never promote).
	WaitSeconds int `json:"wait_seconds,omitempty"`

	// Async returns a job_id immediately without waiting. It is an explicit
	// background start.
	Async bool `json:"async,omitempty"`

	// TimeoutSeconds, when > 0, bounds the whole provisioning pipeline and
	// marks it timed_out if exceeded. 0 uses the daemon default
	// (--provision-timeout, itself 0 = no daemon-level cap).
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// ProvisionResult is the result of a Provision call. Exactly one shape is
// returned: either the finished Workspace, or — when provisioning was
// backgrounded — a JobID/Status the caller polls via the job API. When a
// provision job succeeds, GetProcessStatus carries the finished Workspace.
type ProvisionResult struct {
	// WorkspaceID is always set. For a backgrounded provision it names the
	// workspace that will exist once the job succeeds.
	WorkspaceID string `json:"workspace_id"`

	// Workspace is set when provisioning completed within the call.
	Workspace *Workspace `json:"workspace,omitempty"`

	// JobID is set when provisioning was promoted to a background job; the
	// caller polls GetProcessStatus (the job API) with it. Status is that
	// job's state at the moment the response was produced.
	JobID  string        `json:"job_id,omitempty"`
	Status ProcessStatus `json:"status,omitempty"`
}

// Workspace represents a provisioned ephemeral environment.
type Workspace struct {
	ID       string              `json:"id"`
	Spec     WorkspaceSpec       `json:"spec"`
	Profile  *EnvironmentProfile `json:"profile"`
	CacheHit bool                `json:"cache_hit"`

	// OS is the operating system of the provisioned runtime (e.g. "linux",
	// "freebsd", "openbsd", "minix"). It tells the agent which package
	// manager and binary formats to use when installing tools. It is a
	// plain string rather than runtime.OS to avoid an import cycle
	// (runtime imports this package).
	OS string `json:"os"`

	// WorkDir is the absolute path of the repo inside the runtime (e.g.
	// "/workspace/repo"). Agents should use it as the base for every file
	// path, and it is echoed back so they never have to guess.
	WorkDir string `json:"work_dir"`

	CreatedAt time.Time `json:"created_at"`
}

// ExecRequest is a command to run inside a workspace.
type ExecRequest struct {
	Command        string   `json:"command"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	Secrets        []string `json:"secrets,omitempty"` // secret refs, not values
	AllowNetwork   bool     `json:"allow_network,omitempty"`

	// WaitSeconds is how long Exec blocks before promoting the command to a
	// background job and returning its job_id instead of output. 0 uses the
	// daemon default (--exec-wait-seconds, itself 0 = never promote).
	WaitSeconds int `json:"wait_seconds,omitempty"`

	// Async returns a job_id immediately without waiting. It is an explicit
	// background start and counts against the background job limits.
	Async bool `json:"async,omitempty"`
}

// ExecResult is the result of an exec call. Exactly one shape is returned:
// either inline output (Stdout/Stderr/ExitCode) or, when the command was
// promoted to a background job, a JobID/Status the caller polls.
type ExecResult struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`

	// JobID is set when the command became a background job; the caller polls
	// GetProcessStatus (the job API) with it. Status is that job's state at the
	// moment the response was produced.
	JobID  string `json:"job_id,omitempty"`
	Status string `json:"status,omitempty"`
}

// Artifact represents a file or patch returned from Artifacts().
type Artifact struct {
	Name    string `json:"name"`
	Content []byte `json:"content"`
	Type    string `json:"type"` // "patch" or "file"
}

// EnvironmentProfile captures detected environment info.
type EnvironmentProfile struct {
	Language          string `json:"language"`
	Version           string `json:"version"`
	PackageManager    string `json:"package_manager"`
	InstallCommand    string `json:"install_command"`
	BaseImage         string `json:"base_image"`
	PostCreateCommand string `json:"post_create_command,omitempty"`

	// SystemPackageManager is the package manager of the runtime OS itself,
	// used to install system-level tools (bash, gcc, make, ...) as opposed
	// to the project's own dependencies. It is deliberately separate from
	// PackageManager: a repo whose language was not detected reports
	// PackageManager "none", but the runtime still has a working system
	// package manager ("pkg" on FreeBSD, "apt" on Debian/Ubuntu, "apk" on
	// Alpine). Agents must consult this before concluding no package
	// manager exists. Empty if the runtime OS is unknown.
	SystemPackageManager string `json:"system_package_manager,omitempty"`

	// SystemInstallCommand is the command prefix that installs one or more
	// system packages, e.g. "pkg install -y" or "apt-get install -y".
	// Install a tool by appending its package name(s).
	SystemInstallCommand string `json:"system_install_command,omitempty"`

	// Notes carries agent-readable guidance about the profile, e.g. why
	// detection fell back to "unknown" and what to do about it.
	Notes string `json:"notes,omitempty"`
}

// CommitAndPRRequest asks the daemon to commit whatever is in the
// workspace's working tree, push it to a server-chosen branch, and open a
// merge request. The caller does NOT get to choose the branch name — the
// backend generates it — so an agent can never target an existing branch
// (including main) or collide with another workspace's branch. The forge is
// likewise a daemon-level setting, not a request field.
type CommitAndPRRequest struct {
	CommitMessage     string `json:"commit_message"`
	PRTitle           string `json:"pr_title"`
	PRDescription     string `json:"pr_description,omitempty"`
	RepoURL           string `json:"repo_url,omitempty"`
	ProjectPath       string `json:"project_path,omitempty"`
	GitLabProjectPath string `json:"gitlab_project_path,omitempty"`
}

// CommitAndPRResult is returned on success.
type CommitAndPRResult struct {
	Branch string `json:"branch"`
	MRURL  string `json:"mr_url"`
	Error  string `json:"error,omitempty"`

	// NoChangesDetail carries a factual snapshot of the working tree (git
	// status --porcelain --ignored output) when there was nothing to commit.
	// It exists so the caller can see *why* nothing was staged — most commonly
	// because a newly created file is excluded by .gitignore — and decide how
	// to proceed, rather than the daemon forcing a choice (e.g. force-adding
	// ignored files) on them.
	NoChangesDetail string `json:"no_changes_detail,omitempty"`
}

// FileEntry describes a single entry returned by ListFiles.
type FileEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// ListFilesResult is the result of a ListFiles call.
type ListFilesResult struct {
	Entries []FileEntry `json:"entries"`
}

// ReadFileResult is the result of a ReadFile call.
type ReadFileResult struct {
	Content string `json:"content"`
}

// ProcessStatus summarises the state of a tracked background process (a
// "job" in the AI-facing vocabulary).
type ProcessStatus string

const (
	// ProcessQueued means the job was accepted but has not started running yet.
	ProcessQueued ProcessStatus = "queued"
	// ProcessRunning means the job is currently executing.
	ProcessRunning ProcessStatus = "running"
	// ProcessSucceeded means the job exited with status 0.
	ProcessSucceeded ProcessStatus = "succeeded"
	// ProcessFailed means the job exited non-zero, died unexpectedly, or
	// never started.
	ProcessFailed ProcessStatus = "failed"
	// ProcessCancelled means the job was explicitly killed by a caller.
	ProcessCancelled ProcessStatus = "cancelled"
	// ProcessTimedOut means the job exceeded its deadline and was killed.
	ProcessTimedOut ProcessStatus = "timed_out"
)

// ProcessInfo describes a tracked background process.
type ProcessInfo struct {
	ID       string        `json:"id"`
	Command  string        `json:"command"`
	Status   ProcessStatus `json:"status"`
	ExitCode int           `json:"exit_code,omitempty"`
}

// ProcessStatusResult is the result of a GetProcessStatus call.
type ProcessStatusResult struct {
	ProcessInfo

	// Stdout and Stderr carry the output produced since the caller's
	// cursor. When no cursor is supplied they carry all output so far,
	// preserving the pre-cursor behaviour for existing clients.
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`

	// Output is the same output combined into timestamped JSON-lines
	// records ({"ts",...,"stream":...,"line":...}), the format the
	// AI-facing tools read.
	Output string `json:"output,omitempty"`

	// Cursor is an opaque token to pass back on the next status call to
	// receive only output added after this one. The value "done" means the
	// terminal record has already been delivered.
	Cursor string `json:"cursor,omitempty"`

	// Workspace is set on the terminal status of a background provisioning
	// job: it carries the provisioned workspace's details (OS, work_dir,
	// profile) once the pipeline succeeds.
	Workspace *Workspace `json:"workspace,omitempty"`
}

// StartProcessRequest asks the backend to launch a long-running command
// in the background. The caller polls GetProcessStatus for output.
type StartProcessRequest struct {
	Command      string `json:"command"`
	AllowNetwork bool   `json:"allow_network,omitempty"`

	// TimeoutSeconds, when > 0, kills the job after this many seconds and
	// marks it timed_out.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// WorkspaceBackend interface for provisioning and managing workspaces.
type WorkspaceBackend interface {
	Provision(ctx context.Context, req ProvisionRequest) (*ProvisionResult, error)
	Exec(ctx context.Context, ws *Workspace, cmd ExecRequest) (*ExecResult, error)
	Artifacts(ctx context.Context, ws *Workspace, paths []string) ([]Artifact, error)
	Destroy(ctx context.Context, ws *Workspace) error

	// CommitAndPR commits the workspace's current working tree changes
	// locally inside the container (no network, no credentials), bundles
	// the resulting commit, and hands the bundle to the host-side push
	// path. The container itself never sees a credential and never
	// contacts the remote.
	CommitAndPR(ctx context.Context, ws *Workspace, req CommitAndPRRequest) (*CommitAndPRResult, error)

	SafeEdit(ctx context.Context, ws *Workspace, filePath string, pattern string, replacement string) error

	// File system tools

	// ListFiles returns a structured listing of files and directories at
	// the given path inside the workspace.
	ListFiles(ctx context.Context, ws *Workspace, path string) (*ListFilesResult, error)

	// ReadFile reads the contents of a specific file inside the workspace.
	ReadFile(ctx context.Context, ws *Workspace, path string) (*ReadFileResult, error)

	// WriteFile writes text content to a file inside the workspace,
	// creating or overwriting it.
	WriteFile(ctx context.Context, ws *Workspace, path string, content string) error

	// Background process management tools

	// StartProcess launches a command inside the workspace as a tracked
	// background process. Returns the process ID for subsequent calls.
	StartProcess(ctx context.Context, ws *Workspace, req StartProcessRequest) (*ProcessInfo, error)

	// ListProcesses returns all tracked background processes for a workspace.
	ListProcesses(ctx context.Context, ws *Workspace) ([]ProcessInfo, error)

	// GetProcessStatus returns the current status and the output produced
	// since cursor. An empty cursor means "from the beginning"; the result
	// carries the cursor to pass on the next call. The record and its
	// output remain queryable after the job reaches a terminal state.
	GetProcessStatus(ctx context.Context, ws *Workspace, processID string, cursor string) (*ProcessStatusResult, error)

	// KillProcess terminates a background process (its whole process group)
	// and marks it cancelled. The record and its output remain queryable
	// afterwards.
	KillProcess(ctx context.Context, ws *Workspace, processID string) error
}

// EnvironmentDetector interface for detecting project environment.
type EnvironmentDetector interface {
	Detect(repoPath string) (*EnvironmentProfile, error)
}

// SecretBroker interface for managing short-lived secrets.
type SecretBroker interface {
	// Grant writes the secret into the running process's environment for a single exec call.
	Grant(ctx context.Context, workspaceID string, secretRef string, ttl time.Duration) error
	Revoke(ctx context.Context, workspaceID string, secretRef string) error
}
