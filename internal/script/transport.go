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
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
	"github.com/dop251/goja"
)

// ExecTransport implements runtime.ExecTransport generically on top of the
// plugin's execArgs pure function. Core owns ReadFile, WriteFile, CopyDirIn and
// Bootstrap; the plugin only describes how to turn (handle, argv, workDir, env)
// into the host command line that runs it.
//
// Note: the goja VM used by execArgs/bootstrapCmd is shared by every handle in
// the process, so serialization lives on Runtime.transport.mu, not here.
type ExecTransport struct {
	runtime     *Runtime
	provisioner *Provisioner
	nativeID    string
	handle      map[string]interface{}
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
// execArgs invokes the plugin's execArgs(handle, cmd, workDir, env, opts) and
// returns the resulting argv plus an optional stdin payload. opts carries
// {stdin, root}: core sets root=true for its own privileged file I/O
// (ReadFile/WriteFile/CopyDirIn) and false for user Exec, so a plugin can run
// file operations as root while leaving user commands at the image's default
// user.
//
// A plugin may return either a plain argv array or {argv, stdin}; the stdin
// form exists for runtimes that stream the command script on stdin rather than
// embedding it in argv (e.g. zlogin's `sh -s`, which avoids su -c argument
// mangling). When the plugin supplies a stdin payload it overrides the data
// stdin core would otherwise stream.
func (t *ExecTransport) execArgs(cmd []string, workDir string, env []string, stdin, root bool) ([]string, *string, error) {
	t.runtime.transport.mu.Lock()
	defer t.runtime.transport.mu.Unlock()
	vm := t.runtime.transport.vm
	out, err := t.runtime.transport.execArgs(goja.Undefined(),
		vm.ToValue(t.handle),
		vm.ToValue(cmd),
		vm.ToValue(workDir),
		vm.ToValue(env),
		vm.ToValue(map[string]interface{}{"stdin": stdin, "root": root}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("execArgs: %w", err)
	}
	return argvResultFromJS(out)
}

func argvFromJS(val goja.Value) ([]string, error) {
	argv, _, err := argvResultFromJS(val)
	return argv, err
}

// argvResultFromJS converts an execArgs return value into an argv and an
// optional stdin payload. The plain array form (no stdin) is used by argv-only
// runtimes; the {argv, stdin} object form is used by stdin-streaming runtimes.
func argvResultFromJS(val goja.Value) ([]string, *string, error) {
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return nil, nil, fmt.Errorf("execArgs must return an argv array or {argv, stdin}")
	}
	if raw, ok := val.Export().([]interface{}); ok {
		argv, err := argvSlice(raw)
		return argv, nil, err
	}
	if m, ok := val.Export().(map[string]interface{}); ok {
		raw, ok := m["argv"].([]interface{})
		if !ok {
			return nil, nil, fmt.Errorf("execArgs object form must include an argv array")
		}
		argv, err := argvSlice(raw)
		if err != nil {
			return nil, nil, err
		}
		var stdin *string
		if s, ok := m["stdin"].(string); ok {
			stdin = &s
		}
		return argv, stdin, nil
	}
	return nil, nil, fmt.Errorf("execArgs must return an array of strings or {argv, stdin}")
}

func argvSlice(raw []interface{}) ([]string, error) {
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
	argv, cmdStdin, err := t.execArgs(cmd, workDir, env, stdin != nil, root)
	if err != nil {
		return hostResult{}, err
	}
	// A nil []byte must map to a nil io.Reader, not a typed-nil *bytes.Reader:
	// os/exec only skips attaching stdin when c.Stdin is a nil interface. A
	// typed nil would let the childStdin goroutine call WriteTo on a nil
	// *bytes.Reader and panic on the first stdin-less Exec.
	var in io.Reader
	if cmdStdin != nil {
		in = strings.NewReader(*cmdStdin)
	} else if stdin != nil {
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

	argv, _, err := t.execArgs([]string{"tar", "-xf", "-", "-C", destPath}, "", nil, true, true)
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

// maxReadFileBytes caps a single file read pulled out of a runtime. It keeps a
// huge repo file (or a pathological bundle) from exhausting daemon memory.
const maxReadFileBytes = 64 << 20

func (t *ExecTransport) ReadFile(ctx context.Context, path string) ([]byte, error) {
	// Read one byte past the cap so an oversized file is detected without
	// pulling the whole thing into memory. The path is passed as an argument
	// to sh so it is never interpreted as an option.
	script := fmt.Sprintf(`head -c %d "$1"`, maxReadFileBytes+1)
	res, err := t.run(ctx, []string{"sh", "-c", script, "sh", path}, "", nil, nil, true)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("read %s (exit %d): %s", path, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if len(res.Stdout) > maxReadFileBytes {
		return nil, fmt.Errorf("read %s: file exceeds %d-byte limit", path, maxReadFileBytes)
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

	// mode 0 means "preserve the existing file's mode". Redirection above
	// truncates the file in place, so its permission bits (including exec)
	// survive; explicitly chmod-ing to 0644 would silently drop them.
	if mode == 0 {
		return nil
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
	t.runtime.transport.mu.Lock()
	vm := t.runtime.transport.vm
	out, err := t.runtime.transport.bootstrapCmd(goja.Undefined(), vm.ToValue(t.handle), vm.ToValue(workDir))
	t.runtime.transport.mu.Unlock()
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
