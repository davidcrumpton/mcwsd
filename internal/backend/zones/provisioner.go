// Package zones implements a runtime.Provisioner backed by illumos zones,
// with OpenIndiana as the primary target. It drives the native zone tools
// (zonecfg(8), zoneadm(8), zlogin(1), and dladm(8)) instead of recreating
// their behavior.
//
// A workspace gets its own zone named wsd-<workspace-id>. Depending on
// configuration the zone is either installed from the ipkg brand or cloned
// from an administrator-built golden zone, then booted and reached through
// zlogin. Shared-IP zones share the global zone's network stack; exclusive-IP
// zones get a dedicated VNIC and run DHCP or static addressing inside the
// zone. See ZONES.md for the host-side setup.
package zones

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
	"strings"
	"sync"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
)

const (
	defaultBrand        = "ipkg"
	defaultNetworkMode  = "shared"
	defaultBootTimeout  = 5 * time.Minute
	defaultPollInterval = 2 * time.Second
	defaultBrandDir     = "/usr/lib/brand"

	zoneNamePrefix = "wsd-"
)

// commandRunner runs an external command and returns its combined output. It
// is a field on the provisioner so tests can substitute a fake without an
// illumos host.
type commandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Options configures the zones provisioner. Empty values take their documented
// defaults. Escalation is deliberately not defaulted here: the CLI sets it to
// "pfexec", while an embedding that runs as root may leave it empty.
type Options struct {
	// ZonePath is the parent ZFS dataset under which per-workspace zone roots
	// are created, for example /rpool1/zones. It is required to provision.
	ZonePath string
	// Brand is the zone brand used when installing without a golden zone.
	Brand string
	// BaseZone is an installed, halted zone to clone for each workspace. When
	// empty, zoneadm install is used instead.
	BaseZone string
	// NetworkMode is "shared" (default) or "exclusive".
	NetworkMode string
	// Interface is the global-zone data link for shared-IP zones, or the
	// uplink from which a VNIC is created for exclusive-IP zones.
	Interface string
	// IPAddress is the shared-IP address (optionally address/prefix) or the
	// exclusive-IP static address/prefix. Empty means DHCP for exclusive mode
	// and the global zone's primary address for shared mode.
	IPAddress string
	// DefRouter is the default router for static addressing.
	DefRouter string
	// IPBaseNetwork and IPPoolSize enable automatic address allocation.
	IPBaseNetwork string
	IPPoolSize    int
	// DNSServers overrides the global zone's resolver configuration.
	DNSServers []string
	// Escalation is the privilege escalation command: pfexec, sudo, doas, or
	// empty to run privileged commands directly.
	Escalation string
	// BootTimeout bounds how long Provision waits for a new zone to boot and
	// accept zlogin.
	BootTimeout time.Duration
	// PollInterval is the readiness probe interval.
	PollInterval time.Duration
}

// ZonesProvisioner implements runtime.Provisioner using illumos zones.
type ZonesProvisioner struct {
	opts Options

	// brandDir is the parent of the per-brand installer scripts. It is a field
	// so tests can substitute a temporary directory.
	brandDir string

	// run is the side-effecting operation used during provisioning. It is a
	// field so unit tests can exercise the full flow without an illumos host.
	run      commandRunner
	lookPath func(name string) (string, error)

	// ipam lazily allocates addresses from IPBaseNetwork.
	ipamMu sync.Mutex
	ipam   *ipam
}

// NewZonesProvisioner creates a zones provisioner with the given options.
func NewZonesProvisioner(opts Options) *ZonesProvisioner {
	if opts.Brand == "" {
		opts.Brand = defaultBrand
	}
	if opts.NetworkMode == "" {
		opts.NetworkMode = defaultNetworkMode
	}
	if opts.BootTimeout <= 0 {
		opts.BootTimeout = defaultBootTimeout
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultPollInterval
	}
	return &ZonesProvisioner{
		opts:     opts,
		brandDir: defaultBrandDir,
		run:      defaultCommandRunner,
		lookPath: exec.LookPath,
	}
}

var (
	_ runtime.Provisioner          = (*ZonesProvisioner)(nil)
	_ runtime.Validator            = (*ZonesProvisioner)(nil)
	_ runtime.WorkspaceRecoverer   = (*ZonesProvisioner)(nil)
	_ runtime.WorkspaceIDDestroyer = (*ZonesProvisioner)(nil)
	_ runtime.OrphanCleaner        = (*ZonesProvisioner)(nil)
)

// Name identifies this provisioner.
func (p *ZonesProvisioner) Name() string { return "zones" }

// Init verifies that the configured privilege escalator works before the
// daemon starts serving.
func (p *ZonesProvisioner) Init(ctx context.Context) error {
	if p.opts.Escalation == "" {
		return nil
	}
	if _, err := p.runPrivileged(ctx, "zoneadm", "list", "-cv"); err != nil {
		return fmt.Errorf("privilege escalator %s is unavailable: %w", p.opts.Escalation, err)
	}
	return nil
}

// Validate checks the host configuration before the daemon starts serving: the
// zone parent is a ZFS dataset, the required tools exist, the brand installer
// is present for install mode, and escalation works.
func (p *ZonesProvisioner) Validate(ctx context.Context) error {
	if err := p.validateConfig(); err != nil {
		return err
	}
	info, err := os.Stat(p.opts.ZonePath)
	if err != nil {
		return fmt.Errorf("zones zonepath %s is not accessible: %w", p.opts.ZonePath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("zones zonepath %s is not a directory", p.opts.ZonePath)
	}

	tools := []string{"zoneadm", "zonecfg", "zlogin", "chroot", "zfs"}
	if p.opts.NetworkMode == "exclusive" {
		tools = append(tools, "dladm")
	}
	for _, name := range tools {
		if _, err := p.lookPath(name); err != nil {
			return fmt.Errorf("%s not found in PATH: %w", name, err)
		}
	}

	if _, err := p.runPrivileged(ctx, "zfs", "list", "-H", "-o", "name", p.opts.ZonePath); err != nil {
		return fmt.Errorf("zones zonepath %s is not a ZFS dataset (create it with 'zfs create <pool>/zones'): %w", p.opts.ZonePath, err)
	}

	if p.opts.BaseZone == "" {
		installer := filepath.Join(p.brandDir, p.opts.Brand, "pkgcreatezone")
		if _, err := os.Stat(installer); err != nil {
			return fmt.Errorf("zone brand %q installer %s is missing; update the system/zones package or use a golden zone: %w", p.opts.Brand, installer, err)
		}
	}

	if p.opts.Escalation != "" {
		if _, err := p.runPrivileged(ctx, "zoneadm", "list", "-cv"); err != nil {
			return fmt.Errorf("privilege escalator %s is unavailable: %w", p.opts.Escalation, err)
		}
	}

	if p.opts.Interface != "" {
		if err := p.validateInterfaceExists(ctx, p.opts.Interface); err != nil {
			return err
		}
	}
	return nil
}

// Provision creates, installs or clones, and boots a zone for the workspace.
func (p *ZonesProvisioner) Provision(ctx context.Context, spec runtime.ProvisionSpec) (*runtime.RuntimeHandle, error) {
	if err := p.validateConfig(); err != nil {
		return nil, err
	}
	if err := validateWorkspaceID(spec.WorkspaceID); err != nil {
		return nil, err
	}

	zoneName := zoneNamePrefix + spec.WorkspaceID
	if err := validateZoneName(zoneName); err != nil {
		return nil, err
	}
	zonePath := p.zonePath(zoneName)
	if err := validateZonePath(zonePath); err != nil {
		return nil, err
	}

	mode := p.opts.NetworkMode
	iface := p.opts.Interface
	ipAddress := p.opts.IPAddress
	router := p.opts.DefRouter
	if spec.Interface != "" {
		iface = spec.Interface
	}
	if spec.IP4Address != "" {
		ipAddress = spec.IP4Address
	}

	if ipAddress == "" && p.opts.IPBaseNetwork != "" && p.opts.IPPoolSize > 0 {
		alloc, err := p.getIPAM()
		if err != nil {
			return nil, err
		}
		ipAddress, err = alloc.allocate(spec.WorkspaceID)
		if err != nil {
			return nil, err
		}
		if mode == "exclusive" && router == "" {
			router = alloc.gateway().String()
		}
		debugf("auto-assigned address %s to workspace %s from pool %s", ipAddress, spec.WorkspaceID, p.opts.IPBaseNetwork)
	}

	if mode == "shared" {
		if iface == "" {
			return nil, errors.New("zones shared networking requires a data link (-zones-interface)")
		}
		if err := validateInterface(iface); err != nil {
			return nil, err
		}
		if ipAddress == "" {
			addr, err := p.globalZonePrimaryIPv4(ctx)
			if err != nil {
				return nil, err
			}
			ipAddress = addr
		}
		if err := validateAddress(ipAddress, false); err != nil {
			return nil, err
		}
		if router != "" {
			if err := validateRouter(router); err != nil {
				return nil, err
			}
		}
	} else {
		if iface == "" {
			return nil, errors.New("zones exclusive networking requires an uplink interface (-zones-interface)")
		}
		if err := validateInterface(iface); err != nil {
			return nil, err
		}
		if ipAddress != "" {
			if err := validateAddress(ipAddress, true); err != nil {
				return nil, err
			}
			if router == "" {
				return nil, errors.New("static exclusive-IP requires a default router (-zones-defrouter)")
			}
			if err := validateRouter(router); err != nil {
				return nil, err
			}
			if len(p.opts.DNSServers) == 0 {
				return nil, errors.New("static exclusive-IP requires explicit DNS servers (-zones-dns-servers)")
			}
		} else if router != "" {
			return nil, errors.New("zones default router provided without an IPv4 address; omit it for DHCP")
		}
	}
	if err := p.validateDNSServers(); err != nil {
		return nil, err
	}

	// The parent directory must already exist; it is the ZFS dataset that
	// zoneadm will create the per-zone dataset under. Never create it as a
	// plain directory.
	if info, err := os.Stat(p.opts.ZonePath); err != nil {
		return nil, fmt.Errorf("zones zonepath %s is not accessible: %w", p.opts.ZonePath, err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("zones zonepath %s is not a directory", p.opts.ZonePath)
	}

	workspaceHostDir := ""
	if spec.WorkspaceBasePath != "" {
		if !filepath.IsAbs(spec.WorkspaceBasePath) {
			return nil, fmt.Errorf("workspace base path %q must be absolute for zones", spec.WorkspaceBasePath)
		}
		workspaceHostDir = filepath.Join(spec.WorkspaceBasePath, spec.WorkspaceID)
		if err := os.MkdirAll(workspaceHostDir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create workspace host directory %s: %w", workspaceHostDir, err)
		}
	}
	if spec.CacheMountPath != "" && !filepath.IsAbs(spec.CacheMountPath) {
		return nil, fmt.Errorf("cache mount path %q must be absolute for zones", spec.CacheMountPath)
	}

	vnic := ""
	if mode == "exclusive" {
		vnic = vnicNameForZone(zoneName)
		if err := p.ensureVNIC(ctx, iface, vnic); err != nil {
			return nil, err
		}
	}
	physical := iface
	if mode == "exclusive" {
		physical = vnic
	}

	// A previous failed run may have left a partial configuration behind.
	// Removing it first makes provisioning retryable. The zone name is derived
	// from the workspace ID, so it cannot collide with another live workspace.
	p.deleteZoneConfig(ctx, zoneName)

	removeNet, removeFS := false, false
	if p.opts.BaseZone != "" {
		var err error
		removeNet, err = p.zoneHasResource(ctx, p.opts.BaseZone, "net")
		if err != nil {
			return nil, err
		}
		removeFS, err = p.zoneHasResource(ctx, p.opts.BaseZone, "fs")
		if err != nil {
			return nil, err
		}
	}

	config := buildZoneConfig(zoneConfig{
		Name:             zoneName,
		ZonePath:         zonePath,
		Brand:            p.opts.Brand,
		BaseZone:         p.opts.BaseZone,
		NetworkMode:      mode,
		Physical:         physical,
		Address:          ipAddress,
		DefRouter:        router,
		WorkDir:          spec.WorkDir,
		WorkspaceHostDir: workspaceHostDir,
		CacheMountPath:   spec.CacheMountPath,
		RemoveNet:        removeNet,
		RemoveFS:         removeFS,
	})
	if err := p.applyZoneConfig(ctx, zoneName, config); err != nil {
		p.cleanupFailedProvision(ctx, zoneName)
		return nil, err
	}

	if p.opts.BaseZone != "" {
		debugf("cloning zone %s from golden zone %s", zoneName, p.opts.BaseZone)
		// -X skips the brand's sys-unconfig so the workspace keeps the golden
		// zone's completed identity (root password, host keys, sysding) and
		// boots without re-running sysding. Without it the ipkg brand's
		// system-unconfigure step can fail on a golden zone that has not been
		// freshly unconfigured.
		if _, err := p.runPrivileged(ctx, "zoneadm", "-z", zoneName, "clone", "-X", p.opts.BaseZone); err != nil {
			p.cleanupFailedProvision(ctx, zoneName)
			return nil, fmt.Errorf("failed to clone zone %s from %s: %w", zoneName, p.opts.BaseZone, err)
		}
	} else {
		debugf("installing zone %s", zoneName)
		if _, err := p.runPrivileged(ctx, "zoneadm", "-z", zoneName, "install"); err != nil {
			p.cleanupFailedProvision(ctx, zoneName)
			return nil, fmt.Errorf("failed to install zone %s: %w", zoneName, err)
		}
	}

	if err := p.ensureZoneMountPoints(ctx, zonePath, spec.WorkDir, spec.CacheMountPath); err != nil {
		p.cleanupFailedProvision(ctx, zoneName)
		return nil, err
	}

	if err := p.writeZoneResolvConf(ctx, zonePath); err != nil {
		p.cleanupFailedProvision(ctx, zoneName)
		return nil, err
	}

	// A zone cloned with -X is left "mounted" (the clone script mounts the
	// root and skips sys-unconfig). zoneadm boot rejects that state, so
	// unmount it back to "installed" first.
	if p.opts.BaseZone != "" {
		if _, err := p.runPrivileged(ctx, "zoneadm", "-z", zoneName, "unmount"); err != nil {
			p.cleanupFailedProvision(ctx, zoneName)
			return nil, fmt.Errorf("failed to unmount cloned zone %s: %w", zoneName, err)
		}
	}

	debugf("booting zone %s", zoneName)
	if _, err := p.runPrivileged(ctx, "zoneadm", "-z", zoneName, "boot"); err != nil {
		p.cleanupFailedProvision(ctx, zoneName)
		return nil, fmt.Errorf("failed to boot zone %s: %w", zoneName, err)
	}

	if err := p.waitForZoneReady(ctx, zoneName); err != nil {
		p.cleanupFailedProvision(ctx, zoneName)
		return nil, err
	}

	if mode == "exclusive" {
		if err := p.configureExclusiveNetwork(ctx, zoneName, vnic, ipAddress, router); err != nil {
			p.cleanupFailedProvision(ctx, zoneName)
			return nil, err
		}
	}

	debugf("zone %s ready: mode=%s physical=%s address=%s workDir=%s", zoneName, mode, physical, ipAddress, spec.WorkDir)
	return &runtime.RuntimeHandle{
		OS:        runtime.OSIllumos,
		Isolation: runtime.IsolationZone,
		Username:  "root",
		Privilege: runtime.PrivilegeRoot,
		WorkDir:   spec.WorkDir,
		NativeID:  zoneName,
		Transport: NewZonesExecTransport(zoneName, p.opts.Escalation, mode, vnic),
	}, nil
}

// Destroy stops and removes the zone behind handle.
func (p *ZonesProvisioner) Destroy(ctx context.Context, handle *runtime.RuntimeHandle) error {
	return p.DestroyByID(ctx, handle.NativeID)
}

// DestroyByID halts and uninstalls a zone by name, removes its configuration,
// and deletes its VNIC when exclusive networking was used. It is idempotent:
// missing/running states that are already gone are treated as success.
func (p *ZonesProvisioner) DestroyByID(ctx context.Context, zoneName string) error {
	if err := validateZoneName(zoneName); err != nil {
		return err
	}
	debugf("destroying zone %s", zoneName)

	if _, err := p.runPrivileged(ctx, "zoneadm", "-z", zoneName, "halt"); err != nil && !isBenignZoneError(err) {
		debugf("halt zone %s reported: %v (continuing)", zoneName, err)
	}

	// A zone left "mounted" (e.g. a clone with -X that never booted) must be
	// unmounted before it can be uninstalled.
	if _, err := p.runPrivileged(ctx, "zoneadm", "-z", zoneName, "unmount"); err != nil && !isBenignZoneError(err) {
		debugf("unmount zone %s reported: %v (continuing)", zoneName, err)
	}

	if _, err := p.runPrivileged(ctx, "zoneadm", "-z", zoneName, "uninstall", "-F"); err != nil && !isBenignZoneError(err) {
		// Leave the config in place so a later retry can finish the job.
		return fmt.Errorf("failed to uninstall zone %s: %w", zoneName, err)
	}

	if _, err := p.runPrivileged(ctx, "zonecfg", "-z", zoneName, "delete", "-F"); err != nil && !isBenignZoneError(err) {
		return fmt.Errorf("failed to delete zone configuration %s: %w", zoneName, err)
	}

	// zoneadm uninstall (via the ipkg brand script) leaves a cloned zone's ZFS
	// datasets and the golden zone's clone snapshot behind. Destroy them
	// explicitly so repeated provision/destroy cycles do not accumulate.
	p.destroyZoneDatasets(ctx, zoneName)

	if p.opts.NetworkMode == "exclusive" {
		if err := p.deleteVNIC(ctx, vnicNameForZone(zoneName)); err != nil {
			debugf("failed to delete VNIC for zone %s: %v", zoneName, err)
		}
	}
	return nil
}

// destroyZoneDatasets removes the zone's ZFS datasets and, when the zone was
// cloned from a golden zone, the source snapshot zoneadm clone left on it.
// It is best-effort: the zone may not have been installed or cloned yet.
func (p *ZonesProvisioner) destroyZoneDatasets(ctx context.Context, zoneName string) {
	if _, err := p.runPrivileged(ctx, "zfs", "destroy", "-r", p.zonePath(zoneName)); err != nil {
		debugf("zfs destroy %s reported: %v", p.zonePath(zoneName), err)
	}
	if p.opts.BaseZone != "" {
		p.destroyCloneSnapshot(ctx, zoneName)
	}
}

// destroyCloneSnapshot destroys the "<zone>_snap" snapshot that zoneadm clone
// created on the golden zone's active boot environment.
func (p *ZonesProvisioner) destroyCloneSnapshot(ctx context.Context, zoneName string) {
	golden := p.zonePath(p.opts.BaseZone)
	out, err := p.runPrivileged(ctx, "zfs", "list", "-H", "-t", "snapshot", "-o", "name", "-r", golden)
	if err != nil {
		debugf("listing golden zone snapshots failed: %v", err)
		return
	}
	prefix := zoneName + "_snap"
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		at := strings.LastIndex(line, "@")
		if at < 0 || !strings.HasPrefix(line[at+1:], prefix) {
			continue
		}
		if _, err := p.runPrivileged(ctx, "zfs", "destroy", line); err != nil {
			debugf("zfs destroy %s reported: %v", line, err)
		}
	}
}

// DestroyWorkspaceByID destroys the zone whose native name is derived from its
// workspace ID.
func (p *ZonesProvisioner) DestroyWorkspaceByID(ctx context.Context, workspaceID string) error {
	if err := validateWorkspaceID(workspaceID); err != nil {
		return err
	}
	return p.DestroyByID(ctx, zoneNamePrefix+workspaceID)
}

// RecoverHandle creates a RuntimeHandle for an existing zone after a daemon
// restart, booting it when it is installed but halted.
func (p *ZonesProvisioner) RecoverHandle(ctx context.Context, nativeID string, workDir string) (*runtime.RuntimeHandle, error) {
	if err := validateZoneName(nativeID); err != nil {
		return nil, err
	}
	state, err := p.zoneState(ctx, nativeID)
	if err != nil {
		return nil, fmt.Errorf("zone %s not found: %w", nativeID, err)
	}

	switch state {
	case "running":
		// Already usable.
	case "ready":
		if err := p.waitForZoneReady(ctx, nativeID); err != nil {
			return nil, err
		}
	case "installed", "down":
		if _, err := p.runPrivileged(ctx, "zoneadm", "-z", nativeID, "boot"); err != nil {
			return nil, fmt.Errorf("failed to boot recovered zone %s: %w", nativeID, err)
		}
		if err := p.waitForZoneReady(ctx, nativeID); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("zone %s is in state %q and cannot be recovered", nativeID, state)
	}

	vnic := ""
	if p.opts.NetworkMode == "exclusive" {
		vnic = vnicNameForZone(nativeID)
	}
	return &runtime.RuntimeHandle{
		OS:        runtime.OSIllumos,
		Isolation: runtime.IsolationZone,
		Username:  "root",
		Privilege: runtime.PrivilegeRoot,
		WorkDir:   workDir,
		NativeID:  nativeID,
		Transport: NewZonesExecTransport(nativeID, p.opts.Escalation, p.opts.NetworkMode, vnic),
	}, nil
}

// CleanupOrphans removes wsd-* zones that are no longer tracked in the state
// file. The configured golden zone is never removed.
func (p *ZonesProvisioner) CleanupOrphans(ctx context.Context, keep map[string]bool) (int, error) {
	names := make(map[string]bool)

	if p.opts.ZonePath != "" {
		entries, err := os.ReadDir(p.opts.ZonePath)
		if err != nil && !os.IsNotExist(err) {
			return 0, fmt.Errorf("failed to scan zones zonepath %s: %w", p.opts.ZonePath, err)
		}
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), zoneNamePrefix) {
				names[e.Name()] = true
			}
		}
	}

	if out, err := p.runPrivileged(ctx, "zoneadm", "list", "-pc"); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Split(line, ":")
			if len(fields) >= 2 && strings.HasPrefix(fields[1], zoneNamePrefix) {
				names[fields[1]] = true
			}
		}
	}

	removed := 0
	for name := range names {
		if keep[name] || name == p.opts.BaseZone {
			continue
		}
		if err := p.DestroyByID(ctx, name); err != nil {
			debugf("failed to remove orphan zone %s: %v", name, err)
			continue
		}
		removed++
	}
	return removed, nil
}

// --- configuration helpers ---

func (p *ZonesProvisioner) validateConfig() error {
	if err := validateZonePath(p.opts.ZonePath); err != nil {
		return fmt.Errorf("invalid zones zonepath: %w", err)
	}
	if p.opts.NetworkMode != "shared" && p.opts.NetworkMode != "exclusive" {
		return fmt.Errorf("unsupported zones network mode %q (want shared or exclusive)", p.opts.NetworkMode)
	}
	if (p.opts.IPBaseNetwork == "") != (p.opts.IPPoolSize == 0) {
		return errors.New("zones IP base network and pool size must be set together")
	}
	if p.opts.IPBaseNetwork != "" {
		if _, _, err := net.ParseCIDR(p.opts.IPBaseNetwork); err != nil {
			return fmt.Errorf("invalid zones IP base network %q: %w", p.opts.IPBaseNetwork, err)
		}
		if p.opts.IPPoolSize < 1 {
			return fmt.Errorf("zones IP pool size must be positive, got %d", p.opts.IPPoolSize)
		}
	}
	return p.validateDNSServers()
}

func (p *ZonesProvisioner) validateDNSServers() error {
	for _, server := range p.opts.DNSServers {
		if net.ParseIP(server) == nil {
			return fmt.Errorf("invalid zones DNS server IP %q", server)
		}
	}
	return nil
}

func (p *ZonesProvisioner) zonePath(zoneName string) string {
	return filepath.Join(p.opts.ZonePath, zoneName)
}

// globalZonePrimaryIPv4 returns the global zone's first non-loopback IPv4
// address, used for shared-IP host networking when no address is configured.
func (p *ZonesProvisioner) globalZonePrimaryIPv4(ctx context.Context) (string, error) {
	out, err := p.runPrivileged(ctx, "ipadm", "show-addr", "-p", "-o", "addr")
	if err != nil {
		return "", fmt.Errorf("failed to determine the global zone IPv4 address; pass --zones-ip-address: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "127.") {
			continue
		}
		ipStr := line
		if i := strings.Index(ipStr, "/"); i >= 0 {
			ipStr = ipStr[:i]
		}
		ip := net.ParseIP(ipStr)
		if ip == nil || ip.To4() == nil || ip.IsLoopback() {
			continue
		}
		return line, nil
	}
	return "", errors.New("could not determine the global zone IPv4 address; pass --zones-ip-address")
}

func (p *ZonesProvisioner) getIPAM() (*ipam, error) {
	p.ipamMu.Lock()
	defer p.ipamMu.Unlock()
	if p.ipam == nil {
		alloc, err := newIPAM(p.opts.IPBaseNetwork, p.opts.IPPoolSize)
		if err != nil {
			return nil, err
		}
		p.ipam = alloc
	}
	return p.ipam, nil
}

// validateInterfaceExists verifies the configured data link is present in the
// global zone so a wrong --zones-interface fails at startup with a clear
// message instead of surfacing later inside an async provision job.
func (p *ZonesProvisioner) validateInterfaceExists(ctx context.Context, iface string) error {
	out, err := p.runPrivileged(ctx, "dladm", "show-link", "-p", "-o", "link")
	if err != nil {
		return fmt.Errorf("failed to list data links: %w", err)
	}
	links := make([]string, 0)
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			links = append(links, line)
			if line == iface {
				return nil
			}
		}
	}
	return fmt.Errorf("zones interface %q does not exist; available data links: %s (find it with 'dladm show-phys')",
		iface, strings.Join(links, ", "))
}

func (p *ZonesProvisioner) ensureVNIC(ctx context.Context, uplink, vnic string) error {
	if out, err := p.runPrivileged(ctx, "dladm", "show-vnic", "-p", "-o", "link"); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.TrimSpace(line) == vnic {
				return nil
			}
		}
	}
	if _, err := p.runPrivileged(ctx, "dladm", "create-vnic", "-l", uplink, vnic); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "already exists") {
			return nil
		}
		return fmt.Errorf("failed to create VNIC %s on %s: %w", vnic, uplink, err)
	}
	return nil
}

func (p *ZonesProvisioner) deleteVNIC(ctx context.Context, vnic string) error {
	if vnic == "" {
		return nil
	}
	if _, err := p.runPrivileged(ctx, "dladm", "delete-vnic", vnic); err != nil && !isBenignZoneError(err) {
		return err
	}
	return nil
}

func (p *ZonesProvisioner) deleteZoneConfig(ctx context.Context, zoneName string) {
	_, _ = p.runPrivileged(ctx, "zonecfg", "-z", zoneName, "delete", "-F")
}

// zoneHasResource reports whether a configured zone has at least one resource
// of the given type (e.g. "net" or "fs"). zonecfg's "info <type>" prints the
// resource section only when the type is present.
func (p *ZonesProvisioner) zoneHasResource(ctx context.Context, zoneName, resType string) (bool, error) {
	out, err := p.runPrivileged(ctx, "zonecfg", "-z", zoneName, "info", resType)
	if err != nil {
		return false, fmt.Errorf("failed to inspect zone %s resources: %w", zoneName, err)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func (p *ZonesProvisioner) applyZoneConfig(ctx context.Context, zoneName, script string) error {
	debugf("writing zonecfg for %s:\n%s", zoneName, strings.TrimSpace(script))
	f, err := os.CreateTemp("", "wsd-zonecfg-*")
	if err != nil {
		return fmt.Errorf("failed to create temporary zonecfg file: %w", err)
	}
	path := f.Name()
	defer os.Remove(path)

	if _, err := f.WriteString(script); err != nil {
		f.Close()
		return fmt.Errorf("failed to write temporary zonecfg file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close temporary zonecfg file: %w", err)
	}

	if _, err := p.runPrivileged(ctx, "zonecfg", "-z", zoneName, "-f", path); err != nil {
		return fmt.Errorf("zonecfg failed for %s: %w", zoneName, err)
	}
	return nil
}

func (p *ZonesProvisioner) ensureZoneMountPoints(ctx context.Context, zonePath, workDir, cacheMountPath string) error {
	root := filepath.Join(zonePath, "root")
	var dirs []string
	if workDir != "" {
		dirs = append(dirs, filepath.Clean("/"+workDir))
	}
	if cacheMountPath != "" {
		dirs = append(dirs, "/cache")
	}
	if len(dirs) == 0 {
		return nil
	}
	// Run mkdir inside a chroot so a symlink in the zone image cannot redirect
	// the privileged write outside the zone root.
	args := append([]string{root, "/usr/bin/mkdir", "-p"}, dirs...)
	if _, err := p.runPrivileged(ctx, "chroot", args...); err != nil {
		return fmt.Errorf("failed to create zone mount points: %w", err)
	}
	return nil
}

func (p *ZonesProvisioner) writeZoneResolvConf(ctx context.Context, zonePath string) error {
	data, err := p.resolvConf()
	if err != nil {
		return err
	}
	root := filepath.Join(zonePath, "root")
	// chroot confines both the mkdir and the resolver write to the zone root,
	// so an absolute /etc symlink in the image cannot escape into the global
	// zone.
	if _, err := p.runPrivileged(ctx, "chroot", root, "/bin/sh", "-c",
		`umask 022; /usr/bin/mkdir -p /etc; printf '%s' "$1" > /etc/resolv.conf`,
		"mcwsd-dns", string(data)); err != nil {
		return fmt.Errorf("failed to write zone resolver configuration: %w", err)
	}
	return nil
}

func (p *ZonesProvisioner) resolvConf() ([]byte, error) {
	if len(p.opts.DNSServers) > 0 {
		var b strings.Builder
		for _, server := range p.opts.DNSServers {
			b.WriteString("nameserver ")
			b.WriteString(server)
			b.WriteByte('\n')
		}
		return []byte(b.String()), nil
	}
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil, fmt.Errorf("failed to read host resolver configuration: %w", err)
	}
	return data, nil
}

func (p *ZonesProvisioner) configureExclusiveNetwork(ctx context.Context, zoneName, vnic, ipAddress, router string) error {
	// Guard each step so the script is idempotent: the zone's datalink/ipadm
	// services are not necessarily ready the moment zlogin first succeeds, and
	// the whole script is retried below until it configures cleanly.
	script := "ipadm show-if " + vnic + " >/dev/null 2>&1 || ipadm create-ip " + vnic + "; "
	if ipAddress == "" {
		script += "ipadm show-addr " + vnic + "/v4 >/dev/null 2>&1 || ipadm create-addr -T dhcp " + vnic + "/v4"
	} else {
		script += "ipadm show-addr " + vnic + "/v4 >/dev/null 2>&1 || ipadm create-addr -T static -a " + ipAddress + " " + vnic + "/v4"
		if router != "" {
			script += "; route -p add default " + router + " 2>/dev/null || true"
		}
	}
	script += "; ifconfig " + vnic + " up"

	deadline := time.Now().Add(p.opts.BootTimeout)
	var lastErr error
	for {
		// zlogin re-parses command arguments through su -c, so the script must
		// be shell-quoted as a single argument to survive intact.
		if _, err := p.runPrivileged(ctx, "zlogin", zoneName, "sh", "-c", quoteArg(script)); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.opts.PollInterval):
		}
	}
	return fmt.Errorf("failed to configure exclusive networking in zone %s: %w", zoneName, lastErr)
}

func (p *ZonesProvisioner) waitForZoneReady(ctx context.Context, zoneName string) error {
	deadline := time.Now().Add(p.opts.BootTimeout)
	var lastErr error
	for {
		state, err := p.zoneState(ctx, zoneName)
		if err == nil && state == "running" {
			if _, err := p.runPrivileged(ctx, "zlogin", zoneName, "true"); err == nil {
				return nil
			} else {
				lastErr = err
			}
		} else if err != nil {
			lastErr = err
		}

		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("timed out waiting for zone %s to accept zlogin: %w (a golden zone that completed sysding is recommended)", zoneName, lastErr)
			}
			return fmt.Errorf("timed out waiting for zone %s to become ready (first boot may be waiting on sysding; use a golden zone for unattended provisioning)", zoneName)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.opts.PollInterval):
		}
	}
}

func (p *ZonesProvisioner) zoneState(ctx context.Context, zoneName string) (string, error) {
	out, err := p.runPrivileged(ctx, "zoneadm", "list", "-pc")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) >= 3 && fields[1] == zoneName {
			return fields[2], nil
		}
	}
	return "", fmt.Errorf("zone %s not found", zoneName)
}

func (p *ZonesProvisioner) cleanupFailedProvision(ctx context.Context, zoneName string) {
	debugf("cleaning up failed provision for zone %s", zoneName)
	// A zone cloned with -X is left "mounted", not "installed"; halt is a
	// no-op for that state, so also unmount before uninstalling.
	_, _ = p.runPrivileged(ctx, "zoneadm", "-z", zoneName, "halt")
	_, _ = p.runPrivileged(ctx, "zoneadm", "-z", zoneName, "unmount")
	_, _ = p.runPrivileged(ctx, "zoneadm", "-z", zoneName, "uninstall", "-F")
	_, _ = p.runPrivileged(ctx, "zonecfg", "-z", zoneName, "delete", "-F")
	p.destroyZoneDatasets(ctx, zoneName)
	if p.opts.NetworkMode == "exclusive" {
		_ = p.deleteVNIC(ctx, vnicNameForZone(zoneName))
	}
}

// --- command runners ---

func (p *ZonesProvisioner) runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	debugCommand(name, args...)
	return p.run(ctx, name, args...)
}

func (p *ZonesProvisioner) runPrivileged(ctx context.Context, name string, args ...string) ([]byte, error) {
	if p.opts.Escalation == "" {
		return p.runCommand(ctx, name, args...)
	}
	escalated := append([]string{name}, args...)
	return p.runCommand(ctx, p.opts.Escalation, escalated...)
}

// --- zone configuration file ---

type zoneConfig struct {
	Name             string
	ZonePath         string
	Brand            string
	BaseZone         string
	NetworkMode      string
	Physical         string
	Address          string
	DefRouter        string
	WorkDir          string
	WorkspaceHostDir string
	CacheMountPath   string
	// RemoveNet and RemoveFS control whether the template's net/fs resources
	// are stripped after "create -t". They are set from the golden zone's
	// actual resources so a golden zone without one of them does not fail.
	RemoveNet bool
	RemoveFS  bool
}

func buildZoneConfig(c zoneConfig) string {
	var lines []string

	if c.BaseZone != "" {
		lines = append(lines, "create -t "+zonecfgValue(c.BaseZone))
		// The template's network and file-system resources are replaced with
		// the workspace-specific ones below. Only strip resources the golden
		// zone actually has; "remove" on a missing resource is an error.
		if c.RemoveNet {
			lines = append(lines, "remove net")
		}
		if c.RemoveFS {
			lines = append(lines, "remove fs")
		}
	} else {
		lines = append(lines, "create")
		lines = append(lines, "set brand="+zonecfgValue(c.Brand))
	}
	lines = append(lines,
		"set zonepath="+zonecfgValue(c.ZonePath),
		"set autoboot=false",
		"set ip-type="+c.NetworkMode,
		"add net",
		"set physical="+zonecfgValue(c.Physical),
	)
	if c.NetworkMode == "shared" && c.Address != "" {
		lines = append(lines, "set address="+zonecfgValue(c.Address))
		if c.DefRouter != "" {
			lines = append(lines, "set defrouter="+zonecfgValue(c.DefRouter))
		}
	}
	lines = append(lines, "end")

	if c.WorkspaceHostDir != "" && c.WorkDir != "" {
		lines = append(lines,
			"add fs",
			"set dir="+zonecfgValue(c.WorkDir),
			"set special="+zonecfgValue(c.WorkspaceHostDir),
			"set type=lofs",
			"set options=rw",
			"end",
		)
	}
	if c.CacheMountPath != "" {
		lines = append(lines,
			"add fs",
			"set dir=/cache",
			"set special="+zonecfgValue(c.CacheMountPath),
			"set type=lofs",
			"set options=rw",
			"end",
		)
	}
	lines = append(lines, "verify", "commit")
	return strings.Join(lines, "\n") + "\n"
}

// zonecfgValue quotes a value for the zonecfg command-file parser when it
// contains whitespace or characters the parser treats specially.
func zonecfgValue(value string) string {
	if value == "" {
		return `""`
	}
	if !strings.ContainsAny(value, " \t\n\"'\\;") {
		return value
	}
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

// --- validation ---

var (
	safeNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

func validateWorkspaceID(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("workspace id is required")
	}
	if strings.Contains(id, "..") || strings.ContainsAny(id, `/\`) || !safeNameRE.MatchString(id) {
		return fmt.Errorf("invalid workspace id %q", id)
	}
	return nil
}

func validateZoneName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("zone name is required")
	}
	if !strings.HasPrefix(name, zoneNamePrefix) {
		return fmt.Errorf("zone name %q must start with %q", name, zoneNamePrefix)
	}
	if strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) || !safeNameRE.MatchString(name) {
		return fmt.Errorf("invalid zone name %q", name)
	}
	return nil
}

func validateZonePath(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("zones zonepath is required (-zones-zonepath)")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("zones zonepath %q must be absolute", path)
	}
	if filepath.Clean(path) == string(filepath.Separator) {
		return errors.New("refusing to use / as the zones zonepath")
	}
	return nil
}

func validateInterface(iface string) error {
	if strings.TrimSpace(iface) == "" {
		return errors.New("zones network interface is required (-zones-interface)")
	}
	if strings.Contains(iface, "..") || strings.ContainsAny(iface, `/\ `) || !safeNameRE.MatchString(iface) {
		return fmt.Errorf("invalid zones network interface %q", iface)
	}
	return nil
}

func validateAddress(address string, requireCIDR bool) error {
	if requireCIDR {
		ip, _, err := net.ParseCIDR(address)
		if err != nil || ip == nil || ip.To4() == nil {
			return fmt.Errorf("invalid zones IPv4 address %q (want address/prefix)", address)
		}
		return nil
	}
	if ip, _, err := net.ParseCIDR(address); err == nil && ip.To4() != nil {
		return nil
	}
	ip := net.ParseIP(address)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("invalid zones IPv4 address %q", address)
	}
	return nil
}

func validateRouter(router string) error {
	ip := net.ParseIP(router)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("invalid zones default router %q", router)
	}
	return nil
}

// vnicNameForZone derives a stable, short VNIC name from the zone name.
//
// illumos data-link names are limited to 31 characters and, per
// dladm_valid_linkname(3DADM), must consist of alphanumerics, '_' and '.',
// must not start with a digit, and must end in a number that has no leading
// zero. Workspace IDs are hex strings, so they may end in a letter (which
// dladm rejects). A trailing digit is appended when needed so the derived
// name is always accepted by dladm.
func vnicNameForZone(zoneName string) string {
	base := strings.ReplaceAll(zoneName, "-", "")
	if len(base) > 20 {
		sum := sha256.Sum256([]byte(zoneName))
		base = fmt.Sprintf("z%x", sum[:8])
	}
	if !validDladmLinkName(base) {
		base = strings.TrimRightFunc(base, func(r rune) bool {
			return r >= '0' && r <= '9'
		}) + "0"
	}
	return base
}

// validDladmLinkName reports whether name satisfies the constraints enforced
// by dladm_valid_linkname(3DADM): under 32 characters, not starting with a
// digit, using only alphanumerics plus '_' and '.', and ending in a number
// without a leading zero.
func validDladmLinkName(name string) bool {
	if name == "" || len(name) >= 32 {
		return false
	}
	if name[0] >= '0' && name[0] <= '9' {
		return false
	}
	if c := name[len(name)-1]; c < '0' || c > '9' {
		return false
	}
	i := len(name) - 1
	for i >= 0 && name[i] >= '0' && name[i] <= '9' {
		i--
	}
	if len(name)-1-i > 1 && name[i+1] == '0' {
		return false
	}
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '.' {
			continue
		}
		return false
	}
	return true
}

func isBenignZoneError(err error) bool {
	if err == nil {
		return true
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "executable file not found") {
		return false
	}
	for _, marker := range []string{
		"not found",
		"no such zone",
		"does not exist",
		"not installed",
		"not running",
		"already stopped",
		"already halted",
		"is not configured",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// --- logging helpers ---

func debugCommand(name string, args ...string) {
	internal.Log.Debug(formatCommand(name, args...))
}

func debugf(format string, args ...interface{}) {
	internal.Log.Debug(fmt.Sprintf(format, args...))
}

func formatCommand(name string, args ...string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, quoteArg(name))
	for _, a := range args {
		parts = append(parts, quoteArg(a))
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

// --- default command implementations ---

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

// createTarArchive creates a tar archive of srcDir rooted at "." for streaming
// into a zone.
func createTarArchive(srcDir string) (string, error) {
	tmpFile, err := os.CreateTemp("", "wsd-zone-copy-*.tar")
	if err != nil {
		return "", err
	}
	tmpFile.Close()

	debugCommand("tar", "-cf", tmpFile.Name(), "-C", srcDir, ".")
	cmd := exec.Command("tar", "-cf", tmpFile.Name(), "-C", srcDir, ".")
	if err := cmd.Run(); err != nil {
		os.Remove(tmpFile.Name())
		return "", err
	}
	return tmpFile.Name(), nil
}
