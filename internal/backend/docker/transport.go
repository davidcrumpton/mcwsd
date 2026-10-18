package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// DockerExecTransport implements runtime.ExecTransport by talking to the
// Docker Engine API for a single, already-running container. It is the
// only place in this package that runs commands or moves files in/out of
// a container — DockerProvisioner only ever creates, starts, and removes
// containers (see provisioner.go).
type DockerExecTransport struct {
	dockerClient *client.Client
	containerID  string
	networkName  string
}

// NewDockerExecTransport creates a transport bound to a specific,
// already-running container.
func NewDockerExecTransport(cli *client.Client, containerID, networkName string) *DockerExecTransport {
	return &DockerExecTransport{dockerClient: cli, containerID: containerID, networkName: networkName}
}

var (
	_ runtime.ExecTransport = (*DockerExecTransport)(nil)
	_ runtime.Bootstrapper  = (*DockerExecTransport)(nil)
)

// dockerBootstrapArgs prepares a freshly created container for the workspace's
// git workflow.
//
// The workspace repo is a bind mount of a host directory created by the
// daemon's unprivileged user, while commands (including git) run as root
// inside the container. Docker preserves the uid/gid recorded in the copy
// archive, so the checked-out tree is owned by the host user, not root. Git
// then refuses to touch a repository it does not own ("detected dubious
// ownership"), which breaks the commit_and_pr flow.
//
// The entry is installed with --system (i.e. /etc/gitconfig) so it applies to
// every user in the container, and --add keeps any existing system config
// intact. This mirrors the safe.directory entry the FreeBSD jail transport
// installs during bootstrap.
var dockerBootstrapArgs = []string{"git", "config", "--system", "--add", "safe.directory", "*"}

// Bootstrap implements runtime.Bootstrapper. The backend calls it once per
// workspace while network access is still attached; failure is treated as
// non-fatal by the caller.
func (t *DockerExecTransport) Bootstrap(ctx context.Context, workDir string) error {
	res, err := t.Exec(ctx, dockerBootstrapArgs, workDir, nil)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("docker bootstrap failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// Exec runs cmd inside the container via docker exec, waiting for it to
// complete and collecting stdout/stderr/exit code.
func (t *DockerExecTransport) Exec(ctx context.Context, cmd []string, workDir string, env []string) (*internal.ExecResult, error) {
	startTime := time.Now()
	internal.Log.Debug("transport_exec_start", "transport", "docker",
		"container_id", t.containerID, "command", fmt.Sprintf("%q", cmd),
		"work_dir", workDir, "env_count", len(env))

	execConfig := container.ExecOptions{
		Cmd:          cmd,
		WorkingDir:   workDir,
		AttachStdout: true,
		AttachStderr: true,
	}
	if env != nil {
		execConfig.Env = env
	}

	execResp, err := t.dockerClient.ContainerExecCreate(ctx, t.containerID, execConfig)
	if err != nil {
		internal.Log.Debug("transport_exec_failed", "transport", "docker",
			"container_id", t.containerID, "duration_ms", time.Since(startTime).Milliseconds(), "error", err)
		return nil, fmt.Errorf("exec create failed: %w", err)
	}

	hijackResp, err := t.dockerClient.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
	if err != nil {
		internal.Log.Debug("transport_exec_failed", "transport", "docker",
			"container_id", t.containerID, "duration_ms", time.Since(startTime).Milliseconds(), "error", err)
		return nil, fmt.Errorf("exec attach failed: %w", err)
	}
	defer hijackResp.Close()

	var stdoutBuf, stderrBuf bytes.Buffer
	go stdcopy.StdCopy(&stdoutBuf, &stderrBuf, hijackResp.Reader)

	// Wait for completion
	for {
		inspect, err := t.dockerClient.ContainerExecInspect(ctx, execResp.ID)
		if err != nil {
			internal.Log.Debug("transport_exec_failed", "transport", "docker",
				"container_id", t.containerID, "duration_ms", time.Since(startTime).Milliseconds(), "error", err)
			return nil, fmt.Errorf("exec inspect failed: %w", err)
		}
		if !inspect.Running {
			durationMs := time.Since(startTime).Milliseconds()
			internal.Log.Debug("transport_exec_complete", "transport", "docker",
				"container_id", t.containerID, "exit_code", inspect.ExitCode, "duration_ms", durationMs)
			return &internal.ExecResult{
				Stdout:     stdoutBuf.String(),
				Stderr:     stderrBuf.String(),
				ExitCode:   inspect.ExitCode,
				DurationMs: durationMs,
			}, nil
		}
		select {
		case <-ctx.Done():
			internal.Log.Debug("transport_exec_failed", "transport", "docker",
				"container_id", t.containerID, "duration_ms", time.Since(startTime).Milliseconds(), "error", ctx.Err())
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// SetNetwork connects or disconnects the container from the transport's
// configured Docker network. It is idempotent: connecting a container that
// is already attached, or disconnecting one that is already detached, is a
// success rather than an error. Without this, a container left attached by
// a previous cancelled exec could never be reconnected ("endpoint ... already
// exists in network") and the workspace stayed permanently wedged.
func (t *DockerExecTransport) SetNetwork(ctx context.Context, enabled bool) error {
	if enabled {
		if err := t.dockerClient.NetworkConnect(ctx, t.networkName, t.containerID, &network.EndpointSettings{}); err != nil && !isAlreadyAttachedError(err) {
			return err
		}
		return nil
	}
	if err := t.dockerClient.NetworkDisconnect(ctx, t.networkName, t.containerID, true); err != nil && !isAlreadyDetachedError(err) {
		return err
	}
	return nil
}

// isAlreadyAttachedError reports whether a Docker NetworkConnect error just
// means the container is already on the network — the desired end state, not
// a failure. The daemon returns untyped, message-only errors for this, so —
// as with isBenignRemoveError in provisioner.go — we match on text.
func isAlreadyAttachedError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "already exists in network")
}

// isAlreadyDetachedError reports the converse: a NetworkDisconnect error that
// just means the container is not on the network. The force=true disconnect
// path can also report a missing endpoint, which is equally benign.
func isAlreadyDetachedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not connected to network") ||
		strings.Contains(msg, "does not have an endpoint")
}

// CopyDirIn tars up hostSrcDir and streams it into the container at
// destPath via the Docker CopyToContainer API.
func (t *DockerExecTransport) CopyDirIn(ctx context.Context, hostSrcDir, destPath string) error {
	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)

	filepath.Walk(hostSrcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		header, err := tar.FileInfoHeader(info, info.Name())
		if err != nil {
			return err
		}
		relPath, _ := filepath.Rel(hostSrcDir, path)
		header.Name = filepath.ToSlash(filepath.Join(filepath.Base(destPath), relPath))
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		data, err := os.Open(path)
		if err != nil {
			return err
		}
		if _, err := io.Copy(tw, data); err != nil {
			data.Close()
			return err
		}
		data.Close()
		return nil
	})

	tw.Close()

	return t.dockerClient.CopyToContainer(ctx, t.containerID, filepath.Dir(destPath), buf, container.CopyToContainerOptions{})
}

// ReadFile reads a single file's contents out of the container.
func (t *DockerExecTransport) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return readFileFromContainer(ctx, t.dockerClient, t.containerID, path)
}

// WriteFile writes content to a file inside the container, preserving the
// given file mode.
func (t *DockerExecTransport) WriteFile(ctx context.Context, path string, content []byte, mode int64) error {
	return writeFileToContainer(ctx, t.dockerClient, t.containerID, path, content, mode)
}

// readFileFromContainer and writeFileToContainer are shared by
// DockerExecTransport (bound to one container) and DockerProvisioner's
// ByContainerID helpers (used by SafeEdit, which takes an explicit
// container identifier instead of a resolved handle).

func readFileFromContainer(ctx context.Context, cli *client.Client, containerID, path string) ([]byte, error) {
	reader, _, err := cli.CopyFromContainer(ctx, containerID, path)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	tr := tar.NewReader(reader)
	header, err := tr.Next()
	if err != nil {
		return nil, err
	}

	if header.FileInfo().IsDir() {
		return nil, fmt.Errorf("expected file, got directory")
	}

	return io.ReadAll(tr)
}

func writeFileToContainer(ctx context.Context, cli *client.Client, containerID, filePath string, content []byte, mode int64) error {
	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)

	hdr := &tar.Header{
		Name: filepath.Base(filePath),
		Mode: mode,
		Size: int64(len(content)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("writeFileToContainer: write header: %w", err)
	}
	if _, err := tw.Write(content); err != nil {
		return fmt.Errorf("writeFileToContainer: write content: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("writeFileToContainer: close tar: %w", err)
	}

	return cli.CopyToContainer(ctx, containerID, filepath.Dir(filePath), buf, container.CopyToContainerOptions{})
}
