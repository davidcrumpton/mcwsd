package podman

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
)

type PodmanExecTransport struct {
	containerID string
}

func NewPodmanExecTransport(containerID string) *PodmanExecTransport {
	return &PodmanExecTransport{containerID: containerID}
}

var _ runtime.ExecTransport = (*PodmanExecTransport)(nil)

func (t *PodmanExecTransport) Exec(ctx context.Context, cmd []string, workDir string, env []string) (*internal.ExecResult, error) {
	startTime := time.Now()
	internal.Log.Debug("transport_exec_start", "transport", "podman",
		"container_id", t.containerID, "command", fmt.Sprintf("%q", cmd),
		"work_dir", workDir, "env_count", len(env))

	args := []string{"exec", "--workdir", workDir}
	for _, e := range env {
		args = append(args, "--env", e)
	}
	args = append(args, t.containerID)
	args = append(args, cmd...)

	c := exec.CommandContext(ctx, "podman", args...)

	// Capture stdout and stderr
	// Wait, we need to be careful with large outputs, but for now we'll capture to buffer
	// Or we can use combined output, but we need them separated.
	// We'll use StderrPipe and StdoutPipe but for simplicity, we can use buffers.
	var stdout, stderr stringsBuilder
	c.Stdout = &stdout
	c.Stderr = &stderr

	err := c.Run()
	durationMs := time.Since(startTime).Milliseconds()

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			internal.Log.Debug("transport_exec_failed", "transport", "podman",
				"container_id", t.containerID, "duration_ms", durationMs, "error", err)
			return nil, err
		}
	}

	internal.Log.Debug("transport_exec_complete", "transport", "podman",
		"container_id", t.containerID, "exit_code", exitCode, "duration_ms", durationMs)
	return &internal.ExecResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		ExitCode:   exitCode,
		DurationMs: durationMs,
	}, nil
}

// stringsBuilder is a simple wrapper to implement io.Writer
type stringsBuilder struct {
	data []byte
}

func (s *stringsBuilder) Write(p []byte) (n int, err error) {
	s.data = append(s.data, p...)
	return len(p), nil
}

func (s *stringsBuilder) String() string {
	return string(s.data)
}

func (t *PodmanExecTransport) SetNetwork(ctx context.Context, enabled bool) error {
	// Podman rootless networking (slirp4netns/pasta) doesn't easily support
	// dynamic connect/disconnect without breaking the container namespace.
	// For MVP, we treat this as a no-op.
	return nil
}

func (t *PodmanExecTransport) CopyDirIn(ctx context.Context, hostSrcDir, destPath string) error {
	// Run every container-side file operation as root. Under rootless podman
	// container root maps to the invoking host user, so extracted files land on
	// the host owned by that user (not a subuid), and root's DAC_OVERRIDE lets
	// tar replace directories regardless of the image's default (possibly
	// non-root) user.

	// Create destPath first
	exec.CommandContext(ctx, "podman", "exec", "--user", "0", t.containerID, "mkdir", "-p", destPath).Run()

	// Ensure the destination is fully writable inside the container before extraction.
	// A prior failed provisioning attempt can leave .git subdirectories with read-only
	// permissions (e.g. .git/refs/tags, .git/objects/info), which causes tar to fail
	// with "Cannot mkdir: Permission denied" even when --no-same-permissions is set.
	exec.CommandContext(ctx, "podman", "exec", "--user", "0", t.containerID, "chmod", "-R", "u+rwX", destPath).Run()

	src := hostSrcDir
	if !filepath.IsAbs(src) {
		cwd, _ := os.Getwd()
		src = filepath.Join(cwd, src)
	}

	// Build the tar stream on the host with forced permissions so that any tar
	// implementation inside the container (including BusyBox tar, which silently
	// ignores GNU-specific flags like --no-same-permissions) receives an archive
	// where all directories are user-writable. Git repos intentionally set
	// .git/refs and .git/objects/* to mode 555; without this, BusyBox tar
	// creates those directories read-only and immediately fails to mkdir children
	// into them (e.g. .git/refs/tags, .git/objects/info).
	tarCmd := exec.CommandContext(ctx, "tar", "--mode=u+rwX,go+rX", "-cf", "-", "-C", src, ".")
	execCmd := exec.CommandContext(ctx, "podman", "exec", "--user", "0", "-i", t.containerID, "tar", "-m", "--no-same-owner", "--no-same-permissions", "--overwrite", "-xf", "-", "-C", destPath)

	pipe, err := tarCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("tar stdout pipe failed: %w", err)
	}
	execCmd.Stdin = pipe

	if err := tarCmd.Start(); err != nil {
		return fmt.Errorf("tar start failed: %w", err)
	}

	if out, err := execCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("podman exec tar extract failed: %w, output: %s", err, string(out))
	}
	if err := tarCmd.Wait(); err != nil {
		return fmt.Errorf("tar wait failed: %w", err)
	}
	return nil
}

func (t *PodmanExecTransport) ReadFile(ctx context.Context, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "podman", "exec", "--user", "0", t.containerID, "cat", path)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("podman exec cat failed: %w, stderr: %s", err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("podman exec cat failed: %w", err)
	}
	return out, nil
}

func (t *PodmanExecTransport) WriteFile(ctx context.Context, path string, content []byte, mode int64) error {
	// Make sure target dir exists
	exec.CommandContext(ctx, "podman", "exec", "--user", "0", t.containerID, "mkdir", "-p", filepath.Dir(path)).Run()

	cmd := exec.CommandContext(ctx, "podman", "exec", "--user", "0", "-i", t.containerID, "sh", "-c", fmt.Sprintf("cat > '%s'", path))
	cmd.Stdin = bytes.NewReader(content)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("podman exec write failed: %w, output: %s", err, string(out))
	}

	// Apply mode
	chmodCmd := exec.CommandContext(ctx, "podman", "exec", "--user", "0", t.containerID, "chmod", fmt.Sprintf("%o", mode), path)
	chmodCmd.Run()

	return nil
}
