package zones

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
)

// zonesPath is the PATH exported for every command run inside a zone.
// zlogin does not start a login shell, so a daemon started from SMF inherits a
// minimal PATH; /usr/local/{bin,sbin} are included for tools installed with
// pkg(1M).
const zonesPath = "/usr/sbin:/usr/bin:/sbin:/bin:/usr/local/sbin:/usr/local/bin"

// bootstrapScript prepares a freshly cloned or installed zone to install
// software. git is required by the workspace commit flow; ca-certificates
// supplies the trust store for HTTPS. safe.directory lets git operate on a
// checkout owned by a different UID. Re-running it is harmless.
const bootstrapScript = "pkg install -y git ca-certificates && git config --system --add safe.directory '*'"

// execRunner runs a host command and returns its captured result. It is a
// field on the transport so tests can substitute a fake.
type execRunner func(ctx context.Context, stdin io.Reader, name string, args ...string) (*internal.ExecResult, error)

// ZonesExecTransport implements runtime.ExecTransport by running commands in a
// zone with zlogin(1).
type ZonesExecTransport struct {
	zoneName    string
	escalation  string
	networkMode string
	vnic        string

	run execRunner
}

var (
	_ runtime.ExecTransport = (*ZonesExecTransport)(nil)
	_ runtime.Bootstrapper  = (*ZonesExecTransport)(nil)
)

// NewZonesExecTransport creates a transport for the named zone.
func NewZonesExecTransport(zoneName, escalation, networkMode, vnic string) *ZonesExecTransport {
	if networkMode == "" {
		networkMode = defaultNetworkMode
	}
	return &ZonesExecTransport{
		zoneName:    zoneName,
		escalation:  escalation,
		networkMode: networkMode,
		vnic:        vnic,
		run:         defaultExecRunner,
	}
}

// command runs name/args on the host, prefixing the configured escalation
// command when one is set.
func (t *ZonesExecTransport) command(ctx context.Context, stdin io.Reader, name string, args ...string) (*internal.ExecResult, error) {
	argv := append([]string{name}, args...)
	if t.escalation != "" {
		argv = append([]string{t.escalation}, argv...)
	}
	debugCommand(argv[0], argv[1:]...)
	return t.run(ctx, stdin, argv[0], argv[1:]...)
}

// Exec runs a command inside the zone. A non-zero zone exit is reported in the
// result, not as an error; only a local failure to start zlogin is an error.
func (t *ZonesExecTransport) Exec(ctx context.Context, cmd []string, workDir string, env []string) (*internal.ExecResult, error) {
	if len(cmd) == 0 {
		return nil, fmt.Errorf("zones exec: empty command")
	}
	payload := buildZloginPayload(cmd, workDir, env)
	internal.Log.Debug("transport_exec_start", "transport", "zones", "zone", t.zoneName,
		"command", fmt.Sprintf("%q", cmd), "work_dir", workDir, "env_count", len(env))
	res, err := t.command(ctx, nil, "zlogin", t.zoneName, "sh", "-c", payload)
	if err != nil {
		return nil, err
	}
	internal.Log.Debug("transport_exec_complete", "transport", "zones", "zone", t.zoneName,
		"exit_code", res.ExitCode, "duration_ms", res.DurationMs)
	return res, nil
}

// Bootstrap installs the baseline tooling inside a freshly provisioned zone.
func (t *ZonesExecTransport) Bootstrap(ctx context.Context, workDir string) error {
	res, err := t.Exec(ctx, []string{"sh", "-c", bootstrapScript}, workDir, nil)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("zones bootstrap failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// SetNetwork enables or disables network access for the zone. Shared-IP zones
// use the global zone's stack and cannot be isolated per zone, so this is a
// no-op for them. Exclusive-IP zones toggle their delegated VNIC.
func (t *ZonesExecTransport) SetNetwork(ctx context.Context, enabled bool) error {
	if t.networkMode != "exclusive" {
		debugf("SetNetwork(%t) is a no-op for shared-IP zone %s", enabled, t.zoneName)
		return nil
	}
	if t.vnic == "" {
		return fmt.Errorf("exclusive-IP zone %s has no VNIC", t.zoneName)
	}
	state := "down"
	if enabled {
		state = "up"
	}
	res, err := t.command(ctx, nil, "zlogin", t.zoneName, "ifconfig", t.vnic, state)
	if err != nil {
		return fmt.Errorf("failed to set zone network %s: %w", state, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("failed to set zone network %s (exit %d): %s", state, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// CopyDirIn recursively copies a directory from the host into the zone by
// streaming a tar archive to tar(1) inside the zone.
func (t *ZonesExecTransport) CopyDirIn(ctx context.Context, hostSrcDir, destPath string) error {
	if _, err := t.Exec(ctx, []string{"mkdir", "-p", destPath}, "", nil); err != nil {
		return fmt.Errorf("failed to create destination directory %s in zone: %w", destPath, err)
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
	res, err := t.command(ctx, f, "zlogin", t.zoneName, "tar", "-xf", "-", "-C", destPath)
	if err != nil {
		return fmt.Errorf("failed to stream tar into zone: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("zone tar extraction failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// ReadFile reads a single file from the zone.
func (t *ZonesExecTransport) ReadFile(ctx context.Context, path string) ([]byte, error) {
	res, err := t.command(ctx, nil, "zlogin", t.zoneName, "cat", path)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("failed to read %s in zone (exit %d): %s", path, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return []byte(res.Stdout), nil
}

// WriteFile writes content to path in the zone. The content is streamed to
// cat(1) on stdin, so newlines, quotes, and backslashes are preserved exactly.
func (t *ZonesExecTransport) WriteFile(ctx context.Context, path string, content []byte, mode int64) error {
	if _, err := t.Exec(ctx, []string{"mkdir", "-p", filepath.Dir(path)}, "", nil); err != nil {
		return fmt.Errorf("failed to create parent directory for %s in zone: %w", path, err)
	}

	// The path is bound as $1 so it needs no shell quoting.
	res, err := t.command(ctx, bytes.NewReader(content), "zlogin", t.zoneName,
		"sh", "-c", `cat > "$1"`, "sh", path)
	if err != nil {
		return fmt.Errorf("failed to write %s in zone: %w", path, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("failed to write %s in zone (exit %d): %s", path, res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	chmodRes, err := t.Exec(ctx, []string{"chmod", fmt.Sprintf("%o", mode), path}, "", nil)
	if err != nil {
		return fmt.Errorf("failed to chmod %s in zone: %w", path, err)
	}
	if chmodRes.ExitCode != 0 {
		return fmt.Errorf("failed to chmod %s in zone (exit %d): %s", path, chmodRes.ExitCode, strings.TrimSpace(chmodRes.Stderr))
	}
	return nil
}

// buildZloginPayload renders the inner shell payload handed to
// `zlogin <zone> sh -c`: PATH and caller environment assignments, an optional
// cd, then the command.
func buildZloginPayload(cmd []string, workDir string, env []string) string {
	assigns := make([]string, 0, len(env)+1)
	assigns = append(assigns, "PATH="+zonesPath)
	for _, e := range env {
		assigns = append(assigns, quoteArg(e))
	}

	parts := make([]string, 0, len(cmd))
	for _, arg := range cmd {
		parts = append(parts, quoteArg(arg))
	}
	payload := "env " + strings.Join(assigns, " ") + " " + strings.Join(parts, " ")
	if workDir != "" {
		payload = "cd " + quoteArg(workDir) + " && " + payload
	}
	return payload
}

// defaultExecRunner runs a host command, capturing stdout and stderr
// separately (ReadFile must return raw bytes).
func defaultExecRunner(ctx context.Context, stdin io.Reader, name string, args ...string) (*internal.ExecResult, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	durationMs := time.Since(start).Milliseconds()

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, err
		}
	}
	return &internal.ExecResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		ExitCode:   exitCode,
		DurationMs: durationMs,
	}, nil
}
