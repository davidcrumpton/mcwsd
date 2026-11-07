package freebsd

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"sync"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
)

// debugLogPrefix is an idea from a different generation and is not needed anymore
const debugLogPrefix = ""

// debugCommand logs, at debug level, the full command line that the
// provisioner is about to run. Every external command goes through here so
// that a failing provision can be reproduced by copying the logged line.
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

// runCommand logs the command and then runs it through the configured runner.
func (p *FreeBSDJailProvisioner) runCommand(ctx context.Context, name string, args ...string) error {
	debugCommand(name, args...)
	return p.run(ctx, name, args...)
}

// runPrivilegedCommand behaves like runCommand, but for the handful of
// operations that genuinely require root, including archive extraction and
// jail filesystem setup. When escalationCommand is set (e.g.
// "mdo"), the command is routed through it instead of running directly under
// the daemon's own credentials, so mcwsd can run as an unprivileged user.
func (p *FreeBSDJailProvisioner) runPrivilegedCommand(ctx context.Context, name string, args ...string) error {
	if p.escalationCommand == "" {
		return p.runCommand(ctx, name, args...)
	}
	escalated := append([]string{name}, args...)
	return p.runCommand(ctx, p.escalationCommand, escalated...)
}

// downloadFile logs the fetch(1) command and then performs the download.
func (p *FreeBSDJailProvisioner) downloadFile(ctx context.Context, url, dest string) error {
	debugCommand("fetch", "-o", dest, url)
	return p.download(ctx, url, dest)
}

// extractArchive preserves the archive's ownership and permissions as root.
func (p *FreeBSDJailProvisioner) extractArchive(ctx context.Context, archive, dest string) error {
	return p.runPrivilegedCommand(ctx, "tar", "-xpf", archive, "-C", dest)
}

const (
	defaultJailConfPath = "/etc/jail.conf.d"
	defaultBaseJailPath = "/usr/local/jails"
	defaultRelease      = "15.1-RELEASE"
	defaultBaseURL      = "https://download.freebsd.org/releases"
)

// commandRunner runs an external command. It is a field on the provisioner so
// tests can substitute a fake without a FreeBSD host.
type commandRunner func(ctx context.Context, name string, args ...string) error

// downloadFunc fetches url and writes it to dest.
type downloadFunc func(ctx context.Context, url, dest string) error

// FreeBSDJailProvisioner implements runtime.Provisioner using FreeBSD jails.
// It manages jails on a FreeBSD host system using the jail(8) command-line tool.
type FreeBSDJailProvisioner struct {
	template   TemplateOptions
	templateMu sync.Mutex
	// jailConfPath is the path to the jail configuration directory (default: /etc/jail.conf.d)
	jailConfPath string
	// baseJailPath is the path to the base jail directory (default: /usr/local/jails)
	baseJailPath string
	// release is the FreeBSD release to use for new jails (default: "15.1-RELEASE").
	// It determines which base.txz is fetched and cached; it is never
	// hard-coded into the download or extraction logic.
	release string
	// networkInterface is the host network interface the jail's IP is
	// configured on (e.g. "vtnet0"). It must be supplied by the caller.
	networkInterface string
	// ip4Address is the IPv4 address assigned to the jail (e.g.
	// "192.168.122.100"). It must be supplied by the caller.
	ip4Address string
	// networkMode selects shared-stack or VNET networking.
	networkMode string
	// vnetGateway is the default route used by a VNET jail.
	vnetGateway string
	// baseURL is the root of the FreeBSD release download tree.
	baseURL string
	// arch is the FreeBSD architecture to fetch the base archive for.
	arch string

	// ip4BaseNetwork is the base network in CIDR notation (e.g., "192.168.1.200/24")
	// for automatic IP allocation. When set, the provisioner will automatically
	// assign IPs from this network pool.
	ip4BaseNetwork string
	// ip4PoolSize is the number of IP addresses to allocate from ip4BaseNetwork.
	// Used for automatic IP assignment when ip4Address is not specified.
	ip4PoolSize int
	// dnsServers is a list of DNS server IP addresses to use in jails.
	// These are written to /etc/resolv.conf in the jail root.
	dnsServers []string
	// ipam is the IP Address Manager for automatic IP allocation. It is
	// created lazily on first use and guarded by ipamMu, because Provision
	// runs concurrently for different workspaces.
	ipamMu sync.Mutex
	ipam   *IPAM

	// run and download are the side-effecting operations used
	// during provisioning. They are fields (rather than direct calls) so
	// unit tests can exercise the full provisioning flow without an actual
	// FreeBSD host.
	run      commandRunner
	download downloadFunc

	// escalationCommand, when non-empty, is prepended to the small set of
	// operations that require root: jail lifecycle, archive extraction,
	// jail filesystem setup and cleanup. This lets mcwsd run as an unprivileged daemon user while
	// still performing the handful of privileged kernel operations jails
	// need. Set it to "mdo" to use FreeBSD 15's base-system mdo(1)/mac_do(4)
	// instead of sudo/doas.
	//
	// It only ever wraps commands run via runPrivilegedCommand. It does
	// nothing for direct filesystem calls (os.MkdirAll, os.WriteFile,
	// os.RemoveAll) used elsewhere in this file — those still run under the
	// daemon's own UID, so jailConfPath, baseJailPath and everything below
	// them (jail roots, the base-archive cache) must be owned by, or
	// writable by, the daemon's user, except for extracted jail contents.
	// mdo cannot escalate a Go syscall; it
	// only escalates a spawned command.
	escalationCommand string
}

// WithEscalationCommand sets the command used to escalate the small set of
// operations that require root (including extraction and jail setup). Pass
// "mdo" to run mcwsd as an unprivileged user on FreeBSD 15+ and rely on
// mac_do(4) for those operations; pass "" (the default) to run them
// directly, which is only correct if the daemon process itself is root.
// Returns the receiver so it can be chained after NewFreeBSDJailProvisioner.
func (p *FreeBSDJailProvisioner) WithEscalationCommand(cmd string) *FreeBSDJailProvisioner {
	p.escalationCommand = cmd
	return p
}

// WithNetworkMode selects the jail networking model. The empty value means
// shared, preserving the historical behavior.
func (p *FreeBSDJailProvisioner) WithNetworkMode(mode string) *FreeBSDJailProvisioner {
	if mode == "" {
		mode = "shared"
	}
	p.networkMode = mode
	return p
}

// WithVNETGateway sets the default route used by VNET jails.
func (p *FreeBSDJailProvisioner) WithVNETGateway(gateway string) *FreeBSDJailProvisioner {
	p.vnetGateway = gateway
	return p
}

// WithIPBaseNetwork sets the base network for automatic IP allocation.
// When set and ip4Address is not provided, the provisioner will automatically
// assign IPs from this network pool.
func (p *FreeBSDJailProvisioner) WithIPBaseNetwork(network string) *FreeBSDJailProvisioner {
	p.ip4BaseNetwork = network
	return p
}

// WithIPPoolSize sets the number of IP addresses to allocate from the base network.
func (p *FreeBSDJailProvisioner) WithIPPoolSize(size int) *FreeBSDJailProvisioner {
	p.ip4PoolSize = size
	return p
}

// WithDNS Servers sets the list of DNS server IP addresses for jails.
func (p *FreeBSDJailProvisioner) WithDNSServers(servers []string) *FreeBSDJailProvisioner {
	p.dnsServers = servers
	return p
}

// WithIPAM sets a custom IP Address Manager for IP allocation.
// This allows for more complex IP management scenarios.
func (p *FreeBSDJailProvisioner) WithIPAM(ipam *IPAM) *FreeBSDJailProvisioner {
	p.ipam = ipam
	return p
}

// getIPAM returns the provisioner's IP Address Manager, creating it lazily on
// first use. Provision runs concurrently for different workspaces, so both the
// creation and every subsequent read must happen under ipamMu.
func (p *FreeBSDJailProvisioner) getIPAM() (*IPAM, error) {
	p.ipamMu.Lock()
	defer p.ipamMu.Unlock()
	if p.ipam == nil {
		alloc, err := NewIPAM(p.ip4BaseNetwork, p.ip4PoolSize)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize IPAM: %w", err)
		}
		p.ipam = alloc
	}
	return p.ipam, nil
}

// releaseIPAM returns a jail's IP address to the pool after it is destroyed.
// jailName is the native jail identifier ("wsd-<workspace-id>").
func (p *FreeBSDJailProvisioner) releaseIPAM(jailName string) {
	p.ipamMu.Lock()
	defer p.ipamMu.Unlock()
	if p.ipam == nil {
		return
	}
	p.ipam.Release(strings.TrimPrefix(jailName, "wsd-"))
}

// reserveIP re-registers a recovered jail's IP address so future allocations
// do not hand it out again. Failures are logged, not fatal: the recovered
// workspace remains usable, only the collision guard is weakened.
func (p *FreeBSDJailProvisioner) reserveIP(workspaceID, ip string) {
	alloc, err := p.getIPAM()
	if err != nil {
		debugf("could not initialize IPAM while recovering workspace %s: %v", workspaceID, err)
		return
	}
	if err := alloc.Reserve(workspaceID, ip); err != nil {
		debugf("could not reserve recovered IP %s for workspace %s: %v", ip, workspaceID, err)
	}
}

// NewFreeBSDJailProvisioner creates a new FreeBSDJailProvisioner with the given configuration.
// If jailConfPath is empty, it defaults to "/etc/jail.conf.d".
// If baseJailPath is empty, it defaults to "/usr/local/jails".
// If release is empty, it defaults to "15.1-RELEASE".
//
// networkInterface and ip4Address are required for provisioning; they may be
// left empty here if the caller supplies them per workspace via the
// ProvisionSpec.
func NewFreeBSDJailProvisioner(jailConfPath, baseJailPath, release, networkInterface, ip4Address string) *FreeBSDJailProvisioner {
	if jailConfPath == "" {
		jailConfPath = defaultJailConfPath
	}
	if baseJailPath == "" {
		baseJailPath = defaultBaseJailPath
	}
	if release == "" {
		release = defaultRelease
	}
	return &FreeBSDJailProvisioner{
		jailConfPath:     jailConfPath,
		baseJailPath:     baseJailPath,
		release:          release,
		networkInterface: networkInterface,
		ip4Address:       ip4Address,
		networkMode:      "shared",
		baseURL:          defaultBaseURL,
		arch:             defaultFreeBSDArch(),
		run:              defaultCommandRunner,
		download:         defaultDownloader,
	}
}

var _ runtime.Provisioner = (*FreeBSDJailProvisioner)(nil)

// Name identifies this provisioner.
func (p *FreeBSDJailProvisioner) Name() string { return "freebsd-jail" }

func (p *FreeBSDJailProvisioner) Init(ctx context.Context) error {
	if err := p.validateTemplate(); err != nil {
		return err
	}
	if err := p.validateDNS(); err != nil {
		return err
	}
	if p.escalationCommand == "" {
		return nil
	}
	if err := p.runPrivilegedCommand(ctx, "jls"); err != nil {
		return fmt.Errorf("privilege escalator %s is unavailable: %w", p.escalationCommand, err)
	}
	return nil
}

// Provision creates a new FreeBSD jail and returns a handle to it. Before the
// jail is started the jail root is populated from the matching FreeBSD
// base.txz, so the jail boots into a usable userland. The jail is configured
// with:
//   - A unique name based on the workspace ID
//   - A root filesystem extracted from the configured release's base.txz
//   - The configured host interface and IPv4 address
//   - devfs mounted at /dev
//   - Appropriate mount points for cache and workspace directories
//   - A working directory set up inside the jail
func (p *FreeBSDJailProvisioner) Provision(ctx context.Context, spec runtime.ProvisionSpec) (*runtime.RuntimeHandle, error) {
	if err := p.validateTemplate(); err != nil {
		return nil, err
	}
	if err := p.validateDNS(); err != nil {
		return nil, err
	}
	release := p.release
	networkInterface := p.networkInterface
	ip4Address := p.ip4Address
	networkMode := p.networkMode
	vnetGateway := p.vnetGateway

	// Per-workspace values override the provisioner's configured defaults.
	if spec.Interface != "" {
		networkInterface = spec.Interface
	}
	if spec.IP4Address != "" {
		ip4Address = spec.IP4Address
	}

	// Auto-assign IP from pool if IPAM is configured and no specific IP is provided
	if ip4Address == "" && p.ip4BaseNetwork != "" && p.ip4PoolSize > 0 {
		alloc, err := p.getIPAM()
		if err != nil {
			return nil, err
		}
		// Allocate IP for this workspace
		assignedIP, err := alloc.Allocate(spec.WorkspaceID)
		if err != nil {
			return nil, fmt.Errorf("failed to allocate IP address: %w", err)
		}
		ip4Address = assignedIP
		debugf("auto-assigned IP %s to workspace %s from pool %s", ip4Address, spec.WorkspaceID, p.ip4BaseNetwork)

		// If no VNET gateway is configured, use the IPAM's gateway
		if vnetGateway == "" && networkMode == "vnet" {
			vnetGateway = alloc.GatewayIP().String()
			debugf("auto-assigned gateway %s for workspace %s", vnetGateway, spec.WorkspaceID)
		}
	}

	// Validate everything before touching the filesystem or writing config.
	if err := validateRelease(release); err != nil {
		return nil, err
	}
	if networkMode == "" {
		networkMode = "shared"
	}
	if networkMode != "shared" && networkMode != "vnet" {
		return nil, fmt.Errorf("unsupported freebsd jail network mode %q (want shared or vnet)", networkMode)
	}
	if networkMode == "vnet" {
		// ip4Address is optional for VNET: when empty, DHCP will be used
		if ip4Address != "" {
			if err := validateVNETIP4Address(ip4Address); err != nil {
				return nil, err
			}
			if err := validateIP4Address(vnetGateway); err != nil {
				return nil, fmt.Errorf("invalid freebsd VNET gateway: %w", err)
			}
			if len(p.dnsServers) == 0 {
				return nil, errors.New("static freebsd VNET requires explicit DNS servers")
			}
		} else if vnetGateway != "" {
			// If gateway is provided, we need an IP address too
			return nil, errors.New("freebsd VNET gateway provided without an IPv4 address; provide a CIDR address or omit both for DHCP")
		}
	}

	// An empty IPv4 address means "use DHCP": the jail inherits the host's
	// addresses on the (DHCP-configured) interface. A fixed address, when
	// supplied, still requires a valid interface.
	if networkMode == "vnet" {
		if err := validateInterface(networkInterface); err != nil {
			return nil, err
		}
	} else if ip4Address == "" {
		debugf("no fixed IPv4 address configured; jail %s will inherit the host's DHCP-assigned address", spec.WorkspaceID)
	} else {
		if err := validateInterface(networkInterface); err != nil {
			return nil, err
		}
		if err := validateIP4Address(ip4Address); err != nil {
			return nil, err
		}
	}

	jailName := fmt.Sprintf("wsd-%s", spec.WorkspaceID)
	jibName := jibNameForJail(jailName)
	vnetInterface := "e0b_" + jibName

	// Determine the per-jail root. The image is not used to name the root:
	// container image references from the environment detector have no
	// meaning for a jail, and every workspace gets its own containers/<jail>
	// directory.
	jailRoot := p.getJailRootPath(jailName)
	if err := p.validateJailRoot(jailRoot); err != nil {
		return nil, err
	}

	// The workspace repo lives in a per-workspace host directory beneath the
	// configured base path (mirroring the podman provisioner). Mounting the
	// base path root would make every workspace share one checkout.
	workspaceHostDir := ""
	if spec.WorkspaceBasePath != "" {
		workspaceHostDir = filepath.Join(spec.WorkspaceBasePath, spec.WorkspaceID)
	}

	debugf("provisioning jail %s: release=%s networkMode=%s interface=%s ip4=%s gateway=%s image=%s jailRoot=%s workDir=%s cacheMount=%s workspaceBase=%s workspaceHostDir=%s",
		jailName, release, networkMode, networkInterface, ip4Address, vnetGateway, spec.Image, jailRoot, spec.WorkDir, spec.CacheMountPath, spec.WorkspaceBasePath, workspaceHostDir)

	// Obtain (or reuse) the release-specific base archive. This is a shared,
	// cached artifact and is never deleted as part of workspace cleanup.
	baseArchive, err := p.ensureBaseArchive(ctx, release)
	if err != nil {
		return nil, err
	}

	// Populate the jail root from the base archive before the jail starts.
	rootExisted := pathExists(jailRoot)
	if err := p.populateJailRoot(ctx, baseArchive, jailRoot); err != nil {
		p.cleanupFailedProvision(jailName, jailRoot, rootExisted)
		return nil, err
	}

	// Nullfs mount targets must exist inside the jail root before the jail
	// starts, or jail(8) aborts with a generic "exit status 1".
	if err := p.ensureJailMountPoints(ctx, jailRoot, spec.WorkDir, spec.CacheMountPath, workspaceHostDir); err != nil {
		p.cleanupFailedProvision(jailName, jailRoot, rootExisted)
		return nil, err
	}

	// Create jail configuration
	if err := p.createJailConfig(jailName, jailRoot, networkMode, networkInterface, ip4Address, vnetGateway, spec.WorkDir, spec.CacheMountPath, workspaceHostDir); err != nil {
		p.cleanupFailedProvision(jailName, jailRoot, rootExisted)
		return nil, fmt.Errorf("failed to create jail config: %w", err)
	}

	// Start the jail. jail(2) creation is a privileged kernel operation, so
	// this goes through runPrivilegedCommand rather than runCommand.
	if err := p.runPrivilegedCommand(ctx, "jail", "-c", "-f", p.getJailConfigPath(jailName)); err != nil {
		// Never report a workspace as provisioned unless the jail started.
		// Remove the generated config, make sure no partially-started jail
		// is left behind, and clean up the root if we created it.
		p.cleanupFailedProvision(jailName, jailRoot, rootExisted)
		return nil, fmt.Errorf("failed to start jail: %w", err)
	}

	return &runtime.RuntimeHandle{
		OS:        runtime.OSFreeBSD,
		Isolation: runtime.IsolationJail,
		Username:  "root",
		Privilege: runtime.PrivilegeRoot,
		WorkDir:   spec.WorkDir,
		NativeID:  jailName,
		Transport: NewFreeBSDJailExecTransport(jailName).
			WithNetworkMode(networkMode, vnetInterface).
			WithEscalationCommand(p.escalationCommand),
	}, nil
}

// jibNameForJail derives a stable interface suffix. jib prefixes this value
// with "e0b_", and FreeBSD interface names have room for at most 11 suffix
// characters.
func jibNameForJail(jailName string) string {
	name := strings.ReplaceAll(strings.TrimPrefix(jailName, "wsd-"), "-", "")
	if len(name) <= 11 {
		return name
	}
	digest := sha256.Sum256([]byte(jailName))
	return fmt.Sprintf("w%x", digest[:5])
}

// cleanupFailedProvision removes the generated jail configuration, stops any
// partially-started jail, and (only if this provisioning run created it)
// removes the workspace jail root. It never touches the shared base archive
// cache.
func (p *FreeBSDJailProvisioner) cleanupFailedProvision(jailName, jailRoot string, rootExisted bool) {
	debugf("cleaning up failed provision for jail %s (jailRoot=%s rootExisted=%t)", jailName, jailRoot, rootExisted)
	// Best-effort stop of a jail that may have started before the failure.
	_ = p.runPrivilegedCommand(context.Background(), "jail", "-r", jailName)
	if err := p.removeJailConfig(jailName); err != nil {
		// Cleanup is best-effort; the original error is what matters.
		_ = err
	}
	if !rootExisted {
		// A partially-started jail can leave devfs/nullfs mounts behind even
		// when `jail -c` reports failure. RemoveAll cannot delete a mount
		// point, so unmount first or the root is left behind as a non-empty
		// directory that no longer looks like a FreeBSD root.
		p.unmountJailRoot(jailRoot)
		_ = p.removeJailRoot(jailRoot)
	}
}

// unmountJailRoot best-effort force-unmounts the filesystems a partially
// started jail may have mounted beneath jailRoot. It never returns an error:
// cleanup proceeds regardless.
func (p *FreeBSDJailProvisioner) unmountJailRoot(jailRoot string) {
	mounts := []string{
		filepath.Join(jailRoot, "workspace", "repo"),
		filepath.Join(jailRoot, "cache"),
		filepath.Join(jailRoot, "tmp"),
		filepath.Join(jailRoot, "dev"),
	}
	// Show what is currently mounted so a stuck mount is visible in the logs.
	_ = p.runCommand(context.Background(), "mount")
	for _, mp := range mounts {
		if !pathExists(mp) {
			continue
		}
		debugf("force-unmounting %s (leftover from a failed jail start)", mp)
		_ = p.runPrivilegedCommand(context.Background(), "umount", "-f", mp)
	}
}

// Destroy stops and removes the jail and its configuration.
func (p *FreeBSDJailProvisioner) Destroy(ctx context.Context, handle *runtime.RuntimeHandle) error {
	return p.DestroyByID(ctx, handle.NativeID)
}

// DestroyByID stops and removes a jail by its name (jail identifier), then
// removes its configuration and its per-jail root. Cleanup runs even if the
// jail was already gone, so repeated destroys do not leak directories.
func (p *FreeBSDJailProvisioner) DestroyByID(ctx context.Context, jailName string) error {
	debugf("destroying jail %s", jailName)
	jailRoot := p.getJailRootPath(jailName)

	// Stop the jail first. "not found" / "already stopped" are benign: there
	// is nothing left to stop and cleanup should still proceed.
	if err := p.runPrivilegedCommand(ctx, "jail", "-r", jailName); err != nil && !isBenignJailError(err) {
		// We could not confirm the jail stopped; leave its root intact and
		// report the failure so the caller can retry.
		return fmt.Errorf("failed to stop jail %s: %w", jailName, err)
	}

	// Remove the jail configuration.
	if err := p.removeJailConfig(jailName); err != nil {
		return fmt.Errorf("failed to remove jail config: %w", err)
	}

	// Remove the per-jail root (unmounting any mounts first).
	p.unmountJailRoot(jailRoot)
	_ = p.removeJailRoot(jailRoot)

	// Return the jail's IP address to the pool now that it is gone.
	p.releaseIPAM(jailName)

	return nil
}

// RecoverHandle creates a RuntimeHandle for an existing FreeBSD jail.
// This implements the runtime.WorkspaceRecoverer interface for workspace recovery
// after daemon restart.
func (p *FreeBSDJailProvisioner) RecoverHandle(ctx context.Context, nativeID string, workDir string) (*runtime.RuntimeHandle, error) {
	// Check if the jail exists by reading its configuration. The file also
	// records the jail's IP address, which we re-reserve so a recovered
	// workspace cannot collide with a new allocation made after the in-memory
	// IPAM state was lost on restart.
	jailConfigPath := p.getJailConfigPath(nativeID)
	data, err := os.ReadFile(jailConfigPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("jail %s not found", nativeID)
		}
		return nil, fmt.Errorf("failed to read jail config %s: %w", nativeID, err)
	}

	if p.ip4BaseNetwork != "" && p.ip4PoolSize > 0 {
		if ip := extractJailIPv4(string(data)); ip != "" {
			p.reserveIP(strings.TrimPrefix(nativeID, "wsd-"), ip)
		}
	}

	return &runtime.RuntimeHandle{
		OS:        runtime.OSFreeBSD,
		Isolation: runtime.IsolationJail,
		Username:  "root",
		Privilege: runtime.PrivilegeRoot,
		WorkDir:   workDir,
		NativeID:  nativeID,
		Transport: NewFreeBSDJailExecTransport(nativeID).WithEscalationCommand(p.escalationCommand),
	}, nil
}

// containersDirName is the subdirectory of baseJailPath that holds one root
// directory per jail. It mirrors the conventional FreeBSD ZFS jail layout
// (jails/containers/<jail>) and gives every workspace its own root instead of
// sharing one release-wide directory.
const containersDirName = "containers"

// getJailRootPath returns the per-jail root for jailName. Each jail gets its
// own directory beneath <baseJailPath>/containers so workspaces never share a
// writable root.
func (p *FreeBSDJailProvisioner) getJailRootPath(jailName string) string {
	return filepath.Join(p.baseJailPath, containersDirName, jailName)
}

// getJailConfigPath returns the path to the jail configuration file.
func (p *FreeBSDJailProvisioner) getJailConfigPath(jailName string) string {
	return fmt.Sprintf("%s/%s.conf", p.jailConfPath, jailName)
}

// baseArchivePath returns the release-specific cache location of base.txz.
// Different releases get different paths so they never overwrite each other.
func (p *FreeBSDJailProvisioner) baseArchivePath(release string) string {
	return filepath.Join(p.baseJailPath, "media", release, "base.txz")
}

// baseArchiveURL constructs the download URL for a release's base.txz. The
// release is the caller-configured value; it is never hard-coded.
func (p *FreeBSDJailProvisioner) baseArchiveURL(release string) string {
	return fmt.Sprintf("%s/%s/%s/%s/base.txz", p.baseURL, p.arch, freebsdMachine(p.arch), release)
}

// ensureBaseArchive returns the path to a cached base.txz for release,
// downloading it first if it is not already present. The archive is cached
// per release beneath the base jail path.
func (p *FreeBSDJailProvisioner) ensureBaseArchive(ctx context.Context, release string) (string, error) {
	if p.template.BaseURL != "" {
		return p.ensureTemplateArchive(ctx, release)
	}
	dest := p.baseArchivePath(release)
	if info, err := os.Stat(dest); err == nil && info.Size() > 0 {
		debugf("using cached base archive %s for release %s (%d bytes)", dest, release, info.Size())
		return dest, nil
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return "", fmt.Errorf("failed to create base archive cache directory: %w", err)
	}

	url := p.baseArchiveURL(release)
	debugf("downloading base archive for release %s: %s -> %s", release, url, dest)
	if err := p.downloadFile(ctx, url, dest); err != nil {
		// Do not leave a partial download behind that a later run might
		// mistake for a valid cached archive.
		_ = os.Remove(dest)
		return "", fmt.Errorf("failed to download base archive for release %s from %s: %w", release, url, err)
	}

	info, err := os.Stat(dest)
	if err != nil {
		return "", fmt.Errorf("base archive not present after download: %w", err)
	}
	if info.Size() == 0 {
		_ = os.Remove(dest)
		return "", fmt.Errorf("base archive download for release %s produced an empty file", release)
	}

	return dest, nil
}

// populateJailRoot creates the jail root (if necessary), extracts the base
// archive into it, and ensures /dev exists for mount.devfs.
func (p *FreeBSDJailProvisioner) populateJailRoot(ctx context.Context, baseArchive, jailRoot string) error {
	debugf("populating jail root %s from base archive %s", jailRoot, baseArchive)
	if err := os.MkdirAll(jailRoot, 0755); err != nil {
		return fmt.Errorf("failed to create jail root %s: %w", jailRoot, err)
	}

	// Clear any devfs/nullfs mounts a previous failed start may have left
	// beneath the root before inspecting or extracting over it.
	p.unmountJailRoot(jailRoot)

	if err := ensureSafeJailRoot(jailRoot); err != nil {
		// A previous failed start can leave a non-empty directory that no
		// longer looks like a FreeBSD root (typically a leftover devfs/nullfs
		// mount). Wipe it and rebuild from the base archive rather than
		// leaving the daemon permanently stuck.
		debugf("jail root %s is not usable (%v); clearing stale contents and rebuilding", jailRoot, err)
		if rmErr := p.removeJailRoot(jailRoot); rmErr != nil {
			return fmt.Errorf("failed to clear stale jail root %s: %w (original: %v)", jailRoot, rmErr, err)
		}
		if mkErr := os.MkdirAll(jailRoot, 0755); mkErr != nil {
			return fmt.Errorf("failed to recreate jail root %s: %w", jailRoot, mkErr)
		}
	}

	if err := p.extractArchive(ctx, baseArchive, jailRoot); err != nil {
		return fmt.Errorf("failed to extract base archive into %s: %w", jailRoot, err)
	}
	if p.template.BaseURL != "" {
		if _, err := os.Stat(filepath.Join(jailRoot, "bin", "sh")); err != nil {
			return fmt.Errorf("template must contain a FreeBSD root with bin/sh at the archive root: %w", err)
		}
	}

	// Ensure the database directory exists even if it was omitted from a template.
	p.ensurePkgDBOwnership(ctx, jailRoot)

	// mount.devfs requires /dev to exist in the jail root.
	if err := p.makeJailDirectories(ctx, jailRoot, "dev"); err != nil {
		return fmt.Errorf("failed to create /dev in jail root: %w", err)
	}

	// The base archive does not ship a usable /etc/resolv.conf. With the
	// ip4=inherit (non-VNET) jail model the jail shares the host's network
	// stack, so copying the host resolver config in is enough for name
	// resolution (pkg, npm, go mod, ...). DNS setup is required for custom
	// templates and explicit DNS settings; legacy defaults remain best-effort.
	if err := p.copyHostResolvConf(ctx, jailRoot); err != nil {
		if p.template.BaseURL != "" || len(p.dnsServers) > 0 {
			return fmt.Errorf("failed to install jail DNS configuration: %w", err)
		}
		debugf("could not install host resolv.conf in jail root %s: %v", jailRoot, err)
	}

	debugf("jail root %s populated", jailRoot)
	return nil
}

// ensurePkgDBOwnership makes the pkg database directory root-owned. A jail
// template may omit this directory, and pkg(8) requires root ownership.
// It is best-effort: failures are logged, not fatal, so
// provisioning still completes and the error surfaces when pkg runs.
func (p *FreeBSDJailProvisioner) ensurePkgDBOwnership(ctx context.Context, jailRoot string) {
	pkgdb := filepath.Join(jailRoot, "var", "db", "pkg")
	if err := p.runPrivilegedCommand(ctx, "mkdir", "-p", pkgdb); err != nil {
		debugf("could not create pkg database dir %s: %v", pkgdb, err)
		return
	}
	if err := p.runPrivilegedCommand(ctx, "chown", "0:0", pkgdb); err != nil {
		debugf("could not chown pkg database dir %s to root: %v", pkgdb, err)
	}
}

// copyHostResolvConf copies the host's /etc/resolv.conf into the jail root so
// name resolution works inside the jail. Writes are confined to the root,
// including when a custom template contains symlinks.
// If custom DNS servers are configured, they are used instead of the host's resolv.conf.
func (p *FreeBSDJailProvisioner) copyHostResolvConf(ctx context.Context, jailRoot string) error {
	root, err := os.OpenRoot(jailRoot)
	if err != nil {
		return err
	}
	defer root.Close()
	dest := filepath.Join("etc", "resolv.conf")
	var data []byte
	if len(p.dnsServers) > 0 {
		var content string
		for _, server := range p.dnsServers {
			content += "nameserver " + server + "\n"
		}
		data = []byte(content)
	} else {
		data, err = os.ReadFile("/etc/resolv.conf")
		if err != nil {
			return err
		}
	}
	if p.escalationCommand != "" {
		// Retain os.Root's rejection of paths escaping the jail. chroot also
		// confines the privileged write if a symlink changes after this check.
		for _, path := range []string{"etc", dest} {
			if _, err := root.Stat(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return p.runPrivilegedCommand(ctx, "chroot", jailRoot, "/bin/sh", "-c",
			`/bin/mkdir -p /etc && umask 022 && printf '%s' "$1" > /etc/resolv.conf`,
			"mcwsd-dns", string(data))
	}
	if err := root.MkdirAll("etc", 0755); err != nil {
		return err
	}
	return root.WriteFile(dest, data, 0644)
}

func (p *FreeBSDJailProvisioner) makeJailDirectories(ctx context.Context, jailRoot string, paths ...string) error {
	if p.escalationCommand != "" {
		args := append([]string{jailRoot, "/bin/mkdir", "-p", "--"}, paths...)
		return p.runPrivilegedCommand(ctx, "chroot", args...)
	}
	root, err := os.OpenRoot(jailRoot)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, path := range paths {
		if err := root.MkdirAll(path, 0755); err != nil {
			return err
		}
	}
	return nil
}

func (p *FreeBSDJailProvisioner) validateDNS() error {
	for _, server := range p.dnsServers {
		if net.ParseIP(server) == nil {
			return fmt.Errorf("invalid freebsd DNS server IP %q", server)
		}
	}
	if p.networkMode == "vnet" && (p.ip4Address != "" || p.vnetGateway != "" || p.ip4PoolSize > 0) && len(p.dnsServers) == 0 {
		return errors.New("static freebsd VNET requires explicit DNS servers")
	}
	return nil
}

// ensureJailMountPoints creates the per-workspace host directory and the
// directories inside the jail root that the generated jail.conf mounts over.
// A missing nullfs source or target makes `jail -c` fail with a bare "exit
// status 1", so this must run before the jail is started.
func (p *FreeBSDJailProvisioner) ensureJailMountPoints(ctx context.Context, jailRoot, workDir, cacheMountPath, workspaceHostDir string) error {
	root, err := os.OpenRoot(jailRoot)
	if err != nil {
		return fmt.Errorf("failed to open jail root for mount points: %w", err)
	}
	defer root.Close()
	if err := p.makeJailDirectories(ctx, jailRoot, "tmp"); err != nil {
		return fmt.Errorf("failed to create jail /tmp mount point: %w", err)
	}
	if p.escalationCommand != "" {
		err = p.runPrivilegedCommand(ctx, "chroot", jailRoot, "/bin/chmod", "1777", "/tmp")
	} else {
		err = root.Chmod("tmp", 0777|os.ModeSticky)
	}
	if err != nil {
		return fmt.Errorf("failed to set jail /tmp permissions: %w", err)
	}
	if workspaceHostDir != "" && workDir != "" {
		// Create the per-workspace host directory that will be nullfs-mounted
		// into the jail. Unlike mounting the shared base path, this keeps each
		// workspace's checkout separate.
		debugf("ensuring workspace host dir %s", workspaceHostDir)
		if err := os.MkdirAll(workspaceHostDir, 0755); err != nil {
			return fmt.Errorf("failed to create workspace host dir %s: %w", workspaceHostDir, err)
		}
		target := filepath.Join(jailRoot, filepath.Clean("/"+workDir))
		debugf("ensuring jail workspace mount point %s (source %s)", target, workspaceHostDir)
		if err := p.makeJailDirectories(ctx, jailRoot, strings.TrimPrefix(filepath.Clean("/"+workDir), "/")); err != nil {
			return fmt.Errorf("failed to create jail workspace mount point %s: %w", target, err)
		}
	}
	if cacheMountPath != "" {
		if !pathExists(cacheMountPath) {
			debugf("WARNING: cache mount path %s does not exist on the host; jail mount and startup will fail", cacheMountPath)
		}
		target := filepath.Join(jailRoot, "cache")
		debugf("ensuring jail cache mount point %s (source %s)", target, cacheMountPath)
		if err := p.makeJailDirectories(ctx, jailRoot, "cache"); err != nil {
			return fmt.Errorf("failed to create jail cache mount point %s: %w", target, err)
		}
	}
	return nil
}

// validateJailRoot rejects jail roots that would be dangerous to extract
// into, such as "/" or the base jail directory itself.
func (p *FreeBSDJailProvisioner) validateJailRoot(jailRoot string) error {
	if strings.TrimSpace(jailRoot) == "" {
		return errors.New("freebsd jail root path is empty")
	}
	if !filepath.IsAbs(jailRoot) {
		return fmt.Errorf("freebsd jail root path %q must be absolute", jailRoot)
	}
	clean := filepath.Clean(jailRoot)
	if clean == string(filepath.Separator) {
		return errors.New("refusing to use / as a freebsd jail root")
	}
	if clean == filepath.Clean(p.baseJailPath) {
		return fmt.Errorf("refusing to use base jail path %q as a jail root", clean)
	}
	return nil
}

// ensureSafeJailRoot refuses to extract into a non-empty directory that does
// not already look like a FreeBSD root. This prevents accidentally unpacking
// a base archive over an unrelated existing directory.
func ensureSafeJailRoot(jailRoot string) error {
	entries, err := os.ReadDir(jailRoot)
	if err != nil {
		return fmt.Errorf("failed to inspect jail root %s: %w", jailRoot, err)
	}
	if len(entries) == 0 {
		return nil
	}
	if _, err := os.Stat(filepath.Join(jailRoot, "bin", "sh")); err != nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		debugf("jail root %s is non-empty (%d entries) but has no bin/sh: %s", jailRoot, len(entries), strings.Join(names, ", "))
		return fmt.Errorf("refusing to extract into non-empty directory %s: it does not look like a FreeBSD jail root (contents: %s)", jailRoot, strings.Join(names, ", "))
	}
	return nil
}

// createJailConfig creates the jail configuration file.
func (p *FreeBSDJailProvisioner) createJailConfig(jailName, jailRoot, networkMode, networkInterface, ip4Address, vnetGateway, workDir, cacheMountPath, workspaceHostDir string) error {
	configPath := p.getJailConfigPath(jailName)

	// Build configuration lines
	var lines []string
	lines = append(lines, fmt.Sprintf("%s {", jailName))
	lines = append(lines, fmt.Sprintf("  path = \"%s\";", jailRoot))
	lines = append(lines, fmt.Sprintf("  host.hostname = \"jail-%s.local\";", jailName))
	if networkMode == "vnet" {
		lines = append(lines, "  vnet;")
		jibName := jibNameForJail(jailName)
		vnetInterface := "e0b_" + jibName
		lines = append(lines, fmt.Sprintf(`  vnet.interface = "%s";`, vnetInterface))
		lines = append(lines, "  devfs_ruleset = 11;")

		lines = append(lines, fmt.Sprintf(`  exec.prestart = "/usr/local/sbin/jib addm %s %s";`, jibName, networkInterface))
		if ip4Address != "" {
			// Static IP configuration
			lines = append(lines, fmt.Sprintf(`  exec.start = "/sbin/ifconfig %s %s up";`, vnetInterface, ip4Address))
			lines = append(lines, fmt.Sprintf(`  exec.start += "/sbin/route add default %s";`, vnetGateway))
			lines = append(lines, `  exec.start += "/bin/sh /etc/rc";`)
		} else {
			// DHCP configuration - use dhclient on the vnet interface
			lines = append(lines, fmt.Sprintf(`  exec.start = "/sbin/dhclient %s";`, vnetInterface))
			lines = append(lines, `  exec.start += "/bin/sh /etc/rc";`)
		}
		lines = append(lines, fmt.Sprintf(`  exec.poststop = "/usr/local/sbin/jib destroy %s";`, jibName))
	} else if ip4Address != "" {
		lines = append(lines, fmt.Sprintf("  interface = \"%s\";", networkInterface))
		lines = append(lines, fmt.Sprintf("  ip4.addr += \"%s\";", ip4Address))
	} else {
		// DHCP: inherit whatever addresses the host obtained on its
		// (DHCP-configured) interface, rather than pinning a fixed one.
		lines = append(lines, "  ip4 = inherit;")
	}
	lines = append(lines, "  mount.devfs;")
	// jail.conf mount entries are fstab(5) lines mounted on the host before
	// the jail is created, so the target is the full host path beneath the
	// jail root (which becomes the in-jail path once chrooted).
	lines = append(lines, fmt.Sprintf("  mount += \"/tmp %s nullfs rw 0 0\";", filepath.Join(jailRoot, "tmp")))

	// Add cache mount if specified
	if cacheMountPath != "" {
		lines = append(lines, fmt.Sprintf("  mount += \"%s %s nullfs rw 0 0\";", cacheMountPath, filepath.Join(jailRoot, "cache")))
	}

	// Add per-workspace host dir mount if specified
	if workspaceHostDir != "" {
		target := filepath.Join(jailRoot, filepath.Clean("/"+workDir))
		lines = append(lines, fmt.Sprintf("  mount += \"%s %s nullfs rw 0 0\";", workspaceHostDir, target))
	}

	lines = append(lines, "  exec.clean;")
	// For non-VNET mode, exec.start is set here (it wasn't set earlier)
	if networkMode != "vnet" {
		lines = append(lines, "  exec.start = \"/bin/sh /etc/rc\";")
	}
	lines = append(lines, "  exec.stop = \"/bin/sh /etc/rc.shutdown\";")
	lines = append(lines, "  allow.raw_sockets = 1;")
	// allow.sysvshm was removed in modern FreeBSD; allow.sysvipc is the
	// supported replacement (enables SysV shared memory, semaphores, and
	// message queues together).
	lines = append(lines, "  allow.sysvipc;")
	lines = append(lines, "}")

	configContent := strings.Join(lines, "\n")

	debugf("writing jail config %s:\n%s", configPath, configContent)

	// Write configuration file
	return writeFile(configPath, configContent, 0644)
}

// removeJailConfig removes the jail configuration file.
func (p *FreeBSDJailProvisioner) removeJailConfig(jailName string) error {
	configPath := p.getJailConfigPath(jailName)
	debugf("removing jail config %s", configPath)
	if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// removeJailRoot removes a workspace jail root. It is only called for roots
// this provisioning run created.
//
// Once pkg(8) has run, the tree contains root-owned files (the pkg database
// and installed packages) that the unprivileged daemon cannot delete with a
// syscall, so when escalation is configured the removal is escalated too.
func (p *FreeBSDJailProvisioner) removeJailRoot(jailRoot string) error {
	debugf("removing jail root %s", jailRoot)
	if p.escalationCommand != "" {
		return p.runPrivilegedCommand(context.Background(), "rm", "-rf", jailRoot)
	}
	return os.RemoveAll(jailRoot)
}

var (
	safeNameRE       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	jailIP4AddrRE    = regexp.MustCompile(`ip4\.addr \+= "([^"]+)"`)
	jailIfconfigIPRE = regexp.MustCompile(`/sbin/ifconfig \S+ (\S+) up`)
)

// validateRelease rejects releases that are empty or unsafe to embed in a
// filesystem path or URL.
func validateRelease(release string) error {
	if release == "" {
		return errors.New("freebsd release is required")
	}
	if strings.Contains(release, "..") || strings.ContainsAny(release, `/\`) || !safeNameRE.MatchString(release) {
		return fmt.Errorf("invalid freebsd release %q", release)
	}
	return nil
}

// validateInterface rejects interface names that are empty or unsafe to embed
// in a jail configuration file.
func validateInterface(networkInterface string) error {
	if networkInterface == "" {
		return errors.New("freebsd jail interface is required (set interface in the prepare request or -freebsd-interface)")
	}
	if strings.Contains(networkInterface, "..") || strings.ContainsAny(networkInterface, `/\ `) || !safeNameRE.MatchString(networkInterface) {
		return fmt.Errorf("invalid freebsd jail interface %q", networkInterface)
	}
	return nil
}

// validateIP4Address requires a syntactically valid IPv4 address.
func validateIP4Address(ip4Address string) error {
	if ip4Address == "" {
		return errors.New("freebsd jail ip4 address is required (set ip4_address in the prepare request or -freebsd-ip4-address)")
	}
	ip := net.ParseIP(ip4Address)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("invalid freebsd jail IPv4 address %q", ip4Address)
	}
	return nil
}

func validateVNETIP4Address(address string) error {
	ip, _, err := net.ParseCIDR(address)
	if err != nil || ip == nil || ip.To4() == nil {
		return fmt.Errorf("invalid freebsd VNET IPv4 address %q (want address/prefix)", address)
	}
	return nil
}

// extractJailIPv4 pulls a workspace's IPv4 address out of a generated jail
// configuration. Shared-stack jails record it on the ip4.addr line; VNET jails
// record it on the exec.start ifconfig line. The returned address never
// carries a prefix length.
func extractJailIPv4(config string) string {
	if m := jailIP4AddrRE.FindStringSubmatch(config); len(m) == 2 {
		return stripCIDR(m[1])
	}
	if m := jailIfconfigIPRE.FindStringSubmatch(config); len(m) == 2 {
		return stripCIDR(m[1])
	}
	return ""
}

func stripCIDR(addr string) string {
	if i := strings.IndexByte(addr, '/'); i >= 0 {
		return addr[:i]
	}
	return addr
}

// isJailNotFoundError checks if the error indicates a jail was not found.
func isJailNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "not found") ||
		strings.Contains(errStr, "no such jail") ||
		strings.Contains(errStr, "not running")
}

// isBenignJailError checks if the error is benign (jail already stopped/removed).
//
// jail(8) reports a missing jail as `jail: "wsd-ws-abc123" not found`, so the
// match is on the generic "not found" rather than a specific full sentence.
// The one "not found" that is NOT benign is the jail(8) binary itself being
// absent: that means we never ran the command at all, and the jail may still
// be up, so it must surface as a failure.
func isBenignJailError(err error) bool {
	if err == nil {
		return true
	}
	errStr := err.Error()
	if strings.Contains(errStr, "executable file not found") {
		return false
	}
	return strings.Contains(errStr, "not found") ||
		strings.Contains(errStr, "no such jail") ||
		strings.Contains(errStr, "not running") ||
		strings.Contains(errStr, "already stopped")
}

// pathExists reports whether path exists on disk.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// defaultFreeBSDArch returns the FreeBSD architecture matching the host Go
// architecture. Jails run on the same machine, so the host architecture is
// the right default.
func defaultFreeBSDArch() string {
	switch goruntime.GOARCH {
	case "arm64":
		return "arm64"
	case "386":
		return "i386"
	default:
		return "amd64"
	}
}

// freebsdMachine maps a FreeBSD architecture name to the machine segment used
// in download.freebsd.org release paths (they differ for arm64).
func freebsdMachine(arch string) string {
	if arch == "arm64" {
		return "aarch64"
	}
	return arch
}

// defaultCommandRunner runs an external command with separate arguments and
// captures its combined output so a failure reports the command's own
// diagnostics instead of a bare "exit status 1".
func defaultCommandRunner(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		if trimmed == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, trimmed)
	}
	if trimmed != "" {
		debugf("command output: %s", trimmed)
	}
	return nil
}

// defaultDownloader fetches url with fetch(1). The download is fully
// non-interactive.
func defaultDownloader(ctx context.Context, url, dest string) error {
	cmd := exec.CommandContext(ctx, "fetch", "-o", dest, url)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// writeFile is a helper to write a file with the given content and mode.
// It uses Go's native os package for reliability.
func writeFile(path, content string, mode int) error {
	// Ensure parent directory exists
	parentDir := filepath.Dir(path)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return fmt.Errorf("failed to create parent directory: %w", err)
	}

	// Write the file with the given content
	if err := os.WriteFile(path, []byte(content), os.FileMode(mode)); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

// EnsureJailBasePath creates the base jail directory if it doesn't exist.
// This is a helper method that can be called during initialization.
func (p *FreeBSDJailProvisioner) EnsureJailBasePath(ctx context.Context) error {
	debugf("ensuring base jail path %s exists", p.baseJailPath)
	return os.MkdirAll(p.baseJailPath, 0755)
}

// EnsureJailConfPath creates the jail configuration directory if it doesn't exist.
func (p *FreeBSDJailProvisioner) EnsureJailConfPath(ctx context.Context) error {
	debugf("ensuring jail config path %s exists", p.jailConfPath)
	return os.MkdirAll(p.jailConfPath, 0755)
}
