package openbsd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
)

func TestNewVMDProvisioner_Defaults(t *testing.T) {
	p := NewVMDProvisioner("", "", "", "", "", 0, "")
	if p.vmRoot != defaultVMRoot {
		t.Errorf("vmRoot = %q, want %q", p.vmRoot, defaultVMRoot)
	}
	if p.memory != defaultMemory {
		t.Errorf("memory = %q, want %q", p.memory, defaultMemory)
	}
	if p.loginUser != defaultLoginUser {
		t.Errorf("loginUser = %q, want %q", p.loginUser, defaultLoginUser)
	}
	if p.sshPort != defaultSSHPort {
		t.Errorf("sshPort = %d, want %d", p.sshPort, defaultSSHPort)
	}
	if p.guestOS != runtime.OSOpenBSD {
		t.Errorf("guestOS = %q, want %q", p.guestOS, runtime.OSOpenBSD)
	}
	if p.masterImage != filepath.Join(defaultVMRoot, masterDiskName) {
		t.Errorf("masterImage = %q, want it under vmRoot", p.masterImage)
	}
	if p.probeTimeout != defaultSSHProbeTimeout {
		t.Errorf("probeTimeout = %s, want %s", p.probeTimeout, defaultSSHProbeTimeout)
	}
}

func TestNewVMDProvisioner_Custom(t *testing.T) {
	p := NewVMDProvisioner("/vms", "/vms/base.qcow2", "2G", "wsd", "/keys/k", 2222, "freebsd")
	if p.vmRoot != "/vms" || p.masterImage != "/vms/base.qcow2" || p.memory != "2G" ||
		p.loginUser != "wsd" || p.sshKeyPath != "/keys/k" || p.sshPort != 2222 || p.guestOS != runtime.OSFreeBSD {
		t.Fatalf("custom fields not preserved: %+v", p)
	}
}

func TestVMDProvisioner_ImplementsInterfaces(t *testing.T) {
	var _ runtime.Provisioner = (*VMDProvisioner)(nil)
	var _ runtime.Validator = (*VMDProvisioner)(nil)
	var _ runtime.OrphanCleaner = (*VMDProvisioner)(nil)
}

func TestParseVMID(t *testing.T) {
	id, err := parseVMID("vmctl: started vm 6 successfully, tty /dev/ttyp3\n")
	if err != nil {
		t.Fatalf("parseVMID: %v", err)
	}
	if id != 6 {
		t.Fatalf("parseVMID = %d, want 6", id)
	}

	if _, err := parseVMID("vmctl: something else"); err == nil {
		t.Fatal("parseVMID should fail when the success line is absent")
	}
}

// TestParseVMIDByName guards the recovery path: the numeric vmd ID must come
// from vmctl status, not from the VM name (which contains the workspace ID).
func TestParseVMIDByName(t *testing.T) {
	status := "   ID   PID VCPUS  MAXMEM  CURMEM     TTY        OWNER    STATE NAME\n" +
		"    6 12345     1    1.0G    200M     ttyp3       root   running wsd-ws-other\n" +
		"    7 23456     1    1.0G    200M     ttyp4       root   running wsd-ws-abc\n"

	id, err := parseVMIDByName(status, "wsd-ws-abc")
	if err != nil {
		t.Fatalf("parseVMIDByName: %v", err)
	}
	if id != 7 {
		t.Fatalf("parseVMIDByName = %d, want 7", id)
	}

	if _, err := parseVMIDByName(status, "wsd-ws-missing"); err == nil {
		t.Fatal("parseVMIDByName should fail when the VM has no status row")
	}
}

// TestRecoverHandleUsesRunningVMID verifies recovery derives the guest address
// from the running VM's numeric vmd ID.
func TestRecoverHandleUsesRunningVMID(t *testing.T) {
	p, runner, _ := newTestProvisioner(t)
	vmName := "wsd-ws-abc"
	if err := os.MkdirAll(p.vmDiskDir(vmName), 0755); err != nil {
		t.Fatal(err)
	}
	runner.outputs["vmctl status"] = "   ID   PID VCPUS  MAXMEM  CURMEM     TTY        OWNER    STATE NAME\n" +
		"    7 23456     1    1.0G    200M     ttyp4       root   running " + vmName + "\n"

	handle, err := p.RecoverHandle(context.Background(), vmName, "/workspace/repo")
	if err != nil {
		t.Fatalf("RecoverHandle: %v", err)
	}
	transport, ok := handle.Transport.(*SSHExecTransport)
	if !ok {
		t.Fatalf("Transport = %T, want *SSHExecTransport", handle.Transport)
	}
	if transport.ip != "100.64.7.3" {
		t.Errorf("transport ip = %q, want 100.64.7.3", transport.ip)
	}
	if handle.NativeID != vmName {
		t.Errorf("NativeID = %q, want %q", handle.NativeID, vmName)
	}
}

// TestRecoverHandleNotRunning verifies recovery refuses a VM that has a disk
// but no running process, instead of inventing an address from its name.
func TestRecoverHandleNotRunning(t *testing.T) {
	p, runner, _ := newTestProvisioner(t)
	vmName := "wsd-ws-stopped"
	if err := os.MkdirAll(p.vmDiskDir(vmName), 0755); err != nil {
		t.Fatal(err)
	}
	runner.outputs["vmctl status"] = "   ID   PID VCPUS  MAXMEM  CURMEM     TTY        OWNER    STATE NAME\n"

	if _, err := p.RecoverHandle(context.Background(), vmName, "/workspace/repo"); err == nil {
		t.Fatal("RecoverHandle should fail when the VM is not running")
	}
}

func TestLocalGuestIP(t *testing.T) {
	if got := localGuestIP(6); got != "100.64.6.3" {
		t.Fatalf("localGuestIP(6) = %q, want 100.64.6.3", got)
	}
}

// TestLocalHostIP guards the distinction between the host's own tap endpoint
// (.2) and the guest (.3): the host address must never be used as the VM.
func TestLocalHostIP(t *testing.T) {
	if got := localHostIP(6); got != "100.64.6.2" {
		t.Fatalf("localHostIP(6) = %q, want 100.64.6.2", got)
	}
	if localHostIP(6) == localGuestIP(6) {
		t.Fatal("host and guest addresses must differ")
	}
}

func TestValidateWorkspaceIDAndVMName(t *testing.T) {
	valid := []string{"ws-abc123", "ws_1.2", "a"}
	for _, id := range valid {
		if err := validateWorkspaceID(id); err != nil {
			t.Errorf("validateWorkspaceID(%q) = %v, want nil", id, err)
		}
	}
	invalid := []string{"", "..", "../etc", "a/b", `a\b`, "-leading"}
	for _, id := range invalid {
		if err := validateWorkspaceID(id); err == nil {
			t.Errorf("validateWorkspaceID(%q) = nil, want error", id)
		}
	}

	if err := validateVMName("wsd-ws-abc"); err != nil {
		t.Errorf("validateVMName(wsd-ws-abc) = %v, want nil", err)
	}
	for _, name := range []string{"", "other", "wsd-../x", "wsd-a/b"} {
		if err := validateVMName(name); err == nil {
			t.Errorf("validateVMName(%q) = nil, want error", name)
		}
	}
}

// TestProvisionFlow verifies the ordered lifecycle: COW clone, then start,
// then readiness wait, and that the returned handle carries the predictable
// local-interface address.
func TestProvisionFlow(t *testing.T) {
	p, runner, rec := newTestProvisioner(t)

	handle, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID: "ws-abc",
		WorkDir:     "/workspace/repo",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if handle.NativeID != "wsd-ws-abc" {
		t.Errorf("NativeID = %q, want wsd-ws-abc", handle.NativeID)
	}
	if handle.Isolation != runtime.IsolationVM {
		t.Errorf("Isolation = %q, want vm", handle.Isolation)
	}
	if handle.OS != runtime.OSOpenBSD {
		t.Errorf("OS = %q, want openbsd", handle.OS)
	}
	if handle.WorkDir != "/workspace/repo" {
		t.Errorf("WorkDir = %q", handle.WorkDir)
	}

	transport, ok := handle.Transport.(*SSHExecTransport)
	if !ok {
		t.Fatalf("Transport = %T, want *SSHExecTransport", handle.Transport)
	}
	if transport.ip != "100.64.6.3" {
		t.Errorf("transport ip = %q, want 100.64.6.3", transport.ip)
	}

	cloneIdx := rec.indexOfPrefix("run:vmctl create -b")
	startIdx := rec.indexOfPrefix("run:vmctl start")
	if cloneIdx == -1 || startIdx == -1 {
		t.Fatalf("clone/start not both recorded: %v", rec.events)
	}
	if cloneIdx > startIdx {
		t.Errorf("clone (event %d) must precede start (event %d)", cloneIdx, startIdx)
	}
	if !runner.calledWith("-b " + p.masterImage) {
		t.Errorf("clone did not use -b with the master image: %v", runner.calls)
	}
	if !runner.calledWith("-L -B disk") {
		t.Errorf("start did not request a local interface booting from disk: %v", runner.calls)
	}

	diskDir := p.vmDiskDir("wsd-ws-abc")
	if _, err := os.Stat(filepath.Join(diskDir, diskFileName)); err != nil {
		t.Errorf("disk directory not created: %v", err)
	}
}

// TestProvisionStartFailureCleansUp verifies that a failed vmctl start stops
// any partial VM and removes the disk directory, so nothing is leaked.
func TestProvisionStartFailureCleansUp(t *testing.T) {
	p, runner, _ := newTestProvisioner(t)
	runner.failOn = "vmctl start"

	_, err := p.Provision(context.Background(), runtime.ProvisionSpec{WorkspaceID: "ws-fail", WorkDir: "/workspace/repo"})
	if err == nil {
		t.Fatal("Provision should fail when vmctl start fails")
	}
	if !runner.calledWith("vmctl stop -f wsd-ws-fail") {
		t.Errorf("failed provision did not stop the VM: %v", runner.calls)
	}
	if _, statErr := os.Stat(p.vmDiskDir("wsd-ws-fail")); !os.IsNotExist(statErr) {
		t.Errorf("failed provision left the disk directory behind: %v", statErr)
	}
}

// TestProvisionCloneFailure verifies a failed clone cleans up too.
func TestProvisionCloneFailure(t *testing.T) {
	p, runner, _ := newTestProvisioner(t)
	runner.failOn = "vmctl create"

	if _, err := p.Provision(context.Background(), runtime.ProvisionSpec{WorkspaceID: "ws-clone", WorkDir: "/workspace/repo"}); err == nil {
		t.Fatal("Provision should fail when the clone fails")
	}
	if _, statErr := os.Stat(p.vmDiskDir("wsd-ws-clone")); !os.IsNotExist(statErr) {
		t.Errorf("failed clone left the disk directory behind: %v", statErr)
	}
}

// TestProvisionEscalation verifies vmctl operations are routed through the
// configured escalation command.
func TestProvisionEscalation(t *testing.T) {
	p, runner, _ := newTestProvisioner(t)
	p.WithEscalationCommand("doas")

	if _, err := p.Provision(context.Background(), runtime.ProvisionSpec{WorkspaceID: "ws-esc", WorkDir: "/workspace/repo"}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !runner.calledWith("doas vmctl create -b") {
		t.Errorf("clone was not escalated: %v", runner.calls)
	}
	if !runner.calledWith("doas vmctl start") {
		t.Errorf("start was not escalated: %v", runner.calls)
	}
}

// TestProvisionSeparateGuestEscalation verifies the host-side and guest-side
// escalation commands are independent: host vmctl operations use the host
// escalator while commands in the VM use the guest escalator.
func TestProvisionSeparateGuestEscalation(t *testing.T) {
	p, runner, _ := newTestProvisioner(t)
	p.loginUser = "wsd"
	p.WithEscalationCommand("sudo")
	p.WithGuestEscalationCommand("doas")

	handle, err := p.Provision(context.Background(), runtime.ProvisionSpec{WorkspaceID: "ws-esc-sep", WorkDir: "/workspace/repo"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !runner.calledWith("sudo vmctl create -b") {
		t.Errorf("host clone was not escalated with sudo: %v", runner.calls)
	}
	tr, ok := handle.Transport.(*SSHExecTransport)
	if !ok {
		t.Fatalf("transport type = %T, want *SSHExecTransport", handle.Transport)
	}
	if tr.escalationCommand != "doas" {
		t.Errorf("guest escalation = %q, want doas", tr.escalationCommand)
	}
}

func TestDestroyByID(t *testing.T) {
	p, runner, _ := newTestProvisioner(t)
	diskDir := p.vmDiskDir("wsd-ws-destroy")
	if err := os.MkdirAll(diskDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(diskDir, diskFileName), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := p.DestroyByID(context.Background(), "wsd-ws-destroy"); err != nil {
		t.Fatalf("DestroyByID: %v", err)
	}
	if !runner.calledWith("vmctl stop -f wsd-ws-destroy") {
		t.Errorf("DestroyByID did not stop the VM: %v", runner.calls)
	}
	if _, err := os.Stat(diskDir); !os.IsNotExist(err) {
		t.Errorf("DestroyByID did not remove the disk directory: %v", err)
	}
}

func TestDestroyWorkspaceByID(t *testing.T) {
	p, runner, _ := newTestProvisioner(t)
	workspaceID := "ws-destroy-workspace"
	vmName := vmNamePrefix + workspaceID
	diskDir := p.vmDiskDir(vmName)
	if err := os.MkdirAll(diskDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(diskDir, diskFileName), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := p.DestroyWorkspaceByID(context.Background(), workspaceID); err != nil {
		t.Fatalf("DestroyWorkspaceByID: %v", err)
	}
	if !runner.calledWith("vmctl stop -f " + vmName) {
		t.Errorf("DestroyWorkspaceByID did not stop the VM: %v", runner.calls)
	}
	if _, err := os.Stat(diskDir); !os.IsNotExist(err) {
		t.Errorf("DestroyWorkspaceByID did not remove the disk directory: %v", err)
	}
}

// TestDestroyByIDBenign verifies a VM that is already gone is not an error.
func TestDestroyByIDBenign(t *testing.T) {
	p, runner, _ := newTestProvisioner(t)
	runner.failOn = "vmctl stop"
	runner.failErr = errString("vmctl: VM wsd-ws-gone not running")

	if err := p.DestroyByID(context.Background(), "wsd-ws-gone"); err != nil {
		t.Fatalf("DestroyByID should tolerate an already-stopped VM: %v", err)
	}
}

// TestCleanupOrphans verifies untracked wsd-* disk directories are removed
// while tracked ones, non-wsd directories and the master image file are left
// alone.
func TestCleanupOrphans(t *testing.T) {
	p, _, _ := newTestProvisioner(t)

	mkVM := func(name string) {
		dir := p.vmDiskDir(name)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, diskFileName), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	mkVM("wsd-orphan")
	mkVM("wsd-live")
	// A non-wsd directory with a disk must be untouched.
	if err := os.MkdirAll(filepath.Join(p.vmRoot, "unrelated"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.vmRoot, "unrelated", diskFileName), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	removed, err := p.CleanupOrphans(context.Background(), map[string]bool{"wsd-live": true})
	if err != nil {
		t.Fatalf("CleanupOrphans: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(p.vmDiskDir("wsd-orphan")); !os.IsNotExist(err) {
		t.Errorf("orphan was not removed: %v", err)
	}
	if _, err := os.Stat(p.vmDiskDir("wsd-live")); err != nil {
		t.Errorf("live VM was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.vmRoot, "unrelated")); err != nil {
		t.Errorf("unrelated directory was removed: %v", err)
	}
	if _, err := os.Stat(p.masterImage); err != nil {
		t.Errorf("master image was removed: %v", err)
	}
}

// TestValidateMissingMasterImage verifies Validate fails fast when the golden
// image is absent.
func TestValidateMissingMasterImage(t *testing.T) {
	p, _, _ := newTestProvisioner(t)
	if err := os.Remove(p.masterImage); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(context.Background()); err == nil {
		t.Fatal("Validate should fail when the master image is missing")
	}
}

func TestValidateEmptyMasterImage(t *testing.T) {
	p, _, _ := newTestProvisioner(t)
	if err := os.WriteFile(p.masterImage, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(context.Background()); err == nil {
		t.Fatal("Validate should fail when the master image is empty")
	}
}

// TestValidateNoVMsIsOK guards the common startup case: vmctl(8) exits
// non-zero when there are no VMs to list (print_vm_info returns ENOENT), but
// vmd(8) answered, so Validate must not treat it as a failure. This is the
// normal state on a freshly started daemon.
func TestValidateNoVMsIsOK(t *testing.T) {
	p, runner, _ := newTestProvisioner(t)
	header := "   ID   PID VCPUS  MAXMEM  CURMEM     TTY        OWNER    STATE NAME\n"
	runner.outputs["vmctl status"] = header
	runner.failOn = "vmctl status"
	runner.failErr = errString("exit status 1: " + strings.TrimSpace(header))

	if err := p.Validate(context.Background()); err != nil {
		t.Fatalf("Validate should accept a reachable vmd with no VMs: %v", err)
	}
}

// TestValidateVMDUnreachable verifies a genuine failure (no table header, just
// a connection error) is fatal.
func TestValidateVMDUnreachable(t *testing.T) {
	p, runner, _ := newTestProvisioner(t)
	runner.failOn = "vmctl status"
	runner.failErr = errString("connect: /var/run/vmd.sock: No such file or directory")

	if err := p.Validate(context.Background()); err == nil {
		t.Fatal("Validate should fail when vmd is unreachable")
	}
}

// TestIsFatalSSHProbeError verifies definitive auth/escalation failures are
// distinguished from transient "not up yet" conditions.
func TestIsFatalSSHProbeError(t *testing.T) {
	fatal := []string{
		"doas: Operation not permitted\n",
		"Permission denied (publickey,password).\n",
		"sudo: a password is required\n",
	}
	for _, s := range fatal {
		if !isFatalSSHProbeError(&internal.ExecResult{Stderr: s}) {
			t.Errorf("isFatalSSHProbeError(%q) = false, want true", s)
		}
	}

	transient := []string{
		"ssh: connect to host 100.64.7.3 port 22: Connection refused\n",
		"",
	}
	for _, s := range transient {
		if isFatalSSHProbeError(&internal.ExecResult{Stderr: s}) {
			t.Errorf("isFatalSSHProbeError(%q) = true, want false", s)
		}
	}
	if isFatalSSHProbeError(nil) {
		t.Error("isFatalSSHProbeError(nil) = true, want false")
	}
}

// --- test helpers ---

type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recorder) indexOfPrefix(prefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, e := range r.events {
		if strings.HasPrefix(e, prefix) {
			return i
		}
	}
	return -1
}

type fakeRunner struct {
	mu      sync.Mutex
	calls   [][]string
	outputs map[string]string
	failOn  string
	failErr error
	rec     *recorder
}

func (f *fakeRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	full := append([]string{name}, args...)
	joined := strings.Join(full, " ")
	f.mu.Lock()
	f.calls = append(f.calls, full)
	outputs := f.outputs
	f.mu.Unlock()

	if f.rec != nil {
		f.rec.add("run:" + joined)
	}

	var out []byte
	for k, v := range outputs {
		if strings.Contains(joined, k) {
			out = []byte(v)
			break
		}
	}

	// Mirror defaultCommandRunner: on failure, the command's output is
	// returned alongside the error.
	if f.failOn != "" && strings.Contains(joined, f.failOn) {
		if f.failErr != nil {
			return out, f.failErr
		}
		return out, errString("fake runner failure for " + f.failOn)
	}

	// Emulate vmctl create actually producing the disk file, so tests that
	// assert the disk directory exists are meaningful.
	if strings.Contains(joined, "vmctl create") && len(args) > 0 {
		disk := args[len(args)-1]
		_ = os.MkdirAll(filepath.Dir(disk), 0755)
		_ = os.WriteFile(disk, []byte("cow-clone"), 0644)
	}
	return out, nil
}

func (f *fakeRunner) calledWith(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), substr) {
			return true
		}
	}
	return false
}

// newTestProvisioner returns a provisioner whose external commands and SSH
// readiness wait are faked, so the full provisioning flow can be exercised
// without an OpenBSD host.
func newTestProvisioner(t *testing.T) (*VMDProvisioner, *fakeRunner, *recorder) {
	t.Helper()
	dir := t.TempDir()
	rec := &recorder{}
	vmRoot := filepath.Join(dir, "vms")
	master := filepath.Join(dir, "disk-master.qcow2")
	if err := os.WriteFile(master, []byte("master-image-bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "agent_ed25519")
	if err := os.WriteFile(key, []byte("private-key"), 0600); err != nil {
		t.Fatal(err)
	}

	p := NewVMDProvisioner(vmRoot, master, "1G", "root", key, 22, "openbsd")
	runner := &fakeRunner{
		rec: rec,
		outputs: map[string]string{
			"vmctl start": "vmctl: started vm 6 successfully, tty /dev/ttyp3\n",
		},
	}
	p.run = runner.run
	p.lookPath = func(string) (string, error) { return "/usr/sbin/vmctl", nil }
	p.waitForSSH = func(ctx context.Context, ip string) error { return nil }
	return p, runner, rec
}

type errString string

func (e errString) Error() string { return string(e) }
