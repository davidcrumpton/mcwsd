package script

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
	"github.com/dop251/goja"
)

// ExecTransport implements runtime.ExecTransport generically on top of the
// plugin's execArgs pure function. Core owns ReadFile, WriteFile, CopyDirIn and
// Bootstrap; the plugin only describes how to turn (handle, argv, workDir, env)
// into the host command line that runs it.
type ExecTransport struct {
	runtime     *Runtime
	provisioner *Provisioner
	nativeID    string
	handle      map[string]interface{}

	// mu serializes access to the shared transport VM. goja VMs are not safe
	// for concurrent calls, and execArgs is the only operation that touches it;
	// the actual command execution happens in Go, outside the lock.
	mu sync.Mutex
}

var (
	_ runtime.ExecTransport = (*ExecTransport)(nil)
	_ runtime.Bootstrapper  = (*ExecTransport)(nil)
)

// execArgs invokes the plugin's execArgs(handle, cmd, workDir, env, opts) and
// returns the resulting argv. opts carries {stdin, root}: core sets root=true
// for its own privileged file I/O (ReadFile/WriteFile/CopyDirIn) and false for
// user Exec, so a plugin can run file operations as root while leaving user
// commands at the image's default user.
func (t *ExecTransport) execArgs(cmd []string, workDir string, env []string, stdin, root bool) ([]string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	vm := t.runtime.transport.vm
	out, err := t.runtime.transport.execArgs(goja.Undefined(),
		vm.ToValue(t.handle),
		vm.ToValue(cmd),
		vm.ToValue(workDir),
		vm.ToValue(env),
		vm.ToValue(map[string]interface{}{"stdin": stdin, "root": root}),
	)
	if err != nil {
		return nil, fmt.Errorf("execArgs: %w", err)
	}
	return argvFromJS(out)
}

func argvFromJS(val goja.Value) ([]string, error) {
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return nil, fmt.Errorf("execArgs must return an argv array")
	}
	raw, ok := val.Export().([]interface{})
	if !ok {
		return nil, fmt.Errorf("execArgs must return an array of strings")
	}
	argv := make([]string, 0, len(raw))
	for _, e := range raw {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("execArgs argv entries must be strings")
		}
		argv = append(argv, s)
	}
	return argv, nil
}

// run executes a command inside the runtime via execArgs, returning the raw
// host result. root selects whether core runs it as a privileged file op.
func (t *ExecTransport) run(ctx context.Context, cmd []string, workDir string, env []string, stdin []byte, root bool) (hostResult, error) {
	argv, err := t.execArgs(cmd, workDir, env, stdin != nil, root)
	if err != nil {
		return hostResult{}, err
	}
	// A nil []byte must map to a nil io.Reader, not a typed-nil *bytes.Reader:
	// os/exec only skips attaching stdin when c.Stdin is a nil interface. A
	// typed nil would let the childStdin goroutine call WriteTo on a nil
	// *bytes.Reader and panic on the first stdin-less Exec.
	var in io.Reader
	if stdin != nil {
		in = bytes.NewReader(stdin)
	}
	return runHostCommand(ctx, argv, in, t.runtime.binaries)
}

func (t *ExecTransport) Exec(ctx context.Context, cmd []string, workDir string, env []string) (*internal.ExecResult, error) {
	start := time.Now()
	internal.Log.Debug("transport_exec_start", "transport", "script:"+t.runtime.Name,
		"native_id", t.nativeID, "command", fmt.Sprintf("%q", cmd), "work_dir", workDir, "env_count", len(env))

	res, err := t.run(ctx, cmd, workDir, env, nil, false)
	if err != nil {
		internal.Log.Debug("transport_exec_failed", "transport", "script:"+t.runtime.Name,
			"native_id", t.nativeID, "error", err)
		return nil, err
	}
	durationMs := time.Since(start).Milliseconds()
	internal.Log.Debug("transport_exec_complete", "transport", "script:"+t.runtime.Name,
		"native_id", t.nativeID, "exit_code", res.ExitCode, "duration_ms", durationMs)

	return &internal.ExecResult{
		Stdout:     res.Stdout,
		Stderr:     res.Stderr,
		ExitCode:   res.ExitCode,
		DurationMs: durationMs,
	}, nil
}

func (t *ExecTransport) SetNetwork(ctx context.Context, enabled bool) error {
	if t.provisioner == nil {
		return nil
	}
	return t.provisioner.setNetwork(ctx, t.handle, enabled)
}

func (t *ExecTransport) CopyDirIn(ctx context.Context, hostSrcDir, destPath string) error {
	if res, err := t.run(ctx, []string{"mkdir", "-p", destPath}, "", nil, nil, true); err != nil {
		return fmt.Errorf("mkdir %s: %w", destPath, err)
	} else if res.ExitCode != 0 {
		return fmt.Errorf("mkdir %s (exit %d): %s", destPath, res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	tmpTar, err := createTarArchive(hostSrcDir)
	if err != nil {
		return fmt.Errorf("create tar archive: %w", err)
	}
	defer os.Remove(tmpTar)

	f, err := os.Open(tmpTar)
	if err != nil {
		return fmt.Errorf("open tar archive: %w", err)
	}
	defer f.Close()

	argv, err := t.execArgs([]string{"tar", "-xf", "-", "-C", destPath}, "", nil, true, true)
	if err != nil {
		return err
	}
	res, err := runHostCommand(ctx, argv, f, t.runtime.binaries)
	if err != nil {
		return fmt.Errorf("stream tar into runtime: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("tar extraction failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func (t *ExecTransport) ReadFile(ctx context.Context, path string) ([]byte, error) {
	res, err := t.run(ctx, []string{"cat", path}, "", nil, nil, true)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("read %s (exit %d): %s", path, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return []byte(res.Stdout), nil
}

func (t *ExecTransport) WriteFile(ctx context.Context, path string, content []byte, mode int64) error {
	if res, err := t.run(ctx, []string{"mkdir", "-p", filepath.Dir(path)}, "", nil, nil, true); err != nil {
		return fmt.Errorf("mkdir parent: %w", err)
	} else if res.ExitCode != 0 {
		return fmt.Errorf("mkdir parent (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	res, err := t.run(ctx, []string{"sh", "-c", `cat > "$1"`, "sh", path}, "", nil, content, true)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("write %s (exit %d): %s", path, res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	if res, err := t.run(ctx, []string{"chmod", fmt.Sprintf("%o", mode), path}, "", nil, nil, true); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	} else if res.ExitCode != 0 {
		return fmt.Errorf("chmod %s (exit %d): %s", path, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func (t *ExecTransport) Bootstrap(ctx context.Context, workDir string) error {
	if t.runtime.transport.bootstrapCmd == nil {
		return nil
	}
	t.mu.Lock()
	vm := t.runtime.transport.vm
	out, err := t.runtime.transport.bootstrapCmd(goja.Undefined(), vm.ToValue(t.handle), vm.ToValue(workDir))
	t.mu.Unlock()
	if err != nil {
		return fmt.Errorf("bootstrapCmd: %w", err)
	}
	if out == nil || goja.IsUndefined(out) || goja.IsNull(out) {
		return nil
	}
	cmd, err := argvFromJS(out)
	if err != nil {
		return err
	}
	if len(cmd) == 0 {
		return nil
	}
	res, err := t.Exec(ctx, cmd, workDir, nil)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("bootstrap failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// createTarArchive creates a tar archive of srcDir rooted at "." for streaming
// into the runtime. This is core-owned (not gated by the plugin's binary list)
// because it runs on the host, not inside the sandbox.
func createTarArchive(srcDir string) (string, error) {
	tmpFile, err := os.CreateTemp("", "wsd-script-copy-*.tar")
	if err != nil {
		return "", err
	}
	name := tmpFile.Name()
	tmpFile.Close()

	cmd := exec.Command("tar", "-cf", name, "-C", srcDir, ".")
	if err := cmd.Run(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}
