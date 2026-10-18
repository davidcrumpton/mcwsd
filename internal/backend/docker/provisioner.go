package docker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
)

// debugLogPrefix is an idea from a different generation and is not needed anymore
// We'll clean it out fully in a future pass.
// prefix used by the FreeBSD and OpenBSD provisioners.
const debugLogPrefix = ""

// debugf logs a debug-level message with the standard [DEBUG] shape.
func debugf(format string, args ...interface{}) {
	internal.Log.Debug(debugLogPrefix + fmt.Sprintf(format, args...))
}

// describeMounts renders mount sources and targets for a debug/error message so
// a failing ContainerCreate names the exact binds it tried to use.
func describeMounts(mounts []mount.Mount) string {
	parts := make([]string, 0, len(mounts))
	for _, m := range mounts {
		parts = append(parts, fmt.Sprintf("%s:%s", m.Source, m.Target))
	}
	return strings.Join(parts, ",")
}

// pathExists reports whether path exists on disk.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// DockerProvisioner implements runtime.Provisioner using the Docker Engine
// API. It is the only place in this package that creates, starts, or
// removes containers — running commands or moving files in/out of an
// already-running container is DockerExecTransport's job instead (see
// transport.go).
type DockerProvisioner struct {
	dockerClient *client.Client
	networkName  string
}

// NewDockerProvisioner creates a DockerProvisioner. networkName is the
// Docker network newly created containers attach to during provisioning
// and reattach to briefly when an exec call requests network access (see
// DockerExecTransport.SetNetwork).
func NewDockerProvisioner(cli *client.Client, networkName string) *DockerProvisioner {
	return &DockerProvisioner{dockerClient: cli, networkName: networkName}
}

var _ runtime.Provisioner = (*DockerProvisioner)(nil)

// Name identifies this provisioner.
func (p *DockerProvisioner) Name() string { return "docker" }

func (p *DockerProvisioner) Init(context.Context) error { return nil }

// Provision pulls spec.Image, creates and starts a container from it, and
// returns a handle whose Transport is bound to that container. It does not
// copy any files in or run any commands — Provision is lifecycle-only, the
// caller does the rest via the returned handle's Transport.
func (p *DockerProvisioner) Provision(ctx context.Context, spec runtime.ProvisionSpec) (*runtime.RuntimeHandle, error) {
	containerName := fmt.Sprintf("wsd-%s", spec.WorkspaceID)

	debugf("provisioning docker container %s: image=%s workDir=%s cacheMountPath=%s workspaceBasePath=%s",
		containerName, spec.Image, spec.WorkDir, spec.CacheMountPath, spec.WorkspaceBasePath)

	if err := p.pullImage(ctx, spec.Image); err != nil {
		debugf("failed to pull image %s: %v", spec.Image, err)
		return nil, fmt.Errorf("failed to pull image %s: %w", spec.Image, err)
	}
	debugf("image %s is available locally", spec.Image)

	// No NetworkMode override here: the container gets Docker's normal
	// default network attachment so provisioning (npm ci, pip install,
	// etc.) can actually reach the network. The caller disconnects it via
	// SetNetwork(false) once provisioning finishes, so the workspace is
	// network-off by default for exec() calls, matching the daemon's
	// stated policy.
	hostConfig := &container.HostConfig{}

	if spec.CacheMountPath != "" {
		// Docker refuses a bind whose source does not exist, so surface the
		// missing path in the logs before the daemon reports its opaque
		// "bind source path does not exist" error.
		if !pathExists(spec.CacheMountPath) {
			debugf("WARNING: cache mount source %s does not exist on the host; docker will reject the bind mount", spec.CacheMountPath)
		}
		debugf("adding cache bind mount %s -> /cache", spec.CacheMountPath)
		hostConfig.Mounts = append(hostConfig.Mounts, mount.Mount{
			Type:   mount.TypeBind,
			Source: spec.CacheMountPath,
			Target: "/cache",
		})
	}
	if spec.WorkspaceBasePath != "" {
		// Mount a per-workspace host directory rather than the shared base
		// path root, mirroring the podman and FreeBSD provisioners: a single
		// shared directory would let successive workspaces clobber each
		// other's checkout. Docker requires the bind source to already
		// exist, so it is created here — this is the step whose absence made
		// an uncreated --workspace-base-path fail deep inside the daemon.
		hostDir := filepath.Join(spec.WorkspaceBasePath, spec.WorkspaceID)
		debugf("ensuring workspace host dir %s", hostDir)
		if err := os.MkdirAll(hostDir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create workspace dir %s: %w", hostDir, err)
		}
		debugf("adding workspace bind mount %s -> %s", hostDir, spec.WorkDir)
		hostConfig.Mounts = append(hostConfig.Mounts, mount.Mount{
			Type:   mount.TypeBind,
			Source: hostDir,
			Target: spec.WorkDir,
		})
	}

	debugf("creating container %s: image=%s workDir=%s mounts=[%s]",
		containerName, spec.Image, spec.WorkDir, describeMounts(hostConfig.Mounts))
	resp, err := p.dockerClient.ContainerCreate(ctx, &container.Config{
		Image:        spec.Image,
		Cmd:          []string{"sleep", "infinity"},
		WorkingDir:   spec.WorkDir,
		Tty:          false,
		AttachStdout: true,
		AttachStderr: true,
	}, hostConfig, nil, nil, containerName)
	if err != nil {
		debugf("container create failed for %s (image=%s workDir=%s mounts=[%s]): %v",
			containerName, spec.Image, spec.WorkDir, describeMounts(hostConfig.Mounts), err)
		return nil, fmt.Errorf("failed to create container %s (image=%s workDir=%s mounts=[%s]): %w",
			containerName, spec.Image, spec.WorkDir, describeMounts(hostConfig.Mounts), err)
	}
	debugf("container %s created with id %s", containerName, resp.ID)

	if err := p.dockerClient.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		debugf("container start failed for %s (id=%s): %v; removing partially-created container", containerName, resp.ID, err)
		p.dockerClient.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("failed to start container %s (id=%s): %w", containerName, resp.ID, err)
	}
	debugf("container %s started (id=%s)", containerName, resp.ID)

	return &runtime.RuntimeHandle{
		OS:        runtime.OSLinux,
		Isolation: runtime.IsolationContainer,
		Username:  "root",
		Privilege: runtime.PrivilegeRoot,
		WorkDir:   spec.WorkDir,
		NativeID:  resp.ID,
		Transport: NewDockerExecTransport(p.dockerClient, resp.ID, p.networkName),
	}, nil
}

// Destroy force-removes the container behind handle.
func (p *DockerProvisioner) Destroy(ctx context.Context, handle *runtime.RuntimeHandle) error {
	return p.removeContainer(ctx, handle.NativeID)
}

// DestroyByID force-removes a container by ID directly, without a
// RuntimeHandle. This is what makes leftover cleanup possible across a
// daemon restart: all that's known about a previous run's container at
// that point is the ID recorded in the state file (see internal/state).
func (p *DockerProvisioner) DestroyByID(ctx context.Context, containerID string) error {
	return p.removeContainer(ctx, containerID)
}

// RecoverHandle creates a RuntimeHandle for an existing Docker container.
// This implements the runtime.WorkspaceRecoverer interface for workspace recovery
// after daemon restart.
func (p *DockerProvisioner) RecoverHandle(ctx context.Context, nativeID string, workDir string) (*runtime.RuntimeHandle, error) {
	debugf("attempting to recover docker container %s (workDir=%s)", nativeID, workDir)
	// Check if the container exists
	_, _, err := p.dockerClient.ContainerInspectWithRaw(ctx, nativeID, false)
	if err != nil {
		debugf("container inspect failed for %s: %v", nativeID, err)
		if client.IsErrNotFound(err) {
			return nil, fmt.Errorf("container %s not found", nativeID)
		}
		return nil, fmt.Errorf("failed to inspect container %s: %w", nativeID, err)
	}
	debugf("docker container %s recovered", nativeID)

	return &runtime.RuntimeHandle{
		OS:        runtime.OSLinux,
		Isolation: runtime.IsolationContainer,
		Username:  "root",
		Privilege: runtime.PrivilegeRoot,
		WorkDir:   workDir,
		NativeID:  nativeID,
		Transport: NewDockerExecTransport(p.dockerClient, nativeID, p.networkName),
	}, nil
}

func (p *DockerProvisioner) removeContainer(ctx context.Context, containerID string) error {
	// Previously this was a single ContainerRemove{Force: true}. Force maps
	// to a daemon-side SIGKILL of the container's PID 1 during removal.
	// That kill can be denied ("could not kill container: permission
	// denied") when an LSM (SELinux/AppArmor) restricts signaling the
	// container, or under rootless/userns-remap where the daemon lacks
	// kill rights over the container's host-side PID. The daemon then
	// failed the remove, kept the state-file entry, and the workspace
	// wedged into an unrecoverable state (the "hang" seen on destroy).
	//
	// This fix does NOT add privilege — it removes the dependency on the
	// privileged kill. ContainerStop sends SIGTERM first; the workspace
	// container's PID 1 is "sleep infinity", which exits cleanly on
	// SIGTERM, so no SIGKILL is needed in the normal path. Once stopped,
	// ContainerRemove has nothing to kill, so the permission-denied path
	// is never reached. We still pass Force so a container that ignores
	// SIGTERM is eventually removed via the daemon's own escalation.
	debugf("removing docker container %s", containerID)
	stopTimeout := 10 // seconds to wait after SIGTERM before daemon escalates
	stopErr := p.dockerClient.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &stopTimeout})
	if stopErr != nil && !isBenignRemoveError(stopErr) {
		// A real stop failure (not "already stopped" / "no such container")
		// is worth surfacing, but we still attempt removal below — a
		// stopped-but-unremovable container is no better than a running one.
		fmt.Printf("warn: stop before remove failed for container %s: %v\n", containerID, stopErr)
	} else if stopErr != nil {
		debugf("container %s was already stopped or gone before removal: %v", containerID, stopErr)
	}

	if err := p.dockerClient.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil {
		if isBenignRemoveError(err) {
			debugf("container %s is already gone", containerID)
			return nil // already gone
		}
		debugf("container remove failed for %s: %v", containerID, err)
		return fmt.Errorf("failed to remove container %s: %w", containerID, err)
	}
	debugf("container %s removed", containerID)
	return nil
}

// isBenignRemoveError reports whether a container stop/remove error simply
// means the container is already stopped or already gone — both are
// successes for our purposes, not failures. Leftover cleanup depends on this:
// if a tracked container no longer exists, its state-file entry must be
// cleared, not left behind to be reported as a leftover forever.
func isBenignRemoveError(err error) bool {
	if err == nil {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such container") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "is not running")
}

func (p *DockerProvisioner) pullImage(ctx context.Context, imageName string) error {
	debugf("pulling docker image %s", imageName)
	reader, err := p.dockerClient.ImagePull(ctx, imageName, image.PullOptions{})
	if err != nil {
		debugf("docker image pull request failed for %s: %v", imageName, err)
		return err
	}
	defer reader.Close()
	// Consume the stream to wait for the pull to complete
	_, err = io.Copy(io.Discard, reader)
	if err != nil {
		debugf("docker image pull stream failed for %s: %v", imageName, err)
		return err
	}
	debugf("docker image %s pulled", imageName)
	return nil
}
