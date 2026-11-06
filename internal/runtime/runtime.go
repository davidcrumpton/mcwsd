// Package runtime defines the normalized abstractions that let the daemon
// support more than one execution environment (Docker, Podman, FreeBSD jails,
// OpenBSD VMD, Kubernetes) without callers needing to know which one is in play.
//
// A Provisioner creates and destroys the underlying environment and hands
// back a RuntimeHandle. An ExecTransport (carried on that handle) is how
// callers run commands and move files in and out, independent of how the
// environment was created. Adding a new runtime means implementing both
// interfaces once — nothing above this package needs to change.
package runtime

import (
	"context"

	"github.com/davidcrumpton/mcwsd/internal"
)

// OS identifies the operating system a RuntimeHandle is running.
type OS string

const (
	OSLinux       OS = "linux"
	OSFreeBSD     OS = "freebsd"
	OSOpenBSD     OS = "openbsd"
	OSNetBSD      OS = "netbsd"
	OSMinix       OS = "minix"
	OSDarwin      OS = "darwin"
	OSIllumos     OS = "illumos"
	OSOpenIndiana OS = "openindiana"
)

// IsolationType identifies the kind of isolation a RuntimeHandle provides.
type IsolationType string

const (
	IsolationContainer IsolationType = "container"
	IsolationJail      IsolationType = "jail"
	IsolationZone      IsolationType = "zone"
	IsolationVM        IsolationType = "vm"
	IsolationProcess   IsolationType = "process"
)

// PrivilegeMode identifies which privilege level commands run under inside
// a RuntimeHandle.
type PrivilegeMode string

const (
	PrivilegeRoot PrivilegeMode = "root"
	PrivilegeUser PrivilegeMode = "user"
)

// RuntimeHandle is the normalized result of provisioning a workspace's
// execution environment. Every Provisioner returns one, regardless of what
// it provisioned underneath. Callers (the workspace backend) only ever
// interact with a runtime through this handle and its Transport — never
// through provisioner-specific types.
//
// Only the fields a given Provisioner can meaningfully populate are set;
// callers should treat an unset field as "unspecified", not an error.
type RuntimeHandle struct {
	OS        OS
	Isolation IsolationType
	Username  string
	Privilege PrivilegeMode

	// WorkDir is the working directory inside the runtime where the
	// workspace's repo lives.
	WorkDir string

	// NativeID is the provisioner-specific identifier for the underlying
	// resource (a container ID, jail name, VM ID, ...). It exists purely
	// for logging and out-of-process tracking (see internal/state) and is
	// never interpreted by anything outside the owning Provisioner.
	NativeID string

	// Transport is how callers execute commands and move files in/out of
	// this runtime. Never nil on a successfully provisioned handle.
	Transport ExecTransport
}

// ProvisionSpec describes the environment a Provisioner should create. It
// intentionally knows nothing about git, caching policy, or how the image
// reference should be interpreted — that orchestration happens above this
// interface.
type ProvisionSpec struct {
	// WorkspaceID names the workspace being provisioned; provisioners use
	// it to name/tag the underlying resource.
	WorkspaceID string

	// Image is a reference to the base image or template to provision
	// from, e.g. a Docker image tag. Its meaning is provisioner-specific.
	Image string

	// WorkDir is the working directory the provisioned runtime should
	// expose for the workspace's repo (mirrored onto the returned handle).
	WorkDir string

	// CacheMountPath, if non-empty, is a host path that should be made
	// available inside the runtime for dependency caching.
	CacheMountPath string

	// WorkspaceBasePath, if non-empty, is a host path that should be mounted
	// as the base directory for the workspace's repo.
	WorkspaceBasePath string

	// Interface and IP4Address are runtime-specific networking settings.
	// They are only meaningful to provisioners whose isolation model
	// requires explicit network configuration (currently the FreeBSD jail
	// provisioner, which needs a host interface and a jail IP). Container
	// provisioners ignore them.
	Interface  string
	IP4Address string
}

// Provisioner creates and destroys execution environments. It is
// responsible only for lifecycle — it knows nothing about git, dependency
// installation, or caching policy; that orchestration happens above this
// interface (see internal/backend/docker.Backend for the Docker case).
//
// Extension point: implement this interface for a new runtime (FreeBSD
// jail, VM, ...) and it can be swapped in without changing the workspace
// backend that calls it.
type Provisioner interface {
	// Name identifies this provisioner, e.g. "docker".
	Name() string

	// Init performs one-time host/runtime setup before the daemon accepts work.
	// It must be safe to call on every daemon start.
	Init(ctx context.Context) error

	// Provision creates a new execution environment and returns a
	// normalized handle to it. The returned handle's Transport is ready to
	// use immediately.
	Provision(ctx context.Context, spec ProvisionSpec) (*RuntimeHandle, error)

	// Destroy tears down the environment behind handle.
	Destroy(ctx context.Context, handle *RuntimeHandle) error

	// DestroyByID force-removes the environment identified by its native ID,
	// without needing a full RuntimeHandle.
	DestroyByID(ctx context.Context, id string) error
}

// WorkspaceIDDestroyer is an optional capability for provisioners whose
// native resource ID can be derived from the public workspace ID. The backend
// uses it only when the workspace is absent from both memory and the state
// file, so an untracked runtime resource can still be explicitly destroyed.
type WorkspaceIDDestroyer interface {
	DestroyWorkspaceByID(ctx context.Context, workspaceID string) error
}

// Validator is an optional interface a Provisioner may implement to check
// that the host is actually configured for it — the hypervisor or runtime is
// present, the required images/keys exist, and the daemon has the privilege
// it needs. The daemon calls Validate once at startup and refuses to serve if
// it fails, so a misconfigured host surfaces immediately instead of wasting
// an agent's first provision.
type Validator interface {
	// Validate returns a non-nil error describing the first problem found.
	Validate(ctx context.Context) error
}

// OrphanCleaner is an optional interface a Provisioner may implement to
// remove resources it created that are no longer tracked in the state file.
// This is needed for runtimes where creating the backing resource (e.g. a VM
// disk image) and starting it are separate steps: a crash in between leaves
// an untracked artifact behind. keep holds the native IDs the daemon still
// considers live; anything else under the provisioner's managed root is fair
// game to remove. Returns the number of resources removed.
type OrphanCleaner interface {
	CleanupOrphans(ctx context.Context, keep map[string]bool) (int, error)
}

// WorkspaceRecoverer is an optional interface a Provisioner may implement to
// support recovering workspaces after a daemon restart. It allows the backend
// to create a RuntimeHandle for an existing container/workspace identified by
// its native ID and work directory.
type WorkspaceRecoverer interface {
	// RecoverHandle creates a RuntimeHandle for an existing workspace.
	// nativeID is the provisioner-specific container/workspace identifier.
	// workDir is the working directory inside the workspace.
	// Returns an error if the workspace does not exist.
	RecoverHandle(ctx context.Context, nativeID string, workDir string) (*RuntimeHandle, error)
}

// Bootstrapper is an optional interface an ExecTransport may implement to
// perform one-time base setup inside a freshly provisioned runtime, before
// any project install commands run. It exists for runtimes whose base
// environment lacks a package manager or common tooling — for example a
// FreeBSD jail populated from base.txz ships neither pkg(8) nor git(1).
//
// The backend calls Bootstrap once per workspace while network access is
// still attached, and treats a failure as non-fatal (it logs and continues),
// so an unreachable package mirror cannot block a workspace that may not
// need one.
type Bootstrapper interface {
	// Bootstrap prepares the runtime's base environment. workDir is the
	// workspace's repo directory (may be used as the working directory).
	Bootstrap(ctx context.Context, workDir string) error
}

// ExecTransport executes commands and moves files into/out of a
// provisioned runtime, independent of how that runtime was created.
//
// Extension point: future transports (SSH, jexec, local process) implement
// this same interface; the workspace backend never needs to know which one
// it's holding.
type ExecTransport interface {
	// Exec runs cmd inside the runtime, in workDir, with the given extra
	// environment variables (may be nil).
	Exec(ctx context.Context, cmd []string, workDir string, env []string) (*internal.ExecResult, error)

	// SetNetwork enables or disables network access for the runtime. Exec
	// calls that need network access toggle this for their duration.
	SetNetwork(ctx context.Context, enabled bool) error

	// CopyDirIn recursively copies hostSrcDir into the runtime at destPath.
	CopyDirIn(ctx context.Context, hostSrcDir string, destPath string) error

	// ReadFile reads a single file's contents out of the runtime.
	ReadFile(ctx context.Context, path string) ([]byte, error)

	// WriteFile writes content to a file inside the runtime, creating or
	// overwriting it with the given mode.
	WriteFile(ctx context.Context, path string, content []byte, mode int64) error
}
