package script

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/ipam"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
	"github.com/dop251/goja"
)

// Reserved option flags that, when declared by a plugin, configure the
// shared IPAM pool in core. Keeping IPAM in core (rather than in plugin state)
// is what makes address allocation survive plugin reloads and be concurrency-
// safe across parallel provisions.
const (
	optionIPBaseNetwork = "ip-base-network"
	optionIPPoolSize    = "ip-pool-size"
)

// Provisioner implements runtime.Provisioner by dispatching lifecycle calls to
// a goja plugin. Each call runs in a fresh VM with a fresh host object, so
// concurrent provisions share only core state (IPAM, options) and never leak
// per-workspace JS state into one another.
type Provisioner struct {
	runtime *Runtime
	options map[string]interface{}
	ipam    *ipam.IPAM
}

func newProvisioner(r *Runtime, options *OptionValues) (*Provisioner, error) {
	obj := options.Object()
	p := &Provisioner{
		runtime: r,
		options: obj,
	}
	if base, ok := obj[optionIPBaseNetwork].(string); ok && base != "" {
		size, _ := obj[optionIPPoolSize].(int64)
		alloc, err := ipam.New(base, int(size))
		if err != nil {
			return nil, fmt.Errorf("invalid IPAM configuration (%s=%q, %s=%d): %w", optionIPBaseNetwork, base, optionIPPoolSize, size, err)
		}
		p.ipam = alloc
	}
	// The transport functions read the resolved options via host.options.
	if err := r.bindTransportOptions(obj); err != nil {
		internal.Log.Warn("bind_transport_options_failed", "error", err)
	}
	return p, nil
}

var (
	_ runtime.Provisioner          = (*Provisioner)(nil)
	_ runtime.Validator            = (*Provisioner)(nil)
	_ runtime.WorkspaceRecoverer   = (*Provisioner)(nil)
	_ runtime.WorkspaceIDDestroyer = (*Provisioner)(nil)
	_ runtime.OrphanCleaner        = (*Provisioner)(nil)
)

func (p *Provisioner) Name() string { return p.runtime.Name }

func (p *Provisioner) Init(ctx context.Context) error {
	return p.call(ctx, "init", nil)
}

func (p *Provisioner) Validate(ctx context.Context) error {
	return p.call(ctx, "validate", nil)
}

func (p *Provisioner) Provision(ctx context.Context, spec runtime.ProvisionSpec) (*runtime.RuntimeHandle, error) {
	cs, err := p.newCall(ctx)
	if err != nil {
		return nil, err
	}
	defer cs.cleanup()

	handle, err := p.callValue(cs, "provision", specToJS(spec))
	if err != nil {
		cs.unwind()
		return nil, err
	}
	rh, hm, err := handleFromJS(p.runtime, handle)
	if err != nil {
		cs.unwind()
		return nil, err
	}
	rh.Transport = p.newTransport(rh.NativeID, hm)
	return rh, nil
}

func (p *Provisioner) Destroy(ctx context.Context, handle *runtime.RuntimeHandle) error {
	return p.call(ctx, "destroy", handleToJS(handle))
}

func (p *Provisioner) DestroyByID(ctx context.Context, id string) error {
	return p.call(ctx, "destroyByID", id)
}

func (p *Provisioner) DestroyWorkspaceByID(ctx context.Context, workspaceID string) error {
	return p.call(ctx, "destroyWorkspaceByID", workspaceID)
}

func (p *Provisioner) RecoverHandle(ctx context.Context, nativeID string, workDir string) (*runtime.RuntimeHandle, error) {
	cs, err := p.newCall(ctx)
	if err != nil {
		return nil, err
	}
	defer cs.cleanup()

	val, err := p.callValue(cs, "recover", nativeID, workDir)
	if err != nil {
		return nil, err
	}
	rh, hm, err := handleFromJS(p.runtime, val)
	if err != nil {
		return nil, err
	}
	rh.Transport = p.newTransport(rh.NativeID, hm)
	return rh, nil
}

func (p *Provisioner) newTransport(nativeID string, handle map[string]interface{}) *ExecTransport {
	return &ExecTransport{runtime: p.runtime, provisioner: p, nativeID: nativeID, handle: handle}
}

// setNetwork is a lifecycle function (not a pure argv mapping) so the plugin
// can drive host.run directly and tolerate the idempotency quirks that a pure
// argv function cannot (e.g. docker network connect on an already-attached
// container).
func (p *Provisioner) setNetwork(ctx context.Context, handle map[string]interface{}, enabled bool) error {
	cs, err := p.newCall(ctx)
	if err != nil {
		return err
	}
	defer cs.cleanup()
	_, err = p.callValue(cs, "setNetwork", handle, enabled)
	return err
}

func (p *Provisioner) CleanupOrphans(ctx context.Context, keep map[string]bool) (int, error) {
	names := make([]string, 0, len(keep))
	for name, live := range keep {
		if live {
			names = append(names, name)
		}
	}
	cs, err := p.newCall(ctx)
	if err != nil {
		return 0, err
	}
	defer cs.cleanup()
	val, err := p.callValue(cs, "cleanupOrphans", names)
	if err != nil {
		return 0, err
	}
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return 0, nil
	}
	n, _ := val.Export().(int64)
	return int(n), nil
}

// --- dispatch ---

func (p *Provisioner) newCall(ctx context.Context) (*callState, error) {
	// Never fall back to a shared temp root here: callState.cleanup does an
	// os.RemoveAll(scratchRoot), so a fallback to os.TempDir() would delete the
	// host's entire temp directory on any provision/exec after MkdirTemp fails.
	scratch, err := os.MkdirTemp("", "wsd-plugin-*")
	if err != nil {
		return nil, fmt.Errorf("create plugin scratch dir: %w", err)
	}
	return &callState{
		ctx:         ctx,
		options:     p.options,
		binaries:    p.runtime.binaries,
		ipam:        p.ipam,
		scratchRoot: scratch,
	}, nil
}

// call invokes an optional lifecycle function that returns nothing of interest
// (init, validate, destroy, destroyByID, destroyWorkspaceByID). A missing
// function is a no-op so a plugin may omit optional steps.
func (p *Provisioner) call(ctx context.Context, name string, args ...interface{}) error {
	cs, err := p.newCall(ctx)
	if err != nil {
		return err
	}
	defer cs.cleanup()
	_, err = p.callValue(cs, name, args...)
	if err != nil {
		cs.unwind()
	}
	return err
}

// callValue runs the named lifecycle function in a fresh VM, converting the
// result. A missing function returns an error naming it (except where callers
// treat the function as optional, which they check before calling).
func (p *Provisioner) callValue(cs *callState, name string, args ...interface{}) (goja.Value, error) {
	hostObj := makeHostObject(cs)
	val, vm, err := p.runtime.newInstance(hostObj)
	if err != nil {
		return nil, err
	}
	cs.vm = vm
	obj := val.ToObject(vm)

	fnVal := obj.Get(name)
	if fnVal == nil || goja.IsUndefined(fnVal) || goja.IsNull(fnVal) {
		return nil, nil
	}
	fn, ok := goja.AssertFunction(fnVal)
	if !ok {
		return nil, fmt.Errorf("plugin field %q must be a function", name)
	}

	// Wire context cancellation into the VM so a JS poll loop or a plugin bug
	// cannot outlive the request that started it.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctxDone(cs.ctx):
			vm.Interrupt(cs.ctx.Err())
		case <-stop:
		}
	}()

	jsArgs := make([]goja.Value, 0, len(args))
	for _, a := range args {
		jsArgs = append(jsArgs, vm.ToValue(a))
	}

	out, err := fn(goja.Undefined(), jsArgs...)
	if err != nil {
		var ie *goja.InterruptedError
		if errors.As(err, &ie) {
			if cs.ctx.Err() != nil {
				return nil, cs.ctx.Err()
			}
		}
		return nil, fmt.Errorf("plugin %s: %w", name, err)
	}
	return out, nil
}

// unwind replays the plugin's registered undo steps in reverse order,
// best-effort, on a context detached from the failed call so cancellation of
// the original request cannot skip cleanup.
func (cs *callState) unwind() {
	if len(cs.undo) == 0 {
		return
	}
	undoCtx, cancel := context.WithTimeout(context.WithoutCancel(cs.ctx), 30*time.Second)
	defer cancel()
	cleanup := &callState{
		ctx:      undoCtx,
		options:  cs.options,
		binaries: cs.binaries,
		ipam:     cs.ipam,
	}
	for i := len(cs.undo) - 1; i >= 0; i-- {
		if _, err := cleanup.run(cs.undo[i], nil); err != nil {
			internal.Log.Warn("undo_step_failed", "argv", redactCommand(cs.undo[i]), "error", err)
		}
	}
}

// --- conversion helpers ---

func specToJS(spec runtime.ProvisionSpec) map[string]interface{} {
	return map[string]interface{}{
		"workspace_id":        spec.WorkspaceID,
		"image":               spec.Image,
		"work_dir":            spec.WorkDir,
		"cache_mount_path":    spec.CacheMountPath,
		"workspace_base_path": spec.WorkspaceBasePath,
		"interface":           spec.Interface,
		"ip4_address":         spec.IP4Address,
	}
}

func handleToJS(handle *runtime.RuntimeHandle) map[string]interface{} {
	return map[string]interface{}{
		"native_id": handle.NativeID,
		"work_dir":  handle.WorkDir,
		"username":  handle.Username,
		"privilege": string(handle.Privilege),
	}
}

// handleFromJS converts a plugin-returned handle object into a RuntimeHandle
// and the raw handle map (which the transport passes back to execArgs so the
// plugin can carry per-workspace connection details beyond the four fixed
// fields: ip, port, user, vnic, network mode, ...).
func handleFromJS(r *Runtime, val goja.Value) (*runtime.RuntimeHandle, map[string]interface{}, error) {
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return nil, nil, fmt.Errorf("plugin %s: provision must return a handle object", r.Name)
	}
	m, ok := val.Export().(map[string]interface{})
	if !ok {
		return nil, nil, fmt.Errorf("plugin %s: handle must be an object", r.Name)
	}

	handle := &runtime.RuntimeHandle{
		OS:        runtime.OS(r.OS),
		Isolation: runtime.IsolationType(r.Isolation),
		Username:  strFromMap(m, "username"),
		WorkDir:   strFromMap(m, "work_dir"),
		NativeID:  strFromMap(m, "native_id"),
		Privilege: runtime.PrivilegeRoot,
	}
	if handle.NativeID == "" {
		return nil, nil, fmt.Errorf("plugin %s: handle must set native_id", r.Name)
	}
	if handle.Username == "" {
		handle.Username = "root"
	}
	// A plugin may override the reported guest OS per handle (e.g. the vmd
	// plugin runs OpenBSD VMs but can host a freebsd or linux guest).
	if osStr := strFromMap(m, "os"); osStr != "" {
		handle.OS = runtime.OS(osStr)
	}
	switch strFromMap(m, "privilege") {
	case "user":
		handle.Privilege = runtime.PrivilegeUser
	case "", "root":
		handle.Privilege = runtime.PrivilegeRoot
	}
	return handle, m, nil
}

// ctxDone returns a channel that closes when ctx is done, tolerating nil.
func ctxDone(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		ctx = context.Background()
	}
	return ctx.Done()
}

// exported for tests.
func (p *Provisioner) optionString(name string) string {
	if v, ok := p.options[name].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}
