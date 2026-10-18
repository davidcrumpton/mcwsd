package podman

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
)

// debugLogPrefix is an idea from a different generation and is not needed anymore
// prefix used by the FreeBSD and OpenBSD provisioners.
const debugLogPrefix = ""

// debugf logs a debug-level message with the standard [DEBUG] shape.
func debugf(format string, args ...interface{}) {
	internal.Log.Debug(debugLogPrefix + fmt.Sprintf(format, args...))
}

// debugCommand logs, at debug level, the full command line that the
// provisioner is about to run, so a failing provision can be reproduced by
// copying the logged line.
func debugCommand(name string, args ...string) {
	internal.Log.Debug(debugLogPrefix + formatCommand(name, args...))
}

// formatCommand renders a command and its arguments as a single, copy-pasteable
// command line, quoting arguments that contain whitespace or shell
// metacharacters.
func formatCommand(name string, args ...string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, quoteArg(name))
	for _, a := range args {
		parts = append(parts, quoteArg(a))
	}
	return strings.Join(parts, " ")
}

// quoteArg shell-quotes an argument when it contains characters that would
// otherwise be interpreted by a shell.
func quoteArg(arg string) string {
	if arg == "" {
		return "''"
	}
	if !strings.ContainsAny(arg, " \t\n\"'\\$&|;<>()*?[]{}!#~`") {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

// PodmanProvisioner implements runtime.Provisioner using the Podman CLI.
type PodmanProvisioner struct {
}

func NewPodmanProvisioner() *PodmanProvisioner {
	return &PodmanProvisioner{}
}

var _ runtime.Provisioner = (*PodmanProvisioner)(nil)

func (p *PodmanProvisioner) Name() string { return "podman" }

func (p *PodmanProvisioner) Init(context.Context) error { return nil }

func (p *PodmanProvisioner) Provision(ctx context.Context, spec runtime.ProvisionSpec) (*runtime.RuntimeHandle, error) {
	containerName := fmt.Sprintf("wsd-%s", spec.WorkspaceID)

	debugf("provisioning podman container %s: image=%s workDir=%s cacheMountPath=%s workspaceBasePath=%s",
		containerName, spec.Image, spec.WorkDir, spec.CacheMountPath, spec.WorkspaceBasePath)

	// Pull image
	if err := p.pullImage(ctx, spec.Image); err != nil {
		debugf("failed to pull image %s: %v", spec.Image, err)
		return nil, fmt.Errorf("failed to pull image %s: %w", spec.Image, err)
	}
	debugf("image %s is available locally", spec.Image)

	args := []string{"create", "--name", containerName, "--workdir", spec.WorkDir}

	if spec.CacheMountPath != "" {
		if !pathExists(spec.CacheMountPath) {
			debugf("WARNING: cache mount source %s does not exist on the host; podman may reject the bind mount", spec.CacheMountPath)
		}
		debugf("adding cache bind mount %s -> /cache", spec.CacheMountPath)
		args = append(args, "-v", fmt.Sprintf("%s:/cache", spec.CacheMountPath))
	}
	if spec.WorkspaceBasePath != "" {
		// Mount a per-workspace host directory instead of the base path root.
		// Two rootless-podman realities make sharing one directory unsafe:
		//   1. Rootless containers run in a user namespace where host UID 0 is
		//      unmapped. If a rootful runtime (e.g. the Docker daemon) ever
		//      wrote into a shared root, those root-owned files can never be
		//      overwritten from a rootless container ("Cannot mkdir:
		//      Permission denied"), because capabilities do not apply to files
		//      whose owner is unmapped.
		//   2. Reusing one directory means successive workspaces clobber each
		//      other's checkout.
		// A fresh, daemon-owned directory per workspace avoids both.
		hostDir := filepath.Join(spec.WorkspaceBasePath, spec.WorkspaceID)
		debugf("ensuring workspace host dir %s", hostDir)
		if err := os.MkdirAll(hostDir, 0o755); err != nil {
			debugf("failed to create workspace dir %s: %v", hostDir, err)
			return nil, fmt.Errorf(
				"failed to create workspace dir %s: %w (if %s contains root- or subuid-owned leftovers from a rootful runtime, reclaim it once with: sudo chown -R \"$(id -u):$(id -g)\" %s)",
				hostDir, err, spec.WorkspaceBasePath, spec.WorkspaceBasePath)
		}
		debugf("adding workspace bind mount %s -> %s", hostDir, spec.WorkDir)
		args = append(args, "-v", fmt.Sprintf("%s:%s", hostDir, spec.WorkDir))
	}

	args = append(args, spec.Image, "sleep", "infinity")

	debugCommand("podman", args...)
	cmd := exec.CommandContext(ctx, "podman", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		debugf("podman create failed for %s: %v, output: %s", containerName, err, strings.TrimSpace(string(out)))
		return nil, fmt.Errorf("failed to create podman container %s: %w, output: %s", containerName, err, string(out))
	}

	// containerID is the first 12 chars or full ID
	containerID := strings.TrimSpace(string(out))
	debugf("podman container %s created with id %s", containerName, containerID)

	startArgs := []string{"start", containerID}
	debugCommand("podman", startArgs...)
	startCmd := exec.CommandContext(ctx, "podman", startArgs...)
	if out, err := startCmd.CombinedOutput(); err != nil {
		debugf("podman start failed for %s (id=%s): %v; removing partially-created container", containerName, containerID, err)
		exec.Command("podman", "rm", "-f", containerID).Run()
		return nil, fmt.Errorf("failed to start podman container %s (id=%s): %w, output: %s", containerName, containerID, err, string(out))
	}
	debugf("podman container %s started (id=%s)", containerName, containerID)

	return &runtime.RuntimeHandle{
		OS:        runtime.OSLinux,
		Isolation: runtime.IsolationContainer,
		Username:  "root",
		Privilege: runtime.PrivilegeRoot,
		WorkDir:   spec.WorkDir,
		NativeID:  containerID,
		Transport: NewPodmanExecTransport(containerID),
	}, nil
}

func (p *PodmanProvisioner) Destroy(ctx context.Context, handle *runtime.RuntimeHandle) error {
	return p.DestroyByID(ctx, handle.NativeID)
}

func (p *PodmanProvisioner) DestroyByID(ctx context.Context, containerID string) error {
	debugf("removing podman container %s", containerID)
	debugCommand("podman", "rm", "-f", containerID)
	cmd := exec.CommandContext(ctx, "podman", "rm", "-f", containerID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "no such container") || strings.Contains(string(out), "No such container") {
			debugf("podman container %s is already gone", containerID)
			return nil
		}
		debugf("podman remove failed for %s: %v, output: %s", containerID, err, strings.TrimSpace(string(out)))
		return fmt.Errorf("failed to remove podman container %s: %w, output: %s", containerID, err, string(out))
	}
	debugf("podman container %s removed", containerID)
	return nil
}

// pathExists reports whether path exists on disk.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// RecoverHandle creates a RuntimeHandle for an existing Podman container.
// This implements the runtime.WorkspaceRecoverer interface for workspace recovery
// after daemon restart.
func (p *PodmanProvisioner) RecoverHandle(ctx context.Context, nativeID string, workDir string) (*runtime.RuntimeHandle, error) {
	debugf("attempting to recover podman container %s (workDir=%s)", nativeID, workDir)
	// Check if the container exists
	debugCommand("podman", "inspect", nativeID)
	cmd := exec.CommandContext(ctx, "podman", "inspect", nativeID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		debugf("podman inspect failed for %s: %v, output: %s", nativeID, err, strings.TrimSpace(string(out)))
		if strings.Contains(string(out), "no such container") || strings.Contains(string(out), "No such container") {
			return nil, fmt.Errorf("container %s not found", nativeID)
		}
		return nil, fmt.Errorf("failed to inspect container %s: %w, output: %s", nativeID, err, string(out))
	}
	debugf("podman container %s recovered", nativeID)

	return &runtime.RuntimeHandle{
		OS:        runtime.OSLinux,
		Isolation: runtime.IsolationContainer,
		Username:  "root",
		Privilege: runtime.PrivilegeRoot,
		WorkDir:   workDir,
		NativeID:  nativeID,
		Transport: NewPodmanExecTransport(nativeID),
	}, nil
}

func (p *PodmanProvisioner) pullImage(ctx context.Context, imageName string) error {
	debugCommand("podman", "pull", imageName)
	cmd := exec.CommandContext(ctx, "podman", "pull", imageName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		debugf("podman pull failed for %s: %v, output: %s", imageName, err, strings.TrimSpace(string(out)))
		return fmt.Errorf("podman pull %s: %w, output: %s", imageName, err, strings.TrimSpace(string(out)))
	}
	debugf("image %s pulled", imageName)
	return nil
}
