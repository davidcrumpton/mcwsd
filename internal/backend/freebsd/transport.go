package freebsd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
)

// FreeBSDJailExecTransport implements runtime.ExecTransport for FreeBSD jails.
// It uses the jexec(8) command-line tool to execute commands inside jails.
type FreeBSDJailExecTransport struct {
	jailName      string
	networkMode   string
	vnetInterface string

	// escalationCommand, when non-empty, is prepended to the commands that
	// require root: jexec(8), jls(8) and `jail -m`. This lets mcwsd run as an
	// unprivileged daemon user while still executing commands inside the
	// jail. Set it to "mdo" on FreeBSD 15+.
	escalationCommand string
}

// NewFreeBSDJailExecTransport creates a new transport for executing commands in a FreeBSD jail.
func NewFreeBSDJailExecTransport(jailName string) *FreeBSDJailExecTransport {
	return &FreeBSDJailExecTransport{jailName: jailName, networkMode: "shared"}
}

func (t *FreeBSDJailExecTransport) WithNetworkMode(mode, vnetInterface string) *FreeBSDJailExecTransport {
	t.networkMode = mode
	t.vnetInterface = vnetInterface
	return t
}

// WithEscalationCommand sets the command used to escalate the root-only
// operations (jexec, jls, jail -m). Pass "mdo" to run mcwsd as an unprivileged
// user on FreeBSD 15+; pass "" (the default) to run them directly, which is
// only correct if the daemon process itself is root. Returns the receiver so
// it can be chained after NewFreeBSDJailExecTransport.
func (t *FreeBSDJailExecTransport) WithEscalationCommand(cmd string) *FreeBSDJailExecTransport {
	t.escalationCommand = cmd
	return t
}

// command builds the exec.Cmd for name/args, routing it through the configured
// escalation command (e.g. "mdo") when one is set. jexec(8), jls(8) and
// jail(8) all require root, so an unprivileged daemon must escalate them.
func (t *FreeBSDJailExecTransport) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	if t.escalationCommand == "" {
		debugCommand(name, redactJexecEnvironment(args)...)
		return exec.CommandContext(ctx, name, args...)
	}
	escalated := append([]string{name}, args...)
	debugCommand(t.escalationCommand, redactJexecEnvironment(escalated)...)
	return exec.CommandContext(ctx, t.escalationCommand, escalated...)
}

func redactJexecEnvironment(args []string) []string {
	redacted := append([]string(nil), args...)
	for i, arg := range redacted {
		if arg == "env" && i+1 < len(redacted) {
			for j := i + 1; j < len(redacted); j++ {
				if !strings.Contains(redacted[j], "=") {
					break
				}
				name, _, _ := strings.Cut(redacted[j], "=")
				redacted[j] = name + "=[redacted]"
			}
			break
		}
	}
	return redacted
}

var (
	_ runtime.ExecTransport = (*FreeBSDJailExecTransport)(nil)
	_ runtime.Bootstrapper  = (*FreeBSDJailExecTransport)(nil)
)

// bootstrapScript prepares a freshly populated jail to install software.
//
//   - certctl rehash builds the OpenSSL trust store (/etc/ssl/cert.pem and
//     /etc/ssl/certs) from the Mozilla certs in /usr/share/certs/trusted. A
//     jail untarred from base.txz has the certs but not the store, because
//     rehash normally runs during a real install. Without it, pkg(8) fails
//     HTTPS certificate verification and cannot bootstrap.
//   - pkg bootstrap installs the real pkg(8); a base.txz jail only has the
//     bootstrap stub. ASSUME_ALWAYS_YES makes it non-interactive.
//   - git is required by the workspace's commit flow (CommitAndPR runs git
//     inside the jail) and ca_root_nss supplies the CA bundle git/curl need
//     for HTTPS.
//   - The workspace repo is a nullfs mount of a host directory owned by the
//     daemon's unprivileged user, while git runs as root inside the jail.
//     Without a safe.directory entry git refuses to touch it ("detected
//     dubious ownership").
//
// Re-running it is harmless: rehash, pkg bootstrap and the git config are all
// idempotent.
const bootstrapScript = "certctl rehash && env ASSUME_ALWAYS_YES=yes pkg bootstrap -y && pkg install -y git ca_root_nss && git config --system --add safe.directory '*'"

// Bootstrap implements runtime.Bootstrapper. The backend calls it once per
// workspace while network access is still attached; failure is treated as
// non-fatal by the caller.
func (t *FreeBSDJailExecTransport) Bootstrap(ctx context.Context, workDir string) error {
	res, err := t.Exec(ctx, []string{"sh", "-c", bootstrapScript}, workDir, nil)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("freebsd bootstrap failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// jailPath is the PATH exported for every command run inside a jail.
//
// jexec(8) does not start a login shell, so commands inherit the daemon's
// environment. A daemon started from rc(8) has a minimal PATH that omits the
// ports prefix, so tools installed with pkg(8) (git, node, ...) would not be
// found even though they are installed. Always export a PATH that includes
// /usr/local/{bin,sbin}.
const jailPath = "/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin:/usr/local/bin"

// buildJexecArgs assembles the argument vector for jexec(8):
// jexec [-d workDir] <jail> env PATH=<jailPath> [VAR=val ...] <command ...>
//
// The default PATH is always exported; caller-supplied env entries follow it,
// so an explicit PATH from the caller wins.
func buildJexecArgs(jailName string, cmd []string, workDir string, env []string) []string {
	args := []string{}

	// Set the working directory with jexec's -d flag (must precede the jail).
	if workDir != "" {
		args = append(args, "-d", workDir)
	}

	// Environment variables are applied by running env(1) inside the jail.
	args = append(args, jailName, "env", "PATH="+jailPath)
	args = append(args, env...)

	return append(args, cmd...)
}

// Exec runs a command inside the jail using jexec.
func (t *FreeBSDJailExecTransport) Exec(ctx context.Context, cmd []string, workDir string, env []string) (*internal.ExecResult, error) {
	args := buildJexecArgs(t.jailName, cmd, workDir, env)

	fullCmd := t.command(ctx, "jexec", args...)
	startTime := time.Now()
	internal.Log.Debug("transport_exec_start", "transport", "freebsd_jail",
		"jail", t.jailName, "command", fmt.Sprintf("%q", cmd),
		"work_dir", workDir, "env_count", len(env))

	// Capture stdout and stderr
	var stdout, stderr bytes.Buffer
	fullCmd.Stdout = &stdout
	fullCmd.Stderr = &stderr

	err := fullCmd.Run()
	durationMs := time.Since(startTime).Milliseconds()

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			internal.Log.Debug("transport_exec_failed", "transport", "freebsd_jail",
				"jail", t.jailName, "duration_ms", durationMs, "error", err)
			return nil, err
		}
	}

	internal.Log.Debug("transport_exec_complete", "transport", "freebsd_jail",
		"jail", t.jailName, "exit_code", exitCode, "duration_ms", durationMs)
	return &internal.ExecResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		ExitCode:   exitCode,
		DurationMs: durationMs,
	}, nil
}

// buildSetNetworkArgs assembles the argument vector for `jail -m` when
// toggling a jail's IPv4 access. In the command-line form (no -f config file)
// jail(8) requires every argument to be a param=value pair and identifies the
// jail via the name= parameter; passing the jail name positionally makes jail
// treat it as a parameter and fail with "unknown parameter".
func buildSetNetworkArgs(jailName, mode string) []string {
	return []string{"-m", "name=" + jailName, "ip4=" + mode}
}

// SetNetwork enables or disables IPv4 networking for the jail by toggling the
// jail's ip4 parameter at runtime. "inherit" shares the host's (DHCP-assigned)
// addresses; "disable" denies access to all IPv4 addresses. This matches the
// non-VNET jail model used by the provisioner.
func (t *FreeBSDJailExecTransport) SetNetwork(ctx context.Context, enabled bool) error {
	if t.networkMode == "vnet" {
		if t.vnetInterface == "" {
			return fmt.Errorf("VNET jail %s has no network interface", t.jailName)
		}
		state := "down"
		if enabled {
			state = "up"
		}
		cmd := t.command(ctx, "jexec", t.jailName, "ifconfig", t.vnetInterface, state)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to set VNET jail network %s: %w: %s", state, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	mode := "disable"
	if enabled {
		mode = "inherit"
	}

	cmd := t.command(ctx, "jail", buildSetNetworkArgs(t.jailName, mode)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to set jail network %s: %w: %s", mode, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// CopyDirIn recursively copies a directory from the host into the jail.
// This uses tar to preserve file metadata.
func (t *FreeBSDJailExecTransport) CopyDirIn(ctx context.Context, hostSrcDir, destPath string) error {
	// First, ensure the destination directory exists in the jail. The tar
	// archive is created with "." as its root, so it must be extracted into
	// destPath itself (not its parent) for the contents to land there.
	mkdirCmd := t.command(ctx, "jexec", t.jailName, "mkdir", "-p", destPath)
	if err := mkdirCmd.Run(); err != nil {
		return fmt.Errorf("failed to create dest dir in jail: %w", err)
	}

	// Use tar to copy the directory
	// Create tar archive on host, then extract in jail
	tmpTar, err := createTarArchive(hostSrcDir)
	if err != nil {
		return fmt.Errorf("failed to create tar archive: %w", err)
	}
	defer os.Remove(tmpTar)

	// Copy tar file to jail
	// We'll use jexec with tar -xf
	extractCmd := t.command(ctx, "jexec", t.jailName, "tar", "-xf", tmpTar, "-C", destPath)
	if err := extractCmd.Run(); err != nil {
		return fmt.Errorf("failed to extract tar in jail: %w", err)
	}

	return nil
}

// ReadFile reads a file from inside the jail.
func (t *FreeBSDJailExecTransport) ReadFile(ctx context.Context, path string) ([]byte, error) {
	// Use jexec with cat to read the file
	cmd := t.command(ctx, "jexec", t.jailName, "cat", path)
	return cmd.Output()
}

// buildWriteFileArgs assembles the jexec argument vector used to write a file.
// The content is never interpolated into the command; it is streamed on stdin
// to cat(1), which is why the script is just `cat > "$1"` with the path bound
// as $1 (so paths containing spaces need no quoting). See WriteFile.
func buildWriteFileArgs(jailName, path string) []string {
	return []string{jailName, "sh", "-c", `cat > "$1"`, "sh", path}
}

// WriteFile writes a file inside the jail.
//
// The content is streamed to cat(1) on stdin rather than interpolated into a
// shell command. This preserves newlines, quotes, backslashes and every other
// byte exactly, and avoids depending on the shell's echo(1), whose handling of
// backslash escapes is not portable (FreeBSD's sh(1) builtin does not expand
// them, which previously turned multi-line files into a single literal line).
func (t *FreeBSDJailExecTransport) WriteFile(ctx context.Context, path string, content []byte, mode int64) error {
	// First, ensure the directory exists
	mkdirCmd := t.command(ctx, "jexec", t.jailName, "mkdir", "-p", filepath.Dir(path))
	if err := mkdirCmd.Run(); err != nil {
		return fmt.Errorf("failed to create file dir in jail: %w", err)
	}

	// jexec(8) inherits stdin, so cat(1) inside the jail reads the content
	// directly from this process. No escaping is involved.
	writeCmd := t.command(ctx, "jexec", buildWriteFileArgs(t.jailName, path)...)
	writeCmd.Stdin = bytes.NewReader(content)
	if out, err := writeCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to write file in jail: %w: %s", err, strings.TrimSpace(string(out)))
	}

	// Set the file mode
	chmodCmd := t.command(ctx, "jexec", t.jailName, "chmod", fmt.Sprintf("%o", mode), path)
	return chmodCmd.Run()
}

// createTarArchive creates a tar archive of the source directory.
func createTarArchive(srcDir string) (string, error) {
	tmpFile, err := os.CreateTemp("", "wsd-jail-copy-*.tar")
	if err != nil {
		return "", err
	}
	tmpFile.Close()

	// Create tar archive
	debugCommand("tar", "-cf", tmpFile.Name(), "-C", srcDir, ".")
	cmd := exec.Command("tar", "-cf", tmpFile.Name(), "-C", srcDir, ".")
	if err := cmd.Run(); err != nil {
		os.Remove(tmpFile.Name())
		return "", err
	}

	return tmpFile.Name(), nil
}

// VerifyJailRunning checks if the jail is currently running.
func (t *FreeBSDJailExecTransport) VerifyJailRunning(ctx context.Context) bool {
	cmd := t.command(ctx, "jls", "-q", t.jailName)
	return cmd.Run() == nil
}

// GetJailID returns the jail identifier (name).
func (t *FreeBSDJailExecTransport) GetJailID() string {
	return t.jailName
}
