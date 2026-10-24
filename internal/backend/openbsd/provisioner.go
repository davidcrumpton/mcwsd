// Package openbsd implements a runtime.Provisioner backed by OpenBSD's
// vmd(8)/vmctl(8) virtual machine monitor.
//
// Unlike a jail or container, a VM's filesystem is locked inside a disk
// image, so the daemon cannot write into it directly. This provisioner
// therefore treats the VM as a black box that only exposes SSH:
//
//  1. clone the administrator-built "golden" master image with a
//     copy-on-write clone (vmctl create -b), which is near-instant;
//  2. start the clone with a local interface (vmctl start -L), which makes
//     vmd(8) run DHCP and assign a predictable address derived from the VM
//     ID (100.64.<id>.3 to the guest, 100.64.<id>.2 to the host tap);
//  3. poll until the guest answers on SSH;
//  4. return a handle whose transport runs commands over SSH.
//
// The master image is an administrative artifact: it must already carry the
// login user, the daemon's SSH public key, and any tooling (git, node, ...)
// the workspaces need. See OPENBSD-VMD-SETUP.md.
package openbsd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
)

// debugLogPrefix is an idea from a different generation and is not needed anymore
const debugLogPrefix = ""

// debugCommand logs, at debug level, the full command line that the
// provisioner is about to run, so a failing provision can be reproduced by
// copying the logged line.
func debugCommand(name string, args ...string) {
	internal.Log.Debug(debugLogPrefix + formatCommand(name, args...))
}

// debugf logs a debug-level message with the standard [DEBUG] shape.
func debugf(format string, args ...interface{}) {
	internal.Log.Debug(debugLogPrefix + fmt.Sprintf(format, args...))
}

// formatCommand renders a command and its arguments as a single, copy-pasteable
// command line, quoting arguments that contain whitespace or shell
// metacharacters.
func formatCommand(name string, args ...string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, quoteArg(name))
	for _, a := range args {
		parts = append(parts, quoteArg(a))
	}
	return strings.Join(parts, " ")
}

// formatArgv renders an argument vector (name followed by arguments) as a
// single, copy-pasteable command line.
func formatArgv(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	return formatCommand(argv[0], argv[1:]...)
}

// quoteArg shell-quotes an argument when it contains characters that would
// otherwise be interpreted by a shell.
func quoteArg(arg string) string {
	if arg == "" {
		return "''"
	}
	if !strings.ContainsAny(arg, " \t\n\"'\\$&|;<>()*?[]{}!#~`") {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

const (
	defaultVMRoot      = "/var/wsd/vms/openbsd"
	defaultMemory      = "1G"
	defaultProcessors  = 1
	defaultSSHPort     = 22
	defaultLoginUser   = "root"
	defaultBootTimeout = 5 * time.Minute
	defaultPollEvery   = 2 * time.Second
	// defaultSSHProbeTimeout bounds a single SSH readiness probe, so one hung
	// remote command cannot eat the whole boot window.
	defaultSSHProbeTimeout = 20 * time.Second

	// masterDiskName is the conventional filename of the golden image inside
	// the VM root. It is only a default; the path is a CLI option.
	masterDiskName = "disk-master.qcow2"
	// diskFileName is the per-workspace clone filename inside its directory.
	diskFileName = "disk.qcow2"
	// vmNamePrefix namespaces every VM/disk this daemon owns. Cleanup only
	// ever touches directories carrying this prefix.
	vmNamePrefix = "wsd-"

	// vmd(8)'s local-interface address formula for the first local interface:
	// the host tap gets 100.64.<id>.2 and the guest vio0 gets 100.64.<id>.3.
	// See vmctl(8) "LOCAL INTERFACES". Only the guest address is reachable as
	// the VM; .2 is the OpenBSD host's own end of the tap (SSHing to it hits
	// the host's sshd, not the guest).
	localNetworkPrefix = "100.64"
	localHostLast      = "2"
	localGuestLast     = "3"
)

// commandRunner runs an external command and returns its combined output. It
// is a field on the provisioner so tests can substitute a fake without an
// OpenBSD host.
type commandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// waitFunc waits until a freshly started VM at ip is reachable over SSH. It is
// a field so tests can bypass the (slow, network-dependent) real poller.
type waitFunc func(ctx context.Context, ip string) error

// VMDProvisioner implements runtime.Provisioner using OpenBSD vmd(8)/vmctl(8).
type VMDProvisioner struct {
	// vmRoot is the directory beneath which each workspace's disk directory
	// lives (default: /var/wsd/vms/openbsd).
	vmRoot string
	// masterImage is the path to the administrator-built golden qcow2 that
	// every workspace disk is a copy-on-write clone of. It is treated as
	// immutable once clones exist.
	masterImage string
	// memory is the vmctl(8) -m value, e.g. "1G".
	memory string
	// loginUser is the account the daemon SSHes into the guest as
	// (default: root).
	processors int
	loginUser string
	// sshKeyPath is the host path to the SSH private key matching the public
	// key baked into the master image.
	sshKeyPath string
	// sshPort is the guest SSH port (default: 22).
	sshPort int
	// guestOS is the operating system running inside the VM. It is reported
	// on the runtime handle so the agent picks the right package manager.
	guestOS runtime.OS
	// OpenBSD 8 support multiple processors
	processors runtime.OS

	// bootTimeout bounds how long Provision waits for a VM to become
	// reachable. VMs boot slowly; the default is deliberately generous.
	bootTimeout time.Duration
	// pollEvery is the interval between readiness probes.
	pollEvery time.Duration
	// probeTimeout bounds a single SSH readiness probe.
	probeTimeout time.Duration

	// escalationCommand, when non-empty, is prepended to the privileged
	// vmctl(8) operations on the host (create/start/stop). OpenBSD's doas(1)
	// is the natural choice; sudo(8) works too. Empty runs them directly,
	// which is only correct when mcwsd itself is root.
	escalationCommand string

	// guestEscalationCommand, when non-empty, is prepended to commands run
	// inside the guest VM over SSH. It is independent of the host-side
	// escalationCommand because the host and guest have separate privilege
	// configurations (and may even use different tools). Empty runs guest
	// commands directly; it is always ignored when loginUser is root.
	guestEscalationCommand string

	// run executes external commands. It is a field so tests can substitute
	// a fake.
	run commandRunner
	// lookPath resolves an executable in PATH. It is a field so tests can
	// stub the presence of vmctl(8).
	lookPath func(name string) (string, error)
	// waitForSSH is the readiness poller. It defaults to p.waitForSSHReady.
	waitForSSH waitFunc
}

// NewVMDProvisioner creates a vmd(8) provisioner. Empty arguments take their
// documented defaults. guestOS is the OS inside the VM ("openbsd", "freebsd",
// "linux", ...); an empty value defaults to openbsd.
func NewVMDProvisioner(vmRoot, masterImage, memory, processors, loginUser, sshKeyPath string, sshPort int, guestOS string) *VMDProvisioner {
	if vmRoot == "" {
		vmRoot = defaultVMRoot
	}
	if memory == "" {
		memory = defaultMemory
	}
	if processors == "" {
		processors = fmt.Sprintf("%d", defaultProcessors)
	}
	if loginUser == "" {
		loginUser = defaultLoginUser
	}
	if sshPort == 0 {
		sshPort = defaultSSHPort
	}
	if masterImage == "" {
		masterImage = filepath.Join(vmRoot, masterDiskName)
	}

	osName := runtime.OS(strings.ToLower(strings.TrimSpace(guestOS)))
	if osName == "" {
		osName = runtime.OSOpenBSD
	}

	p := &VMDProvisioner{
		vmRoot:       vmRoot,
		masterImage:  masterImage,
		memory:       memory,
		processors:    processors,
		loginUser:    loginUser,
		sshKeyPath:   sshKeyPath,
		sshPort:      sshPort,
		guestOS:      osName,
		bootTimeout:  defaultBootTimeout,
		pollEvery:    defaultPollEvery,
		probeTimeout: defaultSSHProbeTimeout,
		run:          defaultCommandRunner,
		lookPath:     exec.LookPath,
	}
	p.waitForSSH = p.waitForSSHReady
	return p
}

// WithEscalationCommand sets the command used to escalate the privileged
// vmctl(8) operations. Pass "doas" (OpenBSD's default) or "sudo"; pass ""
// (the default) to run them directly, which is only correct when mcwsd itself
// is root. Returns the receiver so it can be chained.
func (p *VMDProvisioner) WithEscalationCommand(cmd string) *VMDProvisioner {
	p.escalationCommand = cmd
	return p
}

// WithGuestEscalationCommand sets the command used to escalate commands run
// inside the guest VM over SSH. It is separate from WithEscalationCommand
// (which escalates host-side vmctl(8) operations); pass "doas", "sudo", or ""
// to run guest commands directly. It is ignored when the guest login user is
// root. Returns the receiver so it can be chained.
func (p *VMDProvisioner) WithGuestEscalationCommand(cmd string) *VMDProvisioner {
	p.guestEscalationCommand = cmd
	return p
}

// WithBootTimeout overrides how long Provision waits for SSH readiness.
// Returns the receiver so it can be chained.
func (p *VMDProvisioner) WithBootTimeout(d time.Duration) *VMDProvisioner {
	if d > 0 {
		p.bootTimeout = d
	}
	return p
}

// WithPollInterval overrides the readiness probe interval. Returns the
// receiver so it can be chained.
func (p *VMDProvisioner) WithPollInterval(d time.Duration) *VMDProvisioner {
	if d > 0 {
		p.pollEvery = d
	}
	return p
}

// WithSSHProbeTimeout overrides the bound on a single SSH readiness probe.
// Returns the receiver so it can be chained.
func (p *VMDProvisioner) WithSSHProbeTimeout(d time.Duration) *VMDProvisioner {
	if d > 0 {
		p.probeTimeout = d
	}
	return p
}

var (
	_ runtime.Provisioner          = (*VMDProvisioner)(nil)
	_ runtime.Validator            = (*VMDProvisioner)(nil)
	_ runtime.OrphanCleaner        = (*VMDProvisioner)(nil)
	_ runtime.WorkspaceIDDestroyer = (*VMDProvisioner)(nil)
)

// Name identifies this provisioner.
func (p *VMDProvisioner) Name() string { return "openbsd-vmd" }

func (p *VMDProvisioner) Init(ctx context.Context) error {
	if p.escalationCommand == "" {
		return nil
	}
	if _, err := p.runPrivileged(ctx, "vmctl", "log", "brief"); err != nil {
		return fmt.Errorf("privilege escalator %s is unavailable: %w", p.escalationCommand, err)
	}
	return nil
}

// runCommand logs the command and then runs it through the configured runner.
func (p *VMDProvisioner) runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	debugCommand(name, args...)
	return p.run(ctx, name, args...)
}

// runPrivileged runs a vmctl(8) operation through the configured escalation
// command when one is set.
func (p *VMDProvisioner) runPrivileged(ctx context.Context, name string, args ...string) ([]byte, error) {
	if p.escalationCommand == "" {
		return p.runCommand(ctx, name, args...)
	}
	escalated := append([]string{name}, args...)
	return p.runCommand(ctx, p.escalationCommand, escalated...)
}

// Provision clones the master image, starts a vmd(8) VM with a local
// interface, waits for SSH, and returns a handle whose transport runs commands
// over SSH.
func (p *VMDProvisioner) Provision(ctx context.Context, spec runtime.ProvisionSpec) (*runtime.RuntimeHandle, error) {
	if err := p.validateConfig(); err != nil {
		return nil, err
	}
	if err := validateWorkspaceID(spec.WorkspaceID); err != nil {
		return nil, err
	}

	vmName := vmNamePrefix + spec.WorkspaceID
	diskDir := p.vmDiskDir(vmName)
	diskPath := filepath.Join(diskDir, diskFileName)

	debugf("provisioning VM %s: vmRoot=%s master=%s memory=%s processors=%d loginUser=%s sshPort=%d guestOS=%s disk=%s workDir=%s hostEscalation=%q guestEscalation=%q",
		vmName, p.vmRoot, p.masterImage, p.memory, p.processors, p.loginUser, p.sshPort, p.guestOS, diskPath, spec.WorkDir, p.escalationCommand, p.guestEscalationCommand)

	if err := os.MkdirAll(diskDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create VM directory %s: %w", diskDir, err)
	}

	// 1. Copy-on-write clone of the golden image. -b (not -i) is what makes
	// this near-instant regardless of the master's size; -i would import a
	// full copy. The clone stays dependent on the master, so the master must
	// never be modified or moved while clones exist.
	debugf("cloning master image %s -> %s (copy-on-write)", p.masterImage, diskPath)
	if _, err := p.runPrivileged(ctx, "vmctl", "create", "-b", p.masterImage, diskPath); err != nil {
		p.cleanupFailedProvision(vmName, diskDir)
		return nil, fmt.Errorf("failed to create VM disk from master %s: %w", p.masterImage, err)
	}

	// 2. Start the VM with a local interface (-L). vmd(8) runs DHCP on it and
	// assigns a predictable address derived from the VM ID, so the daemon
	// never has to guess or parse a lease. -B disk boots from the cloned disk.

	// Backward compatibily:  Only OpenBSD 8.0 supports the -p option
	// so, we will only add it if processors is greater than 1.  
	// vmtctl doesn't have -p on 7.9 and earlier so it would error out.
	
	startArgs := []string{"start", "-L", "-B", "disk", "-d", diskPath, "-m", p.memory, vmName}
	if p.processors > 1 {
		startArgs = append(startArgs, "-p", fmt.Sprintf("%d", p.processors))
	}
	out, err := p.runPrivileged(ctx, "vmctl", startArgs...)
	if err != nil {
		p.cleanupFailedProvision(vmName, diskDir)
		return nil, fmt.Errorf("failed to start VM %s: %w", vmName, err)
	}

	vmID, err := parseVMID(string(out))
	if err != nil {
		p.cleanupFailedProvision(vmName, diskDir)
		return nil, fmt.Errorf("VM %s started but its id could not be determined: %w", vmName, err)
	}
	ip := localGuestIP(vmID)
	debugf("VM %s started as vm id %d; guest address %s (host tap endpoint %s is up)",
		vmName, vmID, ip, localHostIP(vmID))

	// 3. Wait for the guest to accept SSH. Boot time is the slow part of VM
	// provisioning, so this is bounded by bootTimeout rather than a single
	// short attempt.
	if err := p.waitForSSH(ctx, ip); err != nil {
		p.cleanupFailedProvision(vmName, diskDir)
		return nil, fmt.Errorf("VM %s started (id %d, %s) but SSH never became ready: %w", vmName, vmID, ip, err)
	}

	privilege := runtime.PrivilegeUser
	if p.loginUser == "root" {
		privilege = runtime.PrivilegeRoot
	}

	debugf("VM %s ready: os=%s user=%s privilege=%s workDir=%s", vmName, p.guestOS, p.loginUser, privilege, spec.WorkDir)

	return &runtime.RuntimeHandle{
		OS:        p.guestOS,
		Isolation: runtime.IsolationVM,
		Username:  p.loginUser,
		Privilege: privilege,
		WorkDir:   spec.WorkDir,
		NativeID:  vmName,
		Transport: NewSSHExecTransport(ip, p.sshPort, p.loginUser, p.sshKeyPath, p.guestEscalationCommand, p.guestOS),
	}, nil
}

// cleanupFailedProvision stops any VM that may have started and removes the
// per-workspace disk directory. It never touches the shared master image.
func (p *VMDProvisioner) cleanupFailedProvision(vmName, diskDir string) {
	debugf("cleaning up failed provision for VM %s (diskDir=%s)", vmName, diskDir)
	_, _ = p.runPrivileged(context.Background(), "vmctl", "stop", "-f", vmName)
	if err := p.removeDiskDir(context.Background(), diskDir); err != nil {
		// Cleanup is best-effort; the original error is what matters.
		_ = err
	}
}

// removeDiskDir removes a per-workspace disk directory. When escalation is
// configured, vmctl create ran as root, so the disk and directory may be
// root-owned and unremovable by the unprivileged daemon; escalate the removal
// for the same reason the jail provisioner escalates removeJailRoot.
func (p *VMDProvisioner) removeDiskDir(ctx context.Context, dir string) error {
	if p.escalationCommand != "" {
		_, err := p.runPrivileged(ctx, "rm", "-rf", dir)
		return err
	}
	return os.RemoveAll(dir)
}

// Destroy stops and removes the VM behind handle.
func (p *VMDProvisioner) Destroy(ctx context.Context, handle *runtime.RuntimeHandle) error {
	return p.DestroyByID(ctx, handle.NativeID)
}

// DestroyByID force-stops a VM by name and removes its disk directory. It is
// idempotent: a VM that is already stopped or already gone is not an error, so
// repeated destroys do not leak directories.
func (p *VMDProvisioner) DestroyByID(ctx context.Context, vmName string) error {
	if err := validateVMName(vmName); err != nil {
		return err
	}
	debugf("destroying VM %s", vmName)

	// vmctl stop -f is the force variant: the workspace is disposable and its
	// disk is about to be destroyed, so there is nothing worth a graceful
	// shutdown. "not found"/"not running" are benign.
	if _, err := p.runPrivileged(ctx, "vmctl", "stop", "-f", vmName); err != nil && !isBenignVMError(err) {
		return fmt.Errorf("failed to stop VM %s: %w", vmName, err)
	}

	diskDir := p.vmDiskDir(vmName)
	debugf("removing VM disk directory %s", diskDir)
	if err := p.removeDiskDir(ctx, diskDir); err != nil {
		return fmt.Errorf("failed to remove VM disk directory %s: %w", diskDir, err)
	}
	return nil
}

// DestroyWorkspaceByID destroys the VM whose native name is derived from its
// workspace ID. This supports cleanup when the backend's state record is
// missing but the OpenBSD VM and disk directory remain.
func (p *VMDProvisioner) DestroyWorkspaceByID(ctx context.Context, workspaceID string) error {
	if err := validateWorkspaceID(workspaceID); err != nil {
		return err
	}
	return p.DestroyByID(ctx, vmNamePrefix+workspaceID)
}

// RecoverHandle creates a RuntimeHandle for an existing OpenBSD VM.
// This implements the runtime.WorkspaceRecoverer interface for workspace recovery
// after daemon restart.
func (p *VMDProvisioner) RecoverHandle(ctx context.Context, nativeID string, workDir string) (*runtime.RuntimeHandle, error) {
	if err := validateVMName(nativeID); err != nil {
		return nil, err
	}

	// Check if the VM exists by checking its disk directory
	diskDir := p.vmDiskDir(nativeID)
	if _, err := os.Stat(diskDir); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("VM %s not found", nativeID)
		}
		return nil, fmt.Errorf("failed to check VM disk directory %s: %w", nativeID, err)
	}

	// The guest address is derived from the VM's numeric vmd(8) ID, which is
	// assigned at start time and is not part of the VM name. Query the running
	// VM table to recover it; the name only contains the workspace ID.
	out, err := p.runPrivileged(ctx, "vmctl", "status")
	if err != nil && !vmctlStatusReachable(out) {
		return nil, fmt.Errorf("failed to query vmctl status for VM %s: %w", nativeID, err)
	}
	vmID, err := parseVMIDByName(string(out), nativeID)
	if err != nil {
		return nil, err
	}
	ip := localGuestIP(vmID)
	debugf("recovered VM %s as vm id %d; guest address %s", nativeID, vmID, ip)

	// Determine privilege based on guest OS
	privilege := runtime.PrivilegeUser
	if p.loginUser == "root" {
		privilege = runtime.PrivilegeRoot
	}

	return &runtime.RuntimeHandle{
		OS:        p.guestOS,
		Isolation: runtime.IsolationVM,
		Username:  p.loginUser,
		Privilege: privilege,
		WorkDir:   workDir,
		NativeID:  nativeID,
		Transport: NewSSHExecTransport(ip, p.sshPort, p.loginUser, p.sshKeyPath, p.guestEscalationCommand, p.guestOS),
	}, nil
}

// CleanupOrphans removes per-workspace VM disk directories that are no longer
// tracked by the daemon (keep holds the live native IDs). This is the disk
// analogue of the state-file orphan sweep: because disk creation and VM start
// are separate steps, a crash in between can leave a disk with no VM and no
// state entry. It only ever touches directories named wsd-* that contain a
// disk.qcow2, so the master image (a file, not a directory) and any unrelated
// content are never removed.
func (p *VMDProvisioner) CleanupOrphans(ctx context.Context, keep map[string]bool) (int, error) {
	entries, err := os.ReadDir(p.vmRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to scan VM root %s: %w", p.vmRoot, err)
	}

	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, vmNamePrefix) {
			continue
		}
		if keep[name] {
			continue
		}
		diskDir := p.vmDiskDir(name)
		if _, err := os.Stat(filepath.Join(diskDir, diskFileName)); err != nil {
			// Not one of ours (no per-workspace disk); leave it alone.
			continue
		}
		debugf("removing orphan VM disk directory %s (no live workspace tracks it)", diskDir)
		_, _ = p.runPrivileged(ctx, "vmctl", "stop", "-f", name)
		if err := p.removeDiskDir(ctx, diskDir); err != nil {
			debugf("failed to remove orphan VM disk directory %s: %v", diskDir, err)
			continue
		}
		removed++
	}
	return removed, nil
}

// Validate checks that the host is ready to run VMs before the daemon starts
// serving: vmctl(8) exists, the golden image is present and non-empty, the VM
// root is writable, and vmd(8) is reachable. A failure here is a startup
// error, not a surprise on the first provision.
func (p *VMDProvisioner) Validate(ctx context.Context) error {
	if p.sshKeyPath == "" {
		return errors.New("openbsd VM ssh key path is required (-openbsd-ssh-key)")
	}
	if _, err := os.Stat(p.sshKeyPath); err != nil {
		return fmt.Errorf("openbsd VM ssh key %s is not readable: %w", p.sshKeyPath, err)
	}

	info, err := os.Stat(p.masterImage)
	if err != nil {
		return fmt.Errorf("openbsd VM master image %s is not readable: %w", p.masterImage, err)
	}
	if info.IsDir() {
		return fmt.Errorf("openbsd VM master image %s is a directory, not a disk image", p.masterImage)
	}
	if info.Size() == 0 {
		return fmt.Errorf("openbsd VM master image %s is empty", p.masterImage)
	}

	if err := os.MkdirAll(p.vmRoot, 0755); err != nil {
		return fmt.Errorf("openbsd VM root %s is not creatable: %w", p.vmRoot, err)
	}
	f, err := os.CreateTemp(p.vmRoot, ".wsd-validate-*")
	if err != nil {
		return fmt.Errorf("openbsd VM root %s is not writable: %w", p.vmRoot, err)
	}
	f.Close()
	_ = os.Remove(f.Name())

	if _, err := p.lookPath("vmctl"); err != nil {
		return fmt.Errorf("vmctl not found in PATH: %w", err)
	}

	// A harmless privileged query verifies both that escalation works (if
	// configured) and that vmd(8) is up.
	//
	// Note vmctl(8) exits non-zero when there are no VMs to list (it returns
	// ENOENT from print_vm_info), which is the normal state at daemon
	// startup. That is not a failure: vmctl only prints its table header
	// after vmd(8) has answered, so the header's presence proves the control
	// connection and escalation both worked. A genuine failure (vmd down,
	// doas/sudo denied) prints an error instead and no header.
	out, err := p.runPrivileged(ctx, "vmctl", "status")
	if err != nil && !vmctlStatusReachable(out) {
		return fmt.Errorf("vmctl status failed (is vmd(8) running, and is escalation configured?): %w", err)
	}
	if err != nil {
		debugf("vmctl status reported no VMs (exit non-zero); vmd is reachable")
	}
	if len(out) > 0 {
		debugf("vmctl status:\n%s", strings.TrimSpace(string(out)))
	}
	return nil
}

// vmctlStatusReachable reports whether out is the table header vmctl(8) prints
// once vmd(8) has answered a status request. Its presence distinguishes "no
// VMs running" (benign, non-zero exit) from a real failure to reach vmd.
func vmctlStatusReachable(out []byte) bool {
	s := string(out)
	return strings.Contains(s, "STATE") && strings.Contains(s, "NAME")
}

// vmDiskDir returns the per-workspace disk directory.
func (p *VMDProvisioner) vmDiskDir(vmName string) string {
	return filepath.Join(p.vmRoot, vmName)
}

// validateConfig rejects an incomplete provisioner configuration before any
// filesystem or hypervisor work happens.
func (p *VMDProvisioner) validateConfig() error {
	if strings.TrimSpace(p.masterImage) == "" {
		return errors.New("openbsd VM master image path is required")
	}
	if !filepath.IsAbs(p.masterImage) {
		return fmt.Errorf("openbsd VM master image path %q must be absolute", p.masterImage)
	}
	if strings.TrimSpace(p.memory) == "" {
		return errors.New("openbsd VM memory size is required")
	}
	if p.processors < 1 || p.processors > 64 {
		return fmt.Errorf("openbsd VM processors %d must be between 1 and 64", p.processors)
	}
	if strings.TrimSpace(p.sshKeyPath) == "" {
		return errors.New("openbsd VM ssh key path is required")
	}
	if err := p.validateVMRoot(); err != nil {
		return err
	}
	if p.loginUser == "" {
		return errors.New("openbsd VM login user is required")
	}
	return nil
}

// validateVMRoot rejects VM roots that would be dangerous to write into or
// remove from, such as "/" or a relative path.
func (p *VMDProvisioner) validateVMRoot() error {
	if strings.TrimSpace(p.vmRoot) == "" {
		return errors.New("openbsd VM root path is empty")
	}
	if !filepath.IsAbs(p.vmRoot) {
		return fmt.Errorf("openbsd VM root path %q must be absolute", p.vmRoot)
	}
	clean := filepath.Clean(p.vmRoot)
	if clean == string(filepath.Separator) {
		return errors.New("refusing to use / as an openbsd VM root")
	}
	return nil
}

var (
	safeNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	// startedVMRE extracts the numeric VM ID from vmctl start's output, e.g.
	// "vmctl: started vm 6 successfully, tty /dev/ttyp3".
	startedVMRE = regexp.MustCompile(`started vm ([0-9]+) successfully`)
)

// validateWorkspaceID rejects workspace IDs that are empty or unsafe to embed
// in a VM name and filesystem path.
func validateWorkspaceID(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("workspace id is required")
	}
	if strings.Contains(id, "..") || strings.ContainsAny(id, `/\`) || !safeNameRE.MatchString(id) {
		return fmt.Errorf("invalid workspace id %q", id)
	}
	return nil
}

// validateVMName guards the name used in vmctl(8) invocations and filesystem
// paths against traversal or injection.
func validateVMName(vmName string) error {
	if strings.TrimSpace(vmName) == "" {
		return errors.New("openbsd VM name is required")
	}
	if !strings.HasPrefix(vmName, vmNamePrefix) {
		return fmt.Errorf("openbsd VM name %q must start with %q", vmName, vmNamePrefix)
	}
	if strings.Contains(vmName, "..") || strings.ContainsAny(vmName, `/\`) || !safeNameRE.MatchString(vmName) {
		return fmt.Errorf("invalid openbsd VM name %q", vmName)
	}
	return nil
}

// parseVMID extracts the numeric VM ID from vmctl start's output. The ID is
// the key to the local-interface address formula.
func parseVMID(output string) (int, error) {
	m := startedVMRE.FindStringSubmatch(output)
	if m == nil {
		return 0, fmt.Errorf("no 'started vm N successfully' line in output: %q", strings.TrimSpace(output))
	}
	id, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("invalid VM id %q: %w", m[1], err)
	}
	return id, nil
}

// parseVMIDByName extracts the numeric VM ID for vmName from `vmctl status`
// output. Each VM is one row with the name in the final column, e.g.
//
//	ID   PID VCPUS  MAXMEM  CURMEM     TTY        OWNER    STATE NAME
//	 7 12345     1    1.0G    200M     ttyp3       root   running wsd-ws-abc
func parseVMIDByName(output, vmName string) (int, error) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[len(fields)-1] != vmName {
			continue
		}
		id, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		return id, nil
	}
	return 0, fmt.Errorf("VM %s is not running (no row in vmctl status)", vmName)
}

// localGuestIP returns the guest-side address vmd(8) assigns to the first
// local interface of the given VM ID: 100.64.<id>.3. See vmctl(8)
// "LOCAL INTERFACES".
func localGuestIP(vmID int) string {
	return fmt.Sprintf("%s.%d.%s", localNetworkPrefix, vmID, localGuestLast)
}

// localHostIP returns the host-side address vmd(8) assigns to the tap(4)
// endpoint of the first local interface: 100.64.<id>.2. This address belongs
// to the OpenBSD host itself, so it must never be used as the VM's address.
func localHostIP(vmID int) string {
	return fmt.Sprintf("%s.%d.%s", localNetworkPrefix, vmID, localHostLast)
}

// waitForSSHReady polls until the guest at ip answers on SSH, or bootTimeout
// elapses. It proceeds in three stages so a failure is reported precisely:
// the address appearing in the ARP table, the TCP port opening, then a real
// SSH command succeeding.
func (p *VMDProvisioner) waitForSSHReady(ctx context.Context, ip string) error {
	deadline := time.Now().Add(p.bootTimeout)
	debugf("waiting up to %s for VM at %s to become reachable", p.bootTimeout, ip)

	// Stage 1: the address must appear on the host's ARP table. On OpenBSD the
	// vmd tap(4) interface answers ARP for the guest.
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s to appear in the ARP table", ip)
		}
		out, err := p.run(ctx, "arp", "-an")
		if err != nil {
			debugf("arp -an failed: %v", err)
		} else if strings.Contains(string(out), ip) {
			debugf("VM at %s appeared in the ARP table", ip)
			break
		}
		if !sleepCtx(ctx, p.pollEvery) {
			return ctx.Err()
		}
	}

	// Stage 2: the SSH port must accept a TCP connection.
	addr := net.JoinHostPort(ip, strconv.Itoa(p.sshPort))
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for TCP %s to open", addr)
		}
		conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err == nil {
			conn.Close()
			debugf("TCP %s is open", addr)
			break
		}
		debugf("waiting for TCP %s: %v", addr, err)
		if !sleepCtx(ctx, p.pollEvery) {
			return ctx.Err()
		}
	}

	// Stage 3: a real SSH command must succeed. This is what proves the key,
	// the login user, sshd, and (when configured) guest privilege escalation
	// all actually work, not just that the port is bound.
	transport := NewSSHExecTransport(ip, p.sshPort, p.loginUser, p.sshKeyPath, p.guestEscalationCommand, p.guestOS)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for SSH on %s to accept commands", ip)
		}
		// Bound each probe so a hung remote command (e.g. doas waiting for a
		// password it can never read) cannot consume the whole boot window.
		probeCtx, cancel := context.WithTimeout(ctx, p.probeTimeout)
		res, err := transport.Exec(probeCtx, []string{"true"}, "", nil)
		cancel()

		if err == nil && res.ExitCode == 0 {
			debugf("SSH to %s is ready", ip)
			return nil
		}
		// The parent context being cancelled (the client gave up, or the
		// daemon is shutting down) is not a readiness problem; report it as-is
		// rather than retrying.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A definitive auth or escalation failure will not fix itself by
		// retrying. Surface it now with the guest's own diagnostics instead of
		// burning the whole boot window and then reporting a vague timeout.
		if isFatalSSHProbeError(res) {
			return fmt.Errorf("SSH to %s is reachable but the guest login/escalation is misconfigured: %s",
				ip, strings.TrimSpace(stderrOf(res)))
		}
		debugf("waiting for SSH on %s to accept commands (exit=%d err=%v stderr=%q)",
			ip, exitCodeOf(res), err, strings.TrimSpace(stderrOf(res)))
		if !sleepCtx(ctx, p.pollEvery) {
			return ctx.Err()
		}
	}
}

// stderrOf safely reads an ExecResult's stderr, tolerating nil.
func stderrOf(res *internal.ExecResult) string {
	if res == nil {
		return ""
	}
	return res.Stderr
}

// isFatalSSHProbeError reports whether a failed readiness probe failed for a
// reason retrying cannot fix: SSH rejected the key, or the guest escalation
// command (doas/sudo) refused. It deliberately does not match transient
// conditions such as "connection refused" (sshd not up yet) or a killed probe,
// which should keep being retried.
func isFatalSSHProbeError(res *internal.ExecResult) bool {
	if res == nil {
		return false
	}
	s := strings.ToLower(res.Stderr)
	for _, marker := range []string{
		"permission denied (publickey",
		"permission denied (keyboard-interactive",
		"doas:",
		"sudo:",
		"operation not permitted",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// exitCodeOf safely reads an ExecResult's exit code, tolerating nil.
func exitCodeOf(res *internal.ExecResult) int {
	if res == nil {
		return -1
	}
	return res.ExitCode
}

// sleepCtx sleeps for d, returning false if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// isBenignVMError reports whether an error from vmctl(8) means the VM was
// already stopped or does not exist — neither of which should block cleanup.
// The one "not found" that is NOT benign is vmctl itself being absent: that
// means we never ran the command, so the VM may still be up.
func isBenignVMError(err error) bool {
	if err == nil {
		return true
	}
	errStr := err.Error()
	if strings.Contains(errStr, "executable file not found") {
		return false
	}
	return strings.Contains(errStr, "not found") ||
		strings.Contains(errStr, "no such") ||
		strings.Contains(errStr, "not running") ||
		strings.Contains(errStr, "does not exist") ||
		strings.Contains(errStr, "not a running")
}

// defaultCommandRunner runs an external command and captures its combined
// output so a failure reports the command's own diagnostics instead of a bare
// "exit status 1".
func defaultCommandRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		if trimmed == "" {
			return out, err
		}
		return out, fmt.Errorf("%w: %s", err, trimmed)
	}
	if trimmed != "" {
		debugf("command output: %s", trimmed)
	}
	return out, nil
}
