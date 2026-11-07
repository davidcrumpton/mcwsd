package script

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/ipam"
	"github.com/dop251/goja"
)

// callState is the per-call mutable state behind one lifecycle invocation.
// It is what makes each provision/destroy call independent: its own undo
// stack, its own scratch root, its own context. The host object is rebuilt
// around it for every call.
type callState struct {
	ctx      context.Context
	options  map[string]interface{}
	binaries map[string]bool
	ipam     *ipam.IPAM
	vm       *goja.Runtime

	// scratchRoot is a per-call temporary directory handed to the plugin as
	// its confined fs root. It is removed when the call finishes.
	scratchRoot string

	// undo holds argv slices the plugin registered with host.onFail. On a
	// failed provision, core replays them in reverse order (best-effort).
	undo [][]string
}

func (c *callState) cleanup() {
	if c.scratchRoot != "" {
		_ = os.RemoveAll(c.scratchRoot)
	}
}

// makeHostObject builds the constrained host API for one call.
func makeHostObject(c *callState) map[string]interface{} {
	return map[string]interface{}{
		"options": c.options,
		"ipam": map[string]interface{}{
			"allocate": func(workspaceID string) (string, error) { return c.requireIPAM().Allocate(workspaceID) },
			"release":  func(workspaceID string) { c.releaseIPAM(workspaceID) },
			"reserve":  func(workspaceID, addr string) error { return c.requireIPAM().Reserve(workspaceID, addr) },
			"gateway": func() string {
				gw := c.requireIPAM().Gateway()
				if gw == nil {
					return ""
				}
				return gw.String()
			},
			"network": func() string {
				if c.ipam == nil {
					return ""
				}
				return c.ipam.Network()
			},
		},
		"run": func(argv []string, opts map[string]interface{}) (map[string]interface{}, error) {
			return c.run(argv, opts)
		},
		"sleep": func(ms int) error { return c.sleep(ms) },
		"log": func(level int, msg string) {
			c.log(level, msg)
		},
		"env": func(name string) string { return os.Getenv(name) },
		"onFail": func(argv []string) {
			if len(argv) > 0 {
				c.undo = append(c.undo, append([]string(nil), argv...))
			}
		},
		"fetch": func(url string, opts map[string]interface{}) (string, error) {
			return c.fetch(url, opts)
		},
		"fs": map[string]interface{}{
			"write": func(path string, content string) error { return c.fsWrite(path, content) },
			"read":  func(path string) (string, error) { return c.fsRead(path) },
		},
	}
}

func (c *callState) requireIPAM() *ipam.IPAM {
	return c.ipam
}

func (c *callState) releaseIPAM(workspaceID string) {
	if c.ipam != nil {
		c.ipam.Release(workspaceID)
	}
}

func (c *callState) log(level int, msg string) {
	// Levels mirror mcphe: 1=warn, 2=info, 3=error, 4=debug.
	switch {
	case level <= 1:
		internal.Log.Warn(msg)
	case level >= 3:
		internal.Log.Error(msg)
	default:
		internal.Log.Info(msg)
	}
}

func (c *callState) sleep(ms int) error {
	if ms <= 0 {
		return nil
	}
	select {
	case <-c.ctx.Done():
		return c.ctx.Err()
	case <-time.After(time.Duration(ms) * time.Millisecond):
		return nil
	}
}

// run executes argv on the host, enforcing the plugin's allowed-binary list.
// A non-zero exit is returned in the result, not as an error; only a local
// failure (binary not allowed, not found, or context cancelled) is an error.
func (c *callState) run(argv []string, opts map[string]interface{}) (map[string]interface{}, error) {
	var stdin io.Reader
	if opts != nil {
		if s, ok := opts["stdin"].(string); ok {
			stdin = strings.NewReader(s)
		}
		if stdin == nil {
			if s, ok := opts["input"].(string); ok {
				stdin = strings.NewReader(s)
			}
		}
	}
	res, err := runHostCommand(c.ctx, argv, stdin, c.binaries)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"stdout":    res.Stdout,
		"stderr":    res.Stderr,
		"exit_code": res.ExitCode,
	}, nil
}

func (c *callState) fsWrite(rel string, content string) error {
	root, err := os.OpenRoot(c.scratchRoot)
	if err != nil {
		return fmt.Errorf("host.fs.write: open scratch root: %w", err)
	}
	defer root.Close()
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, "/") {
		rel = strings.TrimLeft(rel, "/")
	}
	if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil && !os.IsExist(err) {
		return fmt.Errorf("host.fs.write: mkdir: %w", err)
	}
	return root.WriteFile(rel, []byte(content), 0o644)
}

func (c *callState) fsRead(rel string) (string, error) {
	root, err := os.OpenRoot(c.scratchRoot)
	if err != nil {
		return "", fmt.Errorf("host.fs.read: open scratch root: %w", err)
	}
	defer root.Close()
	rel = filepath.ToSlash(rel)
	rel = strings.TrimLeft(rel, "/")
	data, err := root.ReadFile(rel)
	if err != nil {
		return "", fmt.Errorf("host.fs.read: %w", err)
	}
	return string(data), nil
}

func (c *callState) fetch(url string, opts map[string]interface{}) (string, error) {
	// Fetch is used by jail/zone templates. It downloads to the scratch root
	// and returns an absolute path the plugin can hand to host.run (e.g. tar).
	dest, err := os.CreateTemp(c.scratchRoot, "fetch-*")
	if err != nil {
		return "", err
	}
	name := dest.Name()
	defer dest.Close()

	reqCtx := c.ctx
	var cancel context.CancelFunc
	if _, hasTimeout := opts["timeout_seconds"]; hasTimeout {
		if v, ok := opts["timeout_seconds"].(int64); ok && v > 0 {
			reqCtx, cancel = context.WithTimeout(c.ctx, time.Duration(v)*time.Second)
			defer cancel()
		}
	}
	if err := downloadTo(reqCtx, url, name); err != nil {
		return "", err
	}
	return name, nil
}

// redactCommand renders argv for an error message without leaking values.
func redactCommand(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = quoteArg(a)
	}
	return strings.Join(parts, " ")
}

func quoteArg(arg string) string {
	if arg == "" {
		return "''"
	}
	if !strings.ContainsAny(arg, " \t\n\"'\\$&|;<>()*?[]{}!#~`") {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}
