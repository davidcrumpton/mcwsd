// Package script implements a goja-backed runtime plugin host. A single
// TypeScript-authored plugin (compiled to a CommonJS .js file) supplies both
// the lifecycle (provision/destroy/recover/orphans) and the transport
// (execArgs: an argv -> host-argv pure function) for one runtime.
//
// The design contract is a single plugin per daemon instance: an operator runs
// one `mcwsd daemon --plugin <path>` per runtime/account, never several at
// once. Shared state (IPAM, options, the undo stack) lives in core, not in the
// plugin, and each lifecycle call gets a fresh VM so concurrent provisions
// cannot leak state into one another.
package script

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dop251/goja"
)

// Runtime is a loaded plugin: source, metadata, and the pure transport
// callables extracted into a long-lived VM.
type Runtime struct {
	Path        string
	Name        string
	Description string
	Version     string
	OS          string
	Isolation   string
	Options     []Option
	Binaries    []string
	binaries    map[string]bool

	src string

	// transport holds the pure argv-mapping functions. It is a separate VM
	// from the per-call lifecycle VMs because those functions must remain
	// cheap to invoke on every Exec (the hot path), and they are required to
	// be pure: no host.* side effects, no per-call state. SetNetwork is NOT
	// here — it is stateful and uses host.run, so it is a lifecycle function.
	transport struct {
		vm           *goja.Runtime
		execArgs     goja.Callable
		bootstrapCmd goja.Callable
	}
}

// Load reads and validates the plugin at path, extracting its metadata and
// transport callables. It runs no lifecycle function.
func Load(path string) (*Runtime, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read plugin: %w", err)
	}

	r := &Runtime{Path: path, src: string(src)}

	// First pass extracts metadata before the host object is fully formed
	// (options and binaries feed flag binding and the allowed-binary gate).
	probe, probeVM, err := r.newInstance(nil)
	if err != nil {
		return nil, fmt.Errorf("load plugin: %w", err)
	}
	obj := probe.ToObject(probeVM)

	if r.Name, err = stringField(obj, "name"); err != nil {
		return nil, err
	}
	if r.Name == "" {
		return nil, fmt.Errorf("plugin %s must export a non-empty name", path)
	}
	r.Description = stringOrEmpty(obj, "description")
	r.Version = stringOrEmpty(obj, "version")
	if r.OS = stringOrEmpty(obj, "os"); r.OS == "" {
		return nil, fmt.Errorf("plugin %s must export an os (e.g. \"linux\")", path)
	}
	if r.Isolation = stringOrEmpty(obj, "isolation"); r.Isolation == "" {
		return nil, fmt.Errorf("plugin %s must export an isolation (e.g. \"container\")", path)
	}

	if r.Options, err = optionSlice(obj); err != nil {
		return nil, err
	}
	if r.Binaries, err = stringSliceField(obj, "binaries"); err != nil {
		return nil, err
	}
	r.binaries = make(map[string]bool, len(r.Binaries))
	for _, b := range r.Binaries {
		r.binaries[b] = true
	}

	// Extract the transport callables into a long-lived VM whose host object
	// is fixed (no per-call context), so they cannot observe per-call state.
	inst, instVM, err := r.newInstance(nil)
	if err != nil {
		return nil, err
	}
	tobj := inst.ToObject(instVM)
	if r.transport.execArgs, err = requireFunction(tobj, "execArgs"); err != nil {
		return nil, err
	}
	r.transport.bootstrapCmd, _ = optionalFunction(tobj, "bootstrapCmd")
	r.transport.vm = instVM

	return r, nil
}

// NewProvisioner returns a Provisioner bound to this plugin with the given
// parsed option values.
func (r *Runtime) NewProvisioner(options *OptionValues) *Provisioner {
	return newProvisioner(r, options)
}

func (r *Runtime) newTransport(nativeID string, handle map[string]interface{}) *ExecTransport {
	return &ExecTransport{runtime: r, nativeID: nativeID, handle: handle}
}

// bindTransportOptions binds the resolved plugin options into the transport VM
// after flag parsing. execArgs/bootstrapCmd are pure functions of their args
// plus the immutable daemon options, so this is set once, before any
// concurrent exec, and never changed.
func (r *Runtime) bindTransportOptions(options map[string]interface{}) error {
	return r.transport.vm.Set("host", map[string]interface{}{"options": options})
}

// newInstance runs the plugin in a fresh VM with hostObj bound as the global
// `host`, returning the module.exports value and the VM that owns it.
func (r *Runtime) newInstance(hostObj interface{}) (goja.Value, *goja.Runtime, error) {
	vm := goja.New()
	if hostObj != nil {
		if err := vm.Set("host", hostObj); err != nil {
			return nil, nil, fmt.Errorf("bind host: %w", err)
		}
	}
	script := "var module = { exports: {} }; var exports = module.exports;\n" + r.src + "\nmodule.exports"
	val, err := vm.RunString(script)
	if err != nil {
		return nil, nil, fmt.Errorf("run plugin: %w", err)
	}
	return val, vm, nil
}

// --- extraction helpers ---

func stringField(obj *goja.Object, name string) (string, error) {
	v := obj.Get(name)
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return "", nil
	}
	s, ok := v.Export().(string)
	if !ok {
		return "", fmt.Errorf("plugin field %q must be a string", name)
	}
	return s, nil
}

func stringOrEmpty(obj *goja.Object, name string) string {
	s, _ := stringField(obj, name)
	return s
}

func stringSliceField(obj *goja.Object, name string) ([]string, error) {
	v := obj.Get(name)
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, nil
	}
	raw, ok := v.Export().([]interface{})
	if !ok {
		return nil, fmt.Errorf("plugin field %q must be an array of strings", name)
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("plugin field %q must be an array of strings", name)
		}
		out = append(out, s)
	}
	return out, nil
}

func optionSlice(obj *goja.Object) ([]Option, error) {
	v := obj.Get("options")
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, nil
	}
	raw, ok := v.Export().([]interface{})
	if !ok {
		return nil, fmt.Errorf("plugin field \"options\" must be an array")
	}
	out := make([]Option, 0, len(raw))
	for _, e := range raw {
		m, ok := e.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("plugin option entries must be objects")
		}
		o := Option{
			Flag:    strFromMap(m, "flag"),
			Type:    strFromMap(m, "type"),
			Default: strFromMap(m, "default"),
			Usage:   strFromMap(m, "usage"),
		}
		if o.Type == "" {
			o.Type = "string"
		}
		if err := validateOption(o); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

func strFromMap(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func requireFunction(obj *goja.Object, name string) (goja.Callable, error) {
	v := obj.Get(name)
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, fmt.Errorf("plugin %q must export a %s function", stringOrEmpty(obj, "name"), name)
	}
	fn, ok := goja.AssertFunction(v)
	if !ok {
		return nil, fmt.Errorf("plugin field %q must be a function", name)
	}
	return fn, nil
}

func optionalFunction(obj *goja.Object, name string) (goja.Callable, bool) {
	v := obj.Get(name)
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, false
	}
	fn, ok := goja.AssertFunction(v)
	return fn, ok
}

func pluginFileName(path string) string {
	return filepath.Base(strings.TrimSuffix(path, ".js"))
}
