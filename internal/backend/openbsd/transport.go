package openbsd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
)

// SSHExecTransport implements runtime.ExecTransport by running commands over
// an SSH connection to a provisioned VM. The VM is treated as a black box: the
// only contract is that it accepts SSH as the configured user with the
// configured key.
//
// It shells out to the host's ssh(1) binary rather than linking a Go SSH
// implementation, matching the rest of the daemon's "run a command, capture
// output" style and adding no dependencies.
type SSHExecTransport struct {
	ip      string
	port    int
	user    string
	keyPath string

	// escalationCommand, when non-empty, is prepended (as `doas sh -c '<cmd>'`)
	// to every command. Set it when user is not root and the guest is
	// configured to allow escalation.
	escalationCommand string

	// os is the guest OS, carried for completeness/logging.
	os runtime.OS

	// sshBinary is the ssh(1) executable to invoke. It is a field so tests can
	// substitute a fake.
	sshBinary string
}

// NewSSHExecTransport creates an SSH transport for the VM at ip.
func NewSSHExecTransport(ip string, port int, user, keyPath, escalationCommand string, guestOS runtime.OS) *SSHExecTransport {
	if port == 0 {
		port = defaultSSHPort
	}
	if user == "" {
		user = defaultLoginUser
	}
	if guestOS == "" {
		guestOS = runtime.OSOpenBSD
	}
	// Root needs no escalation. This also prevents a guest escalation setting
	// from being applied inside the guest when the login user is root, where
	// it would only get in the way.
	if user == "root" {
		escalationCommand = ""
	}
	return &SSHExecTransport{
		ip:                ip,
		port:              port,
		user:              user,
		keyPath:           keyPath,
		escalationCommand: escalationCommand,
		os:                guestOS,
		sshBinary:         "ssh",
	}
}

var _ runtime.ExecTransport = (*SSHExecTransport)(nil)

// guestPath is the PATH exported for every command run in the guest. sshd
// starts a non-login shell, which on OpenBSD does not necessarily include the
// package prefix, so tools installed with pkg_add(1) would not be found.
const guestPath = "/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin:/usr/local/bin"

// buildSSHArgs assembles the ssh(1) argument vector. The remote payload is a
// single argument; ssh concatenates its arguments and hands the result to the
// remote user's login shell, so a payload containing `&&` or `;` is still
// interpreted remotely (as intended) but never by the local shell.
func buildSSHArgs(ip string, port int, user, keyPath, payload string) []string {
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=55",
		"-o", "LogLevel=ERROR",
	}
	if keyPath != "" {
		args = append(args, "-i", keyPath)
	}
	args = append(args,
		"-p", strconv.Itoa(port),
		user+"@"+ip,
		payload,
	)
	return args
}

// buildRemoteCommand renders the inner shell payload: environment
// assignments, then a cd into workDir, then the command itself.
func buildRemoteCommand(cmd []string, workDir string, env []string) string {
	cmdStr := formatArgv(cmd)

	// Variable assignments must immediately precede the command they apply to,
	// so they go after the cd (a `VAR=x cd dir && cmd` would only set VAR for
	// the cd).
	if len(env) > 0 {
		assigns := make([]string, 0, len(env))
		for _, e := range env {
			assigns = append(assigns, quoteArg(e))
		}
		cmdStr = strings.Join(assigns, " ") + " " + cmdStr
	}

	if workDir != "" {
		cmdStr = "cd " + quoteArg(workDir) + " && " + cmdStr
	}
	return cmdStr
}

// remotePayload builds the full payload handed to ssh(1), wrapping the inner
// command in the escalation command when one is configured. The wrap is
// `doas sh -c '<inner>'` (not `doas <inner>`) so that a compound inner command
// (with cd/&&) is still interpreted by a shell — and so that the env
// assignments are applied inside the escalated shell.
func (t *SSHExecTransport) remotePayload(cmd []string, workDir string, env []string) string {
	inner := buildRemoteCommand(cmd, workDir, env)
	if t.escalationCommand != "" {
		return t.escalationCommand + " sh -c " + quoteArg(inner)
	}
	return inner
}

// runSSH invokes ssh(1) with payload, optionally streaming stdin, and returns
// the captured result. A remote non-zero exit is reported in the result, not
// as an error; only a local failure (ssh missing, context cancelled) is an
// error.
func (t *SSHExecTransport) runSSH(ctx context.Context, payload string, stdin io.Reader) (*internal.ExecResult, error) {
	args := buildSSHArgs(t.ip, t.port, t.user, t.keyPath, payload)
	startTime := time.Now()
	internal.Log.Debug("transport_ssh_start", "host", t.ip, "port", t.port,
		"user", t.user, "stdin_stream", stdin != nil)

	cmd := exec.CommandContext(ctx, t.sshBinary, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	durationMs := time.Since(startTime).Milliseconds()

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			internal.Log.Debug("transport_ssh_failed", "host", t.ip,
				"duration_ms", durationMs, "error", err)
			return nil, err
		}
	}

	internal.Log.Debug("transport_ssh_complete", "host", t.ip,
		"exit_code", exitCode, "duration_ms", durationMs)
	return &internal.ExecResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		ExitCode:   exitCode,
		DurationMs: durationMs,
	}, nil
}

// Exec runs a command in the guest over SSH.
func (t *SSHExecTransport) Exec(ctx context.Context, cmd []string, workDir string, env []string) (*internal.ExecResult, error) {
	// Always export a PATH that includes the package prefix; caller-supplied
	// env entries follow it, so an explicit PATH from the caller wins.
	fullEnv := append([]string{"PATH=" + guestPath}, env...)
	internal.Log.Debug("transport_exec_start", "transport", "openbsd_ssh",
		"host", t.ip, "port", t.port, "user", t.user,
		"command", fmt.Sprintf("%q", cmd), "work_dir", workDir, "env_count", len(env))
	return t.runSSH(ctx, t.remotePayload(cmd, workDir, fullEnv), nil)
}

// SetNetwork is a no-op for VMs. A VM is isolated from the host by the
// hypervisor, and vmd(8) offers no cheap per-VM network toggle, so unlike a
// jail the guest simply stays networked for its lifetime. It returns nil so
// the backend's attach/detach bookkeeping is harmless.
func (t *SSHExecTransport) SetNetwork(ctx context.Context, enabled bool) error {
	debugf("SetNetwork(%t) is a no-op for openbsd VMs (guest stays networked)", enabled)
	return nil
}

// CopyDirIn recursively copies hostSrcDir into the guest at destPath. It
// creates a tar archive on the host and streams it to tar(1) inside the guest
// over SSH, preserving metadata.
func (t *SSHExecTransport) CopyDirIn(ctx context.Context, hostSrcDir, destPath string) error {
	if _, err := t.Exec(ctx, []string{"mkdir", "-p", destPath}, "", nil); err != nil {
		return fmt.Errorf("failed to create destination directory %s in guest: %w", destPath, err)
	}

	tmpTar, err := createTarArchive(hostSrcDir)
	if err != nil {
		return fmt.Errorf("failed to create tar archive of %s: %w", hostSrcDir, err)
	}
	defer os.Remove(tmpTar)

	f, err := os.Open(tmpTar)
	if err != nil {
		return fmt.Errorf("failed to open tar archive: %w", err)
	}
	defer f.Close()

	// The archive is rooted at ".", so it must be extracted into destPath
	// itself for the contents to land there.
	payload := t.remotePayload([]string{"tar", "-xf", "-", "-C", destPath}, "", nil)
	res, err := t.runSSH(ctx, payload, f)
	if err != nil {
		return fmt.Errorf("failed to stream tar into guest: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("guest tar extraction failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// ReadFile reads a single file from the guest.
func (t *SSHExecTransport) ReadFile(ctx context.Context, path string) ([]byte, error) {
	res, err := t.Exec(ctx, []string{"cat", path}, "", nil)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("failed to read %s in guest (exit %d): %s", path, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return []byte(res.Stdout), nil
}

// WriteFile writes content to path in the guest. The content is streamed to
// cat(1) on stdin rather than interpolated into a shell command, so newlines,
// quotes and backslashes are preserved exactly.
func (t *SSHExecTransport) WriteFile(ctx context.Context, path string, content []byte, mode int64) error {
	if _, err := t.Exec(ctx, []string{"mkdir", "-p", filepath.Dir(path)}, "", nil); err != nil {
		return fmt.Errorf("failed to create parent directory for %s in guest: %w", path, err)
	}

	// Path is bound as $1 so it needs no quoting; see the jail transport for
	// the same pattern.
	payload := t.remotePayload([]string{"sh", "-c", `cat > "$1"`, "sh", path}, "", nil)
	res, err := t.runSSH(ctx, payload, bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("failed to write %s in guest: %w", path, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("failed to write %s in guest (exit %d): %s", path, res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	if _, err := t.Exec(ctx, []string{"chmod", fmt.Sprintf("%o", mode), path}, "", nil); err != nil {
		return fmt.Errorf("failed to chmod %s in guest: %w", path, err)
	}
	return nil
}

// createTarArchive creates a tar archive of srcDir rooted at ".".
func createTarArchive(srcDir string) (string, error) {
	tmpFile, err := os.CreateTemp("", "wsd-ssh-copy-*.tar")
	if err != nil {
		return "", err
	}
	tmpFile.Close()

	debugCommand("tar", "-cf", tmpFile.Name(), "-C", srcDir, ".")
	cmd := exec.Command("tar", "-cf", tmpFile.Name(), "-C", srcDir, ".")
	if err := cmd.Run(); err != nil {
		os.Remove(tmpFile.Name())
		return "", err
	}
	return tmpFile.Name(), nil
}
