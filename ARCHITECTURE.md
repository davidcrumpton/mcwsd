# Architecture

This document is the contract mcwsd's plugin-oriented design is built on. It
exists so later phases (FreeBSD jails, a runtime router, VMs, additional
git forges, ...) can be implemented by extending these five subsystems
without changing the code above them. 

## The five subsystems

### 1. Provisioner (`internal/runtime`, implementations in `internal/backend/*`)

Creates and destroys execution environments. Responsible for lifecycle
only — it knows nothing about git, dependency installation, or caching
policy.

```go
type Provisioner interface {
    Name() string
    Init(ctx context.Context) error
    Provision(ctx context.Context, spec ProvisionSpec) (*RuntimeHandle, error)
    Destroy(ctx context.Context, handle *RuntimeHandle) error
    DestroyByID(ctx context.Context, id string) error
}
```

Implementations: `docker.DockerProvisioner` (Docker Engine API),
`podman.PodmanProvisioner` (Podman CLI), `freebsd.FreeBSDJailProvisioner`
(FreeBSD jails), `zones.ZonesProvisioner` (OpenIndiana/illumos zones),
`kubernetes.KubernetesProvisioner` (Kubernetes pods), plus goja-backed script
plugins (`script.Provisioner`) for `podman`, `docker`, `kubernetes`, and
`openbsd-vmd`.

Optional interfaces a Provisioner may implement:
- `Validator` — host/runtime configuration validation
- `OrphanCleaner` — cleanup of untracked resources
- `WorkspaceRecoverer` — recover workspace handles after daemon restart
- `WorkspaceIDDestroyer` — destroy by workspace ID without a full handle

### 2. Execution Transport (`internal/runtime`, implementations alongside each Provisioner)

Executes commands and moves files into/out of an already-provisioned
runtime, independent of how that runtime was created.

```go
type ExecTransport interface {
    Exec(ctx context.Context, cmd []string, workDir string, env []string) (*internal.ExecResult, error)
    SetNetwork(ctx context.Context, enabled bool) error
    CopyDirIn(ctx context.Context, hostSrcDir string, destPath string) error
    ReadFile(ctx context.Context, path string) ([]byte, error)
    WriteFile(ctx context.Context, path string, content []byte, mode int64) error
}
```

Implementations: `docker.DockerExecTransport` (Docker Engine API),
`podman.PodmanExecTransport` (Podman CLI), `freebsd.JexecTransport` (jexec),
`zones.ZonesExecTransport` (zlogin),
`kubernetes.KubectlExecTransport` (kubectl exec), and the script
`ExecTransport` (ssh(1) for the openbsd-vmd plugin).

Optional interfaces an ExecTransport may implement:
- `Bootstrapper` — one-time base setup inside a freshly provisioned runtime

A `RuntimeHandle` carries exactly one `ExecTransport`. Every workspace
backend interacts with a runtime only through the handle it got back from
`Provision` — never through a provisioner-specific type.

### 3. Forge (`internal/forge`, implementations in `internal/forge/*`)

Opens merge/pull requests against a git hosting provider's REST API. This
is the *only* hosting-provider-specific step — the generic git plumbing
(clone, fetch a bundle, push) lives in `internal/gitpush` and is identical
regardless of which forge is in play.

```go
type Forge interface {
    Name() string
    OpenMergeRequest(ctx context.Context, req OpenMRRequest) (*OpenMRResult, error)
}
```

Implementations: `gitlab.Forge`, `github.Forge`, `gitea.Forge`, `forgejo.Forge`.

### 4. Profile (`internal.EnvironmentProfile`, detection in `internal/detect`)

Detects and describes a repo's development environment — language,
version, package manager, install command, base image reference, optional
post-create command, and system package manager. A profile is a description,
not a decision: it never picks a runtime or provisioner. `detect.Detector`
already conforms to this and needed no changes for Phase 1.

The detector uses priority-based detection with the following order:
1. `.devcontainer/devcontainer.json`
2. `go.mod` (Go)
3. `Cargo.toml` (Rust)
4. `pyproject.toml` (Python)
5. `requirements.txt` (Python pip)
6. `package.json` (Node.js)
7. Unknown/blank workspace

Future: additional detectors for more languages; the profile shape
itself is expected to stay stable.

### 5. RuntimeHandle (`internal/runtime`)

The normalized object every Provisioner returns.

```go
type RuntimeHandle struct {
    OS        OS
    Isolation IsolationType
    Username  string
    Privilege PrivilegeMode
    WorkDir   string
    NativeID  string // provisioner-specific resource id (container ID, jail name, VM id, ...), for logging/tracking only
    Transport ExecTransport
}
```

**OS** identifies the runtime's operating system:
- `OSLinux`, `OSFreeBSD`, `OSOpenBSD`, `OSNetBSD`, `OSMinix`, `OSDarwin`,
  `OSIllumos`, `OSOpenIndiana`

**IsolationType** identifies the isolation model:
- `IsolationContainer` (Docker, Podman)
- `IsolationJail` (FreeBSD jails)
- `IsolationZone` (OpenIndiana/illumos zones)
- `IsolationVM` (OpenBSD VMD, Kubernetes pods)
- `IsolationProcess` (process-based, future)

**PrivilegeMode** identifies the privilege level:
- `PrivilegeRoot`
- `PrivilegeUser`

Only the fields a given Provisioner can meaningfully populate are set;
callers must treat an unset field as "unspecified", not an error. Each
provisioner populates the fields appropriate to its runtime type.

## Where orchestration lives

Everything that isn't one of the five subsystems above — cloning the repo,
computing a cache key, deciding whether to run the install command, git
identity/commit/bundle steps, secret grant/revoke, state-file bookkeeping —
is orchestration. It lives in the workspace backend (`backend.Backend`)
and is written entirely against the `Provisioner` / `ExecTransport`
/ `Forge` interfaces, not against any runtime-specific types. The backend
holds a single `runtime.Provisioner` (Docker, Podman, FreeBSD jail,
OpenIndiana/illumos zones, Kubernetes, or a script plugin such as openbsd-vmd)
and orchestrates it identically
regardless of which one is in play.

## ProvisionSpec (`internal/runtime`)

The input passed to every Provisioner's `Provision` method:

```go
type ProvisionSpec struct {
    WorkspaceID       string
    Image             string
    WorkDir           string
    CacheMountPath    string
    WorkspaceBasePath string
    Interface         string   // for runtimes requiring explicit network config (e.g., FreeBSD jails, illumos zones)
    IP4Address        string   // for runtimes requiring explicit network config (e.g., FreeBSD jails, illumos zones)
}
```

`Interface` and `IP4Address` are only meaningful to provisioners whose
isolation model requires explicit network configuration (currently the
FreeBSD jail provisioner). Container provisioners (Docker, Podman, Kubernetes)
ignore these fields.

## Rules for extending this

* No provisioner/transport-specific logic outside that provisioner's own
  package (e.g. no raw Docker API calls outside `DockerProvisioner` /
  `DockerExecTransport`).
* A new runtime = implement `Provisioner` + `ExecTransport` once. Nothing
  above `internal/runtime` should need to change.
* A new forge = implement `Forge` once. Nothing in `internal/gitpush`
  should need to change beyond picking which `Forge` to construct.
* `RuntimeHandle` fields are additive — add a field for a new runtime's
  needs rather than overloading an existing one's meaning.
* Optional interfaces (Validator, OrphanCleaner, WorkspaceRecoverer,
  WorkspaceIDDestroyer, Bootstrapper) allow provisioners and transports to
  opt into additional capabilities without breaking the core contracts.

## Known pre-existing quirks

None known at this time.

## Runtime plugins (`internal/script`, `plugins/`, `plugin-devel/`)

The five subsystems above are being complemented by a scriptable runtime layer:
a single goja-backed JavaScript plugin (authored in TypeScript) can supply both
the `Provisioner` lifecycle and the `ExecTransport` for a runtime, behind the
same `runtime.Provisioner` / `runtime.ExecTransport` interfaces. Core owns the
generic file I/O (ReadFile/WriteFile/CopyDirIn) on top of the plugin's pure
`execArgs` function, and owns IPAM, options, and the undo stack.

One plugin backs one daemon instance (`mcwsd daemon --plugin <path>`); a plugin
declares its options (surfaced as `--help` flags) and the host binaries it may
run. See [PLUGINS.md](../PLUGINS.md) for the contract and the single-plugin
policy.
