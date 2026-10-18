package freebsd

import (
	"archive/tar"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal/runtime"
)

func TestNewFreeBSDJailProvisioner(t *testing.T) {
	p := NewFreeBSDJailProvisioner("", "", "", "", "")
	if p == nil {
		t.Fatal("NewFreeBSDJailProvisioner returned nil")
	}
}

func TestNewFreeBSDJailProvisioner_WithCustomPaths(t *testing.T) {
	p := NewFreeBSDJailProvisioner("/custom/jail/conf", "/custom/jails", "13.2-RELEASE", "vtnet0", "192.168.122.100")
	if p == nil {
		t.Fatal("NewFreeBSDJailProvisioner returned nil")
	}
	if p.jailConfPath != "/custom/jail/conf" {
		t.Errorf("jailConfPath = %q, want %q", p.jailConfPath, "/custom/jail/conf")
	}
	if p.baseJailPath != "/custom/jails" {
		t.Errorf("baseJailPath = %q, want %q", p.baseJailPath, "/custom/jails")
	}
	if p.release != "13.2-RELEASE" {
		t.Errorf("release = %q, want %q", p.release, "13.2-RELEASE")
	}
	if p.networkInterface != "vtnet0" {
		t.Errorf("networkInterface = %q, want %q", p.networkInterface, "vtnet0")
	}
	if p.ip4Address != "192.168.122.100" {
		t.Errorf("ip4Address = %q, want %q", p.ip4Address, "192.168.122.100")
	}
}

func TestNewFreeBSDJailProvisioner_WithDefaults(t *testing.T) {
	p := NewFreeBSDJailProvisioner("", "", "", "", "")
	if p.jailConfPath != "/etc/jail.conf.d" {
		t.Errorf("jailConfPath = %q, want %q", p.jailConfPath, "/etc/jail.conf.d")
	}
	if p.baseJailPath != "/usr/local/jails" {
		t.Errorf("baseJailPath = %q, want %q", p.baseJailPath, "/usr/local/jails")
	}
	if p.release != "15.1-RELEASE" {
		t.Errorf("release = %q, want %q", p.release, "15.1-RELEASE")
	}
}

// TestDefaultReleaseIs151 verifies requirement 1: the default release is 15.1-RELEASE.
func TestDefaultReleaseIs151(t *testing.T) {
	p := NewFreeBSDJailProvisioner("", "", "", "", "")
	if p.release != "15.1-RELEASE" {
		t.Fatalf("default release = %q, want %q", p.release, "15.1-RELEASE")
	}
}

// TestCallerCanSpecifyRelease verifies requirement 2.
func TestCallerCanSpecifyRelease(t *testing.T) {
	p := NewFreeBSDJailProvisioner("", "", "14.2-RELEASE", "", "")
	if p.release != "14.2-RELEASE" {
		t.Fatalf("release = %q, want %q", p.release, "14.2-RELEASE")
	}
}

// TestBaseArchiveURLIncorporatesRelease verifies requirement 3.
func TestBaseArchiveURLIncorporatesRelease(t *testing.T) {
	p := NewFreeBSDJailProvisioner("", "/usr/local/jails", "15.1-RELEASE", "vtnet0", "192.168.122.100")

	url := p.baseArchiveURL("15.1-RELEASE")
	if !strings.Contains(url, "/15.1-RELEASE/base.txz") {
		t.Errorf("baseArchiveURL(15.1-RELEASE) = %q, want it to contain /15.1-RELEASE/base.txz", url)
	}
	if !strings.HasPrefix(url, "https://download.freebsd.org/releases/") {
		t.Errorf("baseArchiveURL(15.1-RELEASE) = %q, want FreeBSD download prefix", url)
	}

	other := p.baseArchiveURL("14.2-RELEASE")
	if !strings.Contains(other, "/14.2-RELEASE/base.txz") {
		t.Errorf("baseArchiveURL(14.2-RELEASE) = %q, want it to contain /14.2-RELEASE/base.txz", other)
	}
	if other == url {
		t.Error("base archive URLs for different releases must differ")
	}
}

// TestBaseArchiveCachedByRelease verifies requirement 4.
func TestBaseArchiveCachedByRelease(t *testing.T) {
	p, _, downloader, _, _ := newTestProvisioner(t)
	ctx := context.Background()

	first, err := p.ensureBaseArchive(ctx, "15.1-RELEASE")
	if err != nil {
		t.Fatalf("ensureBaseArchive(15.1-RELEASE): %v", err)
	}
	second, err := p.ensureBaseArchive(ctx, "15.1-RELEASE")
	if err != nil {
		t.Fatalf("ensureBaseArchive(15.1-RELEASE) second call: %v", err)
	}
	if first != second {
		t.Errorf("same release returned different paths: %q vs %q", first, second)
	}
	if downloader.count() != 1 {
		t.Errorf("downloader called %d times for one release, want 1", downloader.count())
	}

	other, err := p.ensureBaseArchive(ctx, "14.2-RELEASE")
	if err != nil {
		t.Fatalf("ensureBaseArchive(14.2-RELEASE): %v", err)
	}
	if other == first {
		t.Errorf("different releases shared a cache path: %q", other)
	}
	if downloader.count() != 2 {
		t.Errorf("downloader called %d times for two releases, want 2", downloader.count())
	}
	if !strings.Contains(other, "14.2-RELEASE") {
		t.Errorf("cache path %q does not identify its release", other)
	}
}

// TestJailRootPopulatedBeforeStartup verifies requirement 5.
func TestJailRootPopulatedBeforeStartup(t *testing.T) {
	p, runner, _, _, rec := newTestProvisioner(t)

	handle, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID: "populate",
		Image:       "custom-image",
		WorkDir:     "/workspace/repo",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	jailRoot := p.getJailRootPath(handle.NativeID)
	if _, err := os.Stat(filepath.Join(jailRoot, "bin", "sh")); err != nil {
		t.Fatalf("jail root was not populated before startup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(jailRoot, "dev")); err != nil {
		t.Fatalf("jail root /dev was not created: %v", err)
	}
	info, err := os.Stat(filepath.Join(jailRoot, "tmp"))
	if err != nil {
		t.Fatalf("jail root /tmp was not created: %v", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0777 || info.Mode()&os.ModeSticky == 0 {
		t.Fatalf("jail /tmp mode = %v, want a sticky world-writable directory", info.Mode())
	}

	extractIdx := rec.indexOf("extract")
	startIdx := rec.indexOfPrefix("run:jail -c")
	if extractIdx == -1 {
		t.Fatal("base archive was never extracted")
	}
	if startIdx == -1 {
		t.Fatal("jail was never started")
	}
	if extractIdx > startIdx {
		t.Errorf("base archive extracted (event %d) after jail start (event %d)", extractIdx, startIdx)
	}
	if !runner.calledWith("jail -c") {
		t.Error("jail -c was not invoked")
	}
}

// TestBsdinstallNeverInvoked verifies requirement 6.
func TestBsdinstallNeverInvoked(t *testing.T) {
	p, runner, _, _, _ := newTestProvisioner(t)
	if _, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID: "nobsd",
		Image:       "custom-image",
		WorkDir:     "/workspace/repo",
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if runner.calledWith("bsdinstall") {
		t.Error("bsdinstall must never be invoked")
	}
}

// TestProvisionConfigContents verifies requirements 7, 8, 12, 13 and that the
// hard-coded loopback address is gone.
func TestProvisionConfigContents(t *testing.T) {
	p, _, _, _, _ := newTestProvisioner(t)

	workspaceBase := t.TempDir()
	handle, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID:       "config",
		Image:             "custom-image",
		WorkDir:           "/workspace/repo",
		CacheMountPath:    "/host/cache",
		WorkspaceBasePath: workspaceBase,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	data, err := os.ReadFile(p.getJailConfigPath(handle.NativeID))
	if err != nil {
		t.Fatalf("read generated jail config: %v", err)
	}
	config := string(data)

	// The workspace is mounted from a per-workspace host directory, not the
	// shared base path. jail.conf mount entries are host fstab(5) lines, so
	// targets are the full host paths beneath the jail root.
	jailRoot := p.getJailRootPath(handle.NativeID)
	workspaceMount := filepath.Join(workspaceBase, "config") + " " + filepath.Join(jailRoot, "workspace", "repo") + " nullfs rw 0 0"
	want := []string{
		`interface = "vtnet0";`,
		`ip4.addr += "192.168.122.100";`,
		"mount.devfs;",
		"/tmp " + filepath.Join(jailRoot, "tmp") + " nullfs rw 0 0",
		"/host/cache " + filepath.Join(jailRoot, "cache") + " nullfs rw 0 0",
		workspaceMount,
		`path = "`,
		"exec.clean;",
		"allow.raw_sockets = 1;",
		"allow.sysvipc;",
	}
	for _, w := range want {
		if !strings.Contains(config, w) {
			t.Errorf("generated config missing %q:\n%s", w, config)
		}
	}

	forbidden := []string{"127.0.1.", "bsdinstall", "linprocfs", "linsysfs", "allow.sysvshm"}
	for _, f := range forbidden {
		if strings.Contains(config, f) {
			t.Errorf("generated config unexpectedly contains %q:\n%s", f, config)
		}
	}
}

// TestMissingInterfaceRejected verifies requirement 9.
func TestMissingInterfaceRejected(t *testing.T) {
	p := newUnconfiguredProvisioner(t, "", "192.168.122.100")
	_, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID: "noiface",
		Image:       "custom-image",
		WorkDir:     "/workspace/repo",
	})
	if err == nil {
		t.Fatal("Provision succeeded without an interface, want error")
	}
	if !strings.Contains(err.Error(), "interface") {
		t.Errorf("error %q does not mention the interface", err)
	}
}

// TestEmptyIPUsesDHCP verifies that omitting the IPv4 address makes the jail
// inherit the host's DHCP-assigned address instead of pinning a fixed one.
func TestEmptyIPUsesDHCP(t *testing.T) {
	p := newUnconfiguredProvisioner(t, "vtnet0", "")
	handle, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID: "dhcp",
		Image:       "custom-image",
		WorkDir:     "/workspace/repo",
	})
	if err != nil {
		t.Fatalf("Provision with no fixed IP should use DHCP, got error: %v", err)
	}

	data, err := os.ReadFile(p.getJailConfigPath(handle.NativeID))
	if err != nil {
		t.Fatalf("read generated jail config: %v", err)
	}
	config := string(data)
	if !strings.Contains(config, "ip4 = inherit;") {
		t.Errorf("generated config does not inherit host addresses for DHCP:\n%s", config)
	}
	if strings.Contains(config, "ip4.addr") {
		t.Errorf("generated config pinned a fixed address despite DHCP:\n%s", config)
	}
}

func TestVNETConfigUsesJibAndPrivateInterface(t *testing.T) {
	p, _, _, _, _ := newTestProvisioner(t)
	p.WithNetworkMode("vnet").WithVNETGateway("192.168.122.1")
	p.WithDNSServers([]string{"192.168.122.1"})
	p.ip4Address = "192.168.122.100/24"

	handle, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID: "ws-ca4c1bebd",
		WorkDir:     "/workspace/repo",
	})
	if err != nil {
		t.Fatalf("Provision VNET: %v", err)
	}

	data, err := os.ReadFile(p.getJailConfigPath(handle.NativeID))
	if err != nil {
		t.Fatalf("read generated jail config: %v", err)
	}
	config := string(data)
	for _, want := range []string{
		"vnet;",
		`vnet.interface = "e0b_wsca4c1bebd";`,
		"/usr/local/sbin/jib addm wsca4c1bebd vtnet0",
		"/sbin/ifconfig e0b_wsca4c1bebd 192.168.122.100/24 up",
		"/sbin/route add default 192.168.122.1",
		"/usr/local/sbin/jib destroy wsca4c1bebd",
	} {
		if !strings.Contains(config, want) {
			t.Errorf("VNET config missing %q:\n%s", want, config)
		}
	}
	if len("e0b_"+jibNameForJail(handle.NativeID)) > 15 {
		t.Errorf("VNET interface name %q exceeds FreeBSD's 15-character limit", "e0b_"+jibNameForJail(handle.NativeID))
	}
	if handle.NativeID != "wsd-ws-ca4c1bebd" {
		t.Errorf("NativeID = %q, want full jail name %q", handle.NativeID, "wsd-ws-ca4c1bebd")
	}
	if strings.Contains(config, "ip4 = inherit;") || strings.Contains(config, "ip4.addr") {
		t.Errorf("VNET config unexpectedly contains shared-stack networking:\n%s", config)
	}
}

func TestVNETRequiresCIDRAndGateway(t *testing.T) {
	// VNET without CIDR/gateway should work (DHCP mode)
	p := newUnconfiguredProvisioner(t, "vtnet0", "")
	p.WithNetworkMode("vnet").WithVNETGateway("")
	// This should succeed - DHCP mode doesn't require CIDR or gateway
	handle, err := p.Provision(context.Background(), runtime.ProvisionSpec{WorkspaceID: "dhcp-mode"})
	if err != nil {
		t.Fatalf("VNET provisioning without CIDR/gateway (DHCP mode) failed: %v", err)
	}

	// Verify DHCP configuration includes dhclient and bpf
	data, err := os.ReadFile(p.getJailConfigPath(handle.NativeID))
	if err != nil {
		t.Fatalf("read generated jail config: %v", err)
	}
	config := string(data)
	if !strings.Contains(config, "/sbin/dhclient") {
		t.Errorf("VNET DHCP config missing dhclient:\n%s", config)
	}

	// VNET with CIDR but without gateway should fail
	p2 := newUnconfiguredProvisioner(t, "vtnet0", "192.168.122.100/24")
	p2.WithNetworkMode("vnet").WithVNETGateway("")
	if _, err := p2.Provision(context.Background(), runtime.ProvisionSpec{WorkspaceID: "missing-gateway"}); err == nil {
		t.Fatal("VNET provisioning with CIDR but without gateway succeeded")
	}

	// VNET with gateway but without CIDR should fail
	p3 := newUnconfiguredProvisioner(t, "vtnet0", "")
	p3.WithNetworkMode("vnet").WithVNETGateway("192.168.122.1")
	if _, err := p3.Provision(context.Background(), runtime.ProvisionSpec{WorkspaceID: "missing-cidr"}); err == nil {
		t.Fatal("VNET provisioning with gateway but without CIDR succeeded")
	}
}

// TestInvalidIPRejected verifies requirement 11.
func TestInvalidIPRejected(t *testing.T) {
	for _, bad := range []string{"999.1.1.1", "not-an-ip", "192.168.122.256", "2001:db8::1"} {
		p := newUnconfiguredProvisioner(t, "vtnet0", bad)
		_, err := p.Provision(context.Background(), runtime.ProvisionSpec{
			WorkspaceID: "badip",
			Image:       "custom-image",
			WorkDir:     "/workspace/repo",
		})
		if err == nil {
			t.Errorf("Provision accepted invalid IP %q, want error", bad)
		}
	}
}

// TestStartupFailureCleansUp verifies requirement 14.
func TestStartupFailureCleansUp(t *testing.T) {
	p, runner, _, _, _ := newTestProvisioner(t)
	runner.failOn = "jail -c"

	handle, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID: "startfail",
		Image:       "custom-image",
		WorkDir:     "/workspace/repo",
	})
	if err == nil {
		t.Fatal("Provision succeeded even though jail startup failed")
	}
	if handle != nil {
		t.Error("Provision returned a handle on startup failure")
	}

	jailName := "wsd-startfail"
	if pathExists(p.getJailConfigPath(jailName)) {
		t.Error("jail config was not removed after startup failure")
	}
	if !runner.calledWith("jail -r") {
		t.Error("no attempt was made to stop a partially-started jail")
	}
	// The root did not exist before this run, so it should be cleaned up.
	if pathExists(p.getJailRootPath(jailName)) {
		t.Error("partially-created jail root was not cleaned up")
	}
}

// TestExtractionFailureCleansUpRoot verifies that a failed extraction does not
// leave a partial workspace root behind.
func TestExtractionFailureCleansUpRoot(t *testing.T) {
	p, _, _, extractor, _ := newTestProvisioner(t)
	extractor.fail = true

	_, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID: "extractfail",
		Image:       "custom-image",
		WorkDir:     "/workspace/repo",
	})
	if err == nil {
		t.Fatal("Provision succeeded even though extraction failed")
	}
	if pathExists(p.getJailRootPath("wsd-extractfail")) {
		t.Error("partially-created jail root was not cleaned up after extraction failure")
	}
}

// TestStaleJailRootIsRebuilt verifies that a jail root left behind by a
// previous failed start (non-empty, no bin/sh) is cleared and repopulated
// instead of permanently blocking provisioning.
func TestStaleJailRootIsRebuilt(t *testing.T) {
	p, _, _, _, _ := newTestProvisioner(t)

	jailRoot := p.getJailRootPath("wsd-stale")
	if err := os.MkdirAll(jailRoot, 0755); err != nil {
		t.Fatalf("seed stale jail root: %v", err)
	}
	stale := filepath.Join(jailRoot, "leftover-mount")
	if err := os.WriteFile(stale, []byte("stale"), 0644); err != nil {
		t.Fatalf("seed stale file: %v", err)
	}

	if _, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID: "stale",
		Image:       "custom-image",
		WorkDir:     "/workspace/repo",
	}); err != nil {
		t.Fatalf("Provision with a stale jail root: %v", err)
	}

	if pathExists(stale) {
		t.Error("stale jail root contents were not cleared")
	}
	if _, err := os.Stat(filepath.Join(jailRoot, "bin", "sh")); err != nil {
		t.Errorf("jail root was not repopulated after clearing: %v", err)
	}
}

// TestDestroyByIDRemovesRootAndConfig verifies that destroying a jail also
// removes its per-jail root, so ephemeral workspaces don't leak directories.
func TestDestroyByIDRemovesRootAndConfig(t *testing.T) {
	p, _, _, _, _ := newTestProvisioner(t)
	jailName := "wsd-destroy"
	jailRoot := p.getJailRootPath(jailName)

	if err := os.MkdirAll(jailRoot, 0755); err != nil {
		t.Fatalf("seed jail root: %v", err)
	}
	if err := writeFile(p.getJailConfigPath(jailName), "x", 0644); err != nil {
		t.Fatalf("seed jail config: %v", err)
	}

	if err := p.DestroyByID(context.Background(), jailName); err != nil {
		t.Fatalf("DestroyByID: %v", err)
	}
	if pathExists(jailRoot) {
		t.Error("jail root was not removed")
	}
	if pathExists(p.getJailConfigPath(jailName)) {
		t.Error("jail config was not removed")
	}
}

// TestDestroyByIDToleratesMissingJail verifies that a jail which no longer
// exists (e.g. after a host reboot) is treated as successfully destroyed, so
// its leftover state-file entry can be cleared instead of wedging cleanup
// forever with `jail: "..." not found`.
func TestDestroyByIDToleratesMissingJail(t *testing.T) {
	p, runner, _, _, _ := newTestProvisioner(t)
	jailName := "wsd-gone"
	jailRoot := p.getJailRootPath(jailName)

	if err := os.MkdirAll(jailRoot, 0755); err != nil {
		t.Fatalf("seed jail root: %v", err)
	}
	if err := writeFile(p.getJailConfigPath(jailName), "x", 0644); err != nil {
		t.Fatalf("seed jail config: %v", err)
	}

	runner.failOn = "jail -r " + jailName
	runner.failErr = fmtError(`exit status 1: jail: "` + jailName + `" not found`)

	if err := p.DestroyByID(context.Background(), jailName); err != nil {
		t.Fatalf("DestroyByID with an already-gone jail: %v", err)
	}
	if pathExists(jailRoot) {
		t.Error("jail root was not removed")
	}
	if pathExists(p.getJailConfigPath(jailName)) {
		t.Error("jail config was not removed")
	}
}

// TestDownloadFailureKeepsCacheClean verifies that a failed download does not
// leave a partial archive and that a shared cached archive is not removed.
func TestDownloadFailureKeepsCacheClean(t *testing.T) {
	p, _, downloader, _, _ := newTestProvisioner(t)

	// Seed a cached archive for one release; it must survive a failed
	// download of a different release.
	cached, err := p.ensureBaseArchive(context.Background(), "15.1-RELEASE")
	if err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	downloader.fail = true
	if _, err := p.ensureBaseArchive(context.Background(), "14.2-RELEASE"); err == nil {
		t.Fatal("ensureBaseArchive succeeded despite download failure")
	}
	if !pathExists(cached) {
		t.Error("shared cached base archive was removed after an unrelated failure")
	}
	if pathExists(p.baseArchivePath("14.2-RELEASE")) {
		t.Error("partial download was left behind in the cache")
	}
}

// TestProvisionPassesEscalationToTransport verifies that the escalation
// command configured on the provisioner is handed to the exec transport, so
// jexec/jls/jail -m also run through it when the daemon is unprivileged.
func TestProvisionPassesEscalationToTransport(t *testing.T) {
	p, _, _, _, _ := newTestProvisioner(t)
	p.WithEscalationCommand("mdo")

	handle, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID: "esctransport",
		Image:       "custom-image",
		WorkDir:     "/workspace/repo",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	tr, ok := handle.Transport.(*FreeBSDJailExecTransport)
	if !ok {
		t.Fatalf("Transport type = %T, want *FreeBSDJailExecTransport", handle.Transport)
	}
	if tr.escalationCommand != "mdo" {
		t.Errorf("transport escalationCommand = %q, want %q", tr.escalationCommand, "mdo")
	}
}

// TestProvisionPrivilegedCommandsUseEscalation verifies that the root-only
// lifecycle commands (jail create/destroy, force-unmount) are prefixed with
// the configured escalation command.
func TestProvisionPrivilegedCommandsUseEscalation(t *testing.T) {
	p, runner, _, _, _ := newTestProvisioner(t)
	p.WithEscalationCommand("mdo")

	handle, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID:       "escjail",
		Image:             "custom-image",
		WorkDir:           "/workspace/repo",
		WorkspaceBasePath: t.TempDir(),
		CacheMountPath:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !runner.calledWith("mdo jail -c") {
		t.Errorf("jail create was not escalated: %v", runner.calls)
	}
	jailRoot := p.getJailRootPath(handle.NativeID)
	for _, command := range []string{
		"mdo tar -xpf " + p.baseArchivePath(p.release) + " -C " + jailRoot,
		"mdo chroot " + jailRoot + " /bin/sh -c",
		"mdo chroot " + jailRoot + " /bin/mkdir -p -- dev",
		"mdo chroot " + jailRoot + " /bin/mkdir -p -- tmp",
		"mdo chroot " + jailRoot + " /bin/mkdir -p -- workspace/repo",
		"mdo chroot " + jailRoot + " /bin/mkdir -p -- cache",
		"mdo chroot " + jailRoot + " /bin/chmod 1777 /tmp",
	} {
		if !runner.calledWith(command) {
			t.Errorf("missing privileged setup command %q: %v", command, runner.calls)
		}
	}

	if err := p.Destroy(context.Background(), handle); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if !runner.calledWith("mdo jail -r") {
		t.Errorf("jail destroy was not escalated: %v", runner.calls)
	}
}

func TestExtractArchiveUsesPrivilegedRunner(t *testing.T) {
	for _, escalator := range []string{"", "mdo"} {
		t.Run("escalator="+escalator, func(t *testing.T) {
			p := NewFreeBSDJailProvisioner("", "", "", "", "").WithEscalationCommand(escalator)
			want := []string{"tar", "-xpf", "/cache/base archive.txz", "-C", "/jails/jail root"}
			if escalator != "" {
				want = append([]string{escalator}, want...)
			}
			sentinel := errors.New("tar: cannot restore ownership")
			p.run = func(ctx context.Context, name string, args ...string) error {
				if got := append([]string{name}, args...); !slices.Equal(got, want) {
					t.Fatalf("command = %q, want %q", got, want)
				}
				return sentinel
			}
			if err := p.extractArchive(context.Background(), "/cache/base archive.txz", "/jails/jail root"); !errors.Is(err, sentinel) {
				t.Fatalf("extraction error = %v, want %v", err, sentinel)
			}
		})
	}
}

func TestPrivilegedSetupFailurePreventsJailStartup(t *testing.T) {
	for _, command := range []string{"mdo tar", "mdo chroot"} {
		t.Run(command, func(t *testing.T) {
			p, runner, _, _, _ := newTestProvisioner(t)
			p.WithEscalationCommand("mdo")
			p.WithDNSServers([]string{"192.0.2.53"})
			sentinel := errors.New("privileged setup denied")
			runner.failOn, runner.failErr = command, sentinel
			handle, err := p.Provision(context.Background(), runtime.ProvisionSpec{WorkspaceID: "setupfail"})
			if handle != nil || !errors.Is(err, sentinel) {
				t.Fatalf("Provision = %v, %v; want nil handle and setup error", handle, err)
			}
			if runner.calledWith("mdo jail -c") {
				t.Fatal("jail started after privileged setup failed")
			}
		})
	}
}

func TestExtractArchivePreservesMetadata(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("restoring archive ownership requires root")
	}
	dir := t.TempDir()
	archive := filepath.Join(dir, "base.tar")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	want := tar.Header{Name: "owned-file", Mode: 04755, Uid: 1234, Gid: 2345}
	if err := writer.WriteHeader(&want); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "root")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	p := NewFreeBSDJailProvisioner("", "", "", "", "")
	if err := p.extractArchive(context.Background(), archive, root); err != nil {
		t.Fatal(err)
	}
	// Re-archive to inspect numeric ownership without platform-specific stat types.
	snapshot := filepath.Join(dir, "snapshot.tar")
	if err := defaultCommandRunner(context.Background(), "tar", "-cf", snapshot, "-C", root, want.Name); err != nil {
		t.Fatal(err)
	}
	result, err := os.Open(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	got, err := tar.NewReader(result).Next()
	if err != nil {
		t.Fatal(err)
	}
	if got.Uid != want.Uid || got.Gid != want.Gid || got.Mode != want.Mode {
		t.Fatalf("metadata = uid %d gid %d mode %o; want uid %d gid %d mode %o",
			got.Uid, got.Gid, got.Mode, want.Uid, want.Gid, want.Mode)
	}
}

// TestSuccessfulProvisionReturnsHandle verifies requirement 15.
func TestSuccessfulProvisionReturnsHandle(t *testing.T) {
	p, _, _, _, _ := newTestProvisioner(t)

	handle, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID: "success",
		Image:       "custom-image",
		WorkDir:     "/workspace/repo",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if handle == nil {
		t.Fatal("Provision returned nil handle")
	}
	if handle.OS != runtime.OSFreeBSD {
		t.Errorf("OS = %v, want %v", handle.OS, runtime.OSFreeBSD)
	}
	if handle.Isolation != runtime.IsolationJail {
		t.Errorf("Isolation = %v, want %v", handle.Isolation, runtime.IsolationJail)
	}
	if handle.Username != "root" {
		t.Errorf("Username = %q, want %q", handle.Username, "root")
	}
	if handle.Privilege != runtime.PrivilegeRoot {
		t.Errorf("Privilege = %v, want %v", handle.Privilege, runtime.PrivilegeRoot)
	}
	if handle.WorkDir != "/workspace/repo" {
		t.Errorf("WorkDir = %q, want %q", handle.WorkDir, "/workspace/repo")
	}
	if handle.NativeID != "wsd-success" {
		t.Errorf("NativeID = %q, want %q", handle.NativeID, "wsd-success")
	}
	if handle.Transport == nil {
		t.Error("Transport is nil")
	}
}

// TestSpecOverridesProvisionerNetworking verifies that per-workspace values
// take precedence over the provisioner defaults.
func TestSpecOverridesProvisionerNetworking(t *testing.T) {
	p, _, _, _, _ := newTestProvisioner(t)

	handle, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID: "override",
		Image:       "custom-image",
		WorkDir:     "/workspace/repo",
		Interface:   "em0",
		IP4Address:  "10.0.0.5",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	data, err := os.ReadFile(p.getJailConfigPath(handle.NativeID))
	if err != nil {
		t.Fatalf("read generated jail config: %v", err)
	}
	if !strings.Contains(string(data), `interface = "em0";`) {
		t.Errorf("spec interface override not used:\n%s", data)
	}
	if !strings.Contains(string(data), `ip4.addr += "10.0.0.5";`) {
		t.Errorf("spec IP override not used:\n%s", data)
	}
}

func TestFreeBSDJailProvisioner_Name(t *testing.T) {
	p := NewFreeBSDJailProvisioner("", "", "", "", "")
	if name := p.Name(); name != "freebsd-jail" {
		t.Errorf("Name() = %q, want %q", name, "freebsd-jail")
	}
}

func TestFreeBSDJailProvisioner_ImplementsInterface(t *testing.T) {
	p := NewFreeBSDJailProvisioner("", "", "", "", "")
	var _ runtime.Provisioner = p
}

func TestGetJailRootPath(t *testing.T) {
	p := NewFreeBSDJailProvisioner("/etc/jail.conf.d", "/usr/local/jails", "15.1-RELEASE", "", "")

	tests := []struct {
		jailName string
		expected string
	}{
		{"wsd-abc123", "/usr/local/jails/containers/wsd-abc123"},
		{"wsd-00f7995e4", "/usr/local/jails/containers/wsd-00f7995e4"},
		{"my-jail", "/usr/local/jails/containers/my-jail"},
	}

	for _, tt := range tests {
		t.Run(tt.jailName, func(t *testing.T) {
			result := p.getJailRootPath(tt.jailName)
			if result != tt.expected {
				t.Errorf("getJailRootPath(%q) = %q, want %q", tt.jailName, result, tt.expected)
			}
		})
	}
}

func TestGetJailConfigPath(t *testing.T) {
	p := NewFreeBSDJailProvisioner("/etc/jail.conf.d", "/usr/local/jails", "14.0-RELEASE", "", "")

	tests := []struct {
		jailName string
		expected string
	}{
		{"test-ws-123", "/etc/jail.conf.d/test-ws-123.conf"},
		{"my-jail", "/etc/jail.conf.d/my-jail.conf"},
	}

	for _, tt := range tests {
		t.Run(tt.jailName, func(t *testing.T) {
			result := p.getJailConfigPath(tt.jailName)
			if result != tt.expected {
				t.Errorf("getJailConfigPath(%q) = %q, want %q", tt.jailName, result, tt.expected)
			}
		})
	}
}

func TestValidateRelease(t *testing.T) {
	valid := []string{"15.1-RELEASE", "14.2-RELEASE", "15.0-STABLE", "15.1"}
	for _, r := range valid {
		if err := validateRelease(r); err != nil {
			t.Errorf("validateRelease(%q) = %v, want nil", r, err)
		}
	}
	invalid := []string{"", "../etc", "15.1/RELEASE", "release name"}
	for _, r := range invalid {
		if err := validateRelease(r); err == nil {
			t.Errorf("validateRelease(%q) = nil, want error", r)
		}
	}
}

func TestValidateInterface(t *testing.T) {
	if err := validateInterface("vtnet0"); err != nil {
		t.Errorf("validateInterface(vtnet0) = %v, want nil", err)
	}
	for _, bad := range []string{"", "vtnet 0", "../eth0", "eth/0"} {
		if err := validateInterface(bad); err == nil {
			t.Errorf("validateInterface(%q) = nil, want error", bad)
		}
	}
}

func TestValidateIP4Address(t *testing.T) {
	if err := validateIP4Address("192.168.122.100"); err != nil {
		t.Errorf("validateIP4Address(valid) = %v, want nil", err)
	}
	for _, bad := range []string{"", "999.1.1.1", "not-an-ip", "2001:db8::1"} {
		if err := validateIP4Address(bad); err == nil {
			t.Errorf("validateIP4Address(%q) = nil, want error", bad)
		}
	}
}

func TestIsJailNotFoundError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"nil error", nil, false},
		{"jail not found", fmtError("jail: jail not found: test-jail"), true},
		{"quoted jail not found", fmtError(`exit status 1: jail: "test-jail" not found`), true},
		{"no such jail", fmtError("no such jail: test-jail"), true},
		{"not running", fmtError("jail is not running: test-jail"), true},
		{"other error", fmtError("some other error"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isJailNotFoundError(tt.err)
			if result != tt.expected {
				t.Errorf("isJailNotFoundError() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestIsBenignJailError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"nil error", nil, true},
		{"jail not found", fmtError("jail: jail not found: test-jail"), true},
		{"quoted jail not found", fmtError(`exit status 1: jail: "test-jail" not found`), true},
		{"no such jail", fmtError("no such jail: test-jail"), true},
		{"not running", fmtError("jail is not running: test-jail"), true},
		{"already stopped", fmtError("jail already stopped: test-jail"), true},
		{"jail binary missing", fmtError(`exec: "jail": executable file not found in $PATH`), false},
		{"other error", fmtError("some other error"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isBenignJailError(tt.err)
			if result != tt.expected {
				t.Errorf("isBenignJailError() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// TestFreeBSDJailProvisioner_HandleProperties is a lightweight check that the
// provisioner satisfies the interface.
func TestFreeBSDJailProvisioner_HandleProperties(t *testing.T) {
	p := NewFreeBSDJailProvisioner("", "", "", "", "")

	if p.Name() != "freebsd-jail" {
		t.Errorf("Expected name 'freebsd-jail', got '%s'", p.Name())
	}

	var _ runtime.Provisioner = p
}

// TestRuntimeHandleProperties tests that the runtime handle has correct properties.
func TestRuntimeHandleProperties(t *testing.T) {
	handle := &runtime.RuntimeHandle{
		OS:        runtime.OSFreeBSD,
		Isolation: runtime.IsolationJail,
		Username:  "root",
		Privilege: runtime.PrivilegeRoot,
		WorkDir:   "/workspace",
		NativeID:  "test-jail",
		Transport: NewFreeBSDJailExecTransport("test-jail"),
	}

	if handle.OS != runtime.OSFreeBSD {
		t.Errorf("OS = %v, want %v", handle.OS, runtime.OSFreeBSD)
	}
	if handle.Isolation != runtime.IsolationJail {
		t.Errorf("Isolation = %v, want %v", handle.Isolation, runtime.IsolationJail)
	}
	if handle.Username != "root" {
		t.Errorf("Username = %q, want %q", handle.Username, "root")
	}
	if handle.Privilege != runtime.PrivilegeRoot {
		t.Errorf("Privilege = %v, want %v", handle.Privilege, runtime.PrivilegeRoot)
	}
	if handle.WorkDir != "/workspace" {
		t.Errorf("WorkDir = %q, want %q", handle.WorkDir, "/workspace")
	}
	if handle.NativeID != "test-jail" {
		t.Errorf("NativeID = %q, want %q", handle.NativeID, "test-jail")
	}
	if handle.Transport == nil {
		t.Error("Transport is nil")
	}
}

// --- test helpers ---

// recorder records ordered provisioning events so tests can assert that the
// jail root is populated before the jail is started.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recorder) indexOf(event string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, e := range r.events {
		if e == event {
			return i
		}
	}
	return -1
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
	mu     sync.Mutex
	calls  [][]string
	failOn string
	// failErr, when set, is returned instead of the generic fake failure for
	// any call matching failOn. It lets tests reproduce the exact stderr text
	// a real command (e.g. jail(8)) produces.
	failErr error
	rec     *recorder
}

func (f *fakeRunner) run(ctx context.Context, name string, args ...string) error {
	full := append([]string{name}, args...)
	joined := strings.Join(full, " ")
	f.mu.Lock()
	f.calls = append(f.calls, full)
	f.mu.Unlock()
	if f.rec != nil {
		f.rec.add("run:" + joined)
	}
	if f.failOn != "" && strings.Contains(joined, f.failOn) {
		if f.failErr != nil {
			return f.failErr
		}
		return fmtError("fake runner failure for " + f.failOn)
	}
	return nil
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

type fakeDownloader struct {
	mu    sync.Mutex
	calls int
	fail  bool
}

func (d *fakeDownloader) download(ctx context.Context, url, dest string) error {
	d.mu.Lock()
	d.calls++
	fail := d.fail
	d.mu.Unlock()
	if fail {
		return fmtError("fake download failure")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	return os.WriteFile(dest, []byte("base-txz-contents"), 0644)
}

func (d *fakeDownloader) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

type fakeExtractor struct {
	fail bool
	rec  *recorder
}

func (e *fakeExtractor) extract(ctx context.Context, archive, dest string) error {
	if e.rec != nil {
		e.rec.add("extract")
	}
	if e.fail {
		return fmtError("fake extract failure")
	}
	if err := os.MkdirAll(filepath.Join(dest, "bin"), 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dest, "bin", "sh"), []byte("#!/bin/sh\n"), 0755)
}

// newTestProvisioner returns a provisioner whose side-effecting operations are
// fakes, so the full provisioning flow can be exercised without a FreeBSD host.
func newTestProvisioner(t *testing.T) (*FreeBSDJailProvisioner, *fakeRunner, *fakeDownloader, *fakeExtractor, *recorder) {
	t.Helper()
	dir := t.TempDir()
	rec := &recorder{}
	p := NewFreeBSDJailProvisioner(
		filepath.Join(dir, "jail.conf.d"),
		filepath.Join(dir, "jails"),
		"",
		"vtnet0",
		"192.168.122.100",
	)
	runner := &fakeRunner{rec: rec}
	downloader := &fakeDownloader{}
	extractor := &fakeExtractor{rec: rec}
	p.run = func(ctx context.Context, name string, args ...string) error {
		if err := runner.run(ctx, name, args...); err != nil {
			return err
		}
		if name == "mdo" && len(args) > 0 {
			name, args = args[0], args[1:]
		}
		if name == "tar" {
			return extractor.extract(ctx, args[1], args[3])
		}
		return nil
	}
	p.download = downloader.download
	return p, runner, downloader, extractor, rec
}

// newUnconfiguredProvisioner returns a provisioner with no interface/IP so
// validation tests can supply (or omit) them per spec.
func newUnconfiguredProvisioner(t *testing.T, networkInterface, ip4Address string) *FreeBSDJailProvisioner {
	t.Helper()
	dir := t.TempDir()
	p := NewFreeBSDJailProvisioner(
		filepath.Join(dir, "jail.conf.d"),
		filepath.Join(dir, "jails"),
		"",
		networkInterface,
		ip4Address,
	)
	p.run = (&fakeRunner{}).run
	p.download = (&fakeDownloader{}).download
	return p
}

// TestEnsurePkgDBOwnership verifies that the pkg database directory is created
// and chowned to root through the escalation command. pkg(8) refuses to run
// when /var/db/pkg is owned by the daemon's unprivileged user.
func TestEnsurePkgDBOwnership(t *testing.T) {
	p, runner, _, _, _ := newTestProvisioner(t)
	p.WithEscalationCommand("mdo")
	jailRoot := p.getJailRootPath("wsd-pkgdb")

	p.ensurePkgDBOwnership(context.Background(), jailRoot)

	wantDir := filepath.Join(jailRoot, "var", "db", "pkg")
	if !runner.calledWith("mdo mkdir -p " + wantDir) {
		t.Errorf("pkg database dir was not created via escalation: %v", runner.calls)
	}
	if !runner.calledWith("mdo chown 0:0 " + wantDir) {
		t.Errorf("pkg database dir was not chowned via escalation: %v", runner.calls)
	}
}

// TestCopyHostResolvConf verifies that the host resolver configuration is
// copied into the jail root, so name resolution works inside the jail.
func TestCopyHostResolvConf(t *testing.T) {
	host, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		t.Skipf("host has no /etc/resolv.conf: %v", err)
	}

	jailRoot := t.TempDir()
	p := NewFreeBSDJailProvisioner("", "", "", "", "")
	if err := p.copyHostResolvConf(context.Background(), jailRoot); err != nil {
		t.Fatalf("copyHostResolvConf: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(jailRoot, "etc", "resolv.conf"))
	if err != nil {
		t.Fatalf("read copied resolv.conf: %v", err)
	}
	if string(got) != string(host) {
		t.Errorf("copied resolv.conf does not match host")
	}
}

// fmtError is a helper to create an error with a formatted message
func fmtError(msg string) error {
	return &testError{msg: msg}
}

type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}
