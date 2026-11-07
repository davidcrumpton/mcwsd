package zones

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/davidcrumpton/mcwsd/internal/runtime"
)

type recordedCall struct {
	name string
	args []string
}

type fakeRunner struct {
	mu       sync.Mutex
	calls    []recordedCall
	zoneName string
	zonecfg  string
	fail     func(name string, args []string) error
}

func (f *fakeRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, recordedCall{name: name, args: append([]string(nil), args...)})
	f.mu.Unlock()

	// Tests run both with and without escalation. Match the command that the
	// escalator would actually execute.
	serverName, serverArgs := name, args
	if name == "pfexec" && len(args) > 0 {
		serverName, serverArgs = args[0], args[1:]
	}

	if f.fail != nil {
		if err := f.fail(serverName, serverArgs); err != nil {
			return nil, err
		}
	}

	switch {
	case serverName == "zoneadm" && len(serverArgs) >= 2 && serverArgs[0] == "list" && serverArgs[1] == "-pc":
		return []byte(fmt.Sprintf("0:global:running:/::ipkg:shared\n1:%s:running:/x:u:ipkg:shared\n", f.zoneName)), nil
	case serverName == "zonecfg":
		if i := indexOf(serverArgs, "-f"); i >= 0 && i+1 < len(serverArgs) {
			if data, err := os.ReadFile(serverArgs[i+1]); err == nil {
				f.mu.Lock()
				f.zonecfg = string(data)
				f.mu.Unlock()
			}
		}
		return nil, nil
	case serverName == "zfs":
		return []byte("/rpool1/zones\n"), nil
	case serverName == "ipadm":
		return []byte("192.168.1.10/24\n127.0.0.1/8\n"), nil
	case serverName == "dladm" && len(serverArgs) > 0 && serverArgs[0] == "show-vnic":
		return nil, nil
	}
	return nil, nil
}

func (f *fakeRunner) called(name string, wantArgs ...string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.name != name {
			continue
		}
		if len(wantArgs) == 0 {
			return true
		}
		if len(c.args) < len(wantArgs) {
			continue
		}
		if strings.Join(c.args[:len(wantArgs)], "\x00") == strings.Join(wantArgs, "\x00") {
			return true
		}
	}
	return false
}

func indexOf(values []string, want string) int {
	for i, v := range values {
		if v == want {
			return i
		}
	}
	return -1
}

func testOptions(t *testing.T) Options {
	t.Helper()
	return Options{
		ZonePath:     t.TempDir(),
		Brand:        "ipkg",
		NetworkMode:  "shared",
		Interface:    "net0",
		IPAddress:    "192.168.1.50/24",
		DefRouter:    "192.168.1.1",
		DNSServers:   []string{"192.168.1.1"},
		BootTimeout:  time.Second,
		PollInterval: time.Millisecond,
	}
}

func testProvisioner(t *testing.T, opts Options) (*ZonesProvisioner, *fakeRunner) {
	t.Helper()
	if opts.BootTimeout == 0 {
		opts.BootTimeout = time.Second
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = time.Millisecond
	}
	p := NewZonesProvisioner(opts)
	fr := &fakeRunner{zoneName: "wsd-ws-test"}
	p.run = fr.run
	p.lookPath = func(name string) (string, error) { return name, nil }
	p.brandDir = t.TempDir()
	return p, fr
}

func TestNewZonesProvisionerDefaults(t *testing.T) {
	p := NewZonesProvisioner(Options{})
	if p.opts.Brand != "ipkg" {
		t.Errorf("brand = %q, want ipkg", p.opts.Brand)
	}
	if p.opts.NetworkMode != "shared" {
		t.Errorf("network mode = %q, want shared", p.opts.NetworkMode)
	}
	if p.opts.Escalation != "" {
		t.Errorf("escalation = %q, want empty (CLI supplies pfexec)", p.opts.Escalation)
	}
	if p.opts.BootTimeout != defaultBootTimeout {
		t.Errorf("boot timeout = %v, want %v", p.opts.BootTimeout, defaultBootTimeout)
	}
}

func TestZonesProvisionerProvisionsSharedZone(t *testing.T) {
	opts := testOptions(t)
	p, fr := testProvisioner(t, opts)
	workspaceBase := t.TempDir()
	cacheDir := t.TempDir()

	handle, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID:       "ws-test",
		WorkDir:           "/workspace/repo",
		WorkspaceBasePath: workspaceBase,
		CacheMountPath:    cacheDir,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if handle.OS != runtime.OSIllumos {
		t.Errorf("OS = %v, want %v", handle.OS, runtime.OSIllumos)
	}
	if handle.Isolation != runtime.IsolationZone {
		t.Errorf("Isolation = %v, want %v", handle.Isolation, runtime.IsolationZone)
	}
	if handle.NativeID != "wsd-ws-test" {
		t.Errorf("NativeID = %q, want wsd-ws-test", handle.NativeID)
	}
	if handle.Transport == nil {
		t.Fatal("Transport is nil")
	}

	if !fr.called("zonecfg", "-z", "wsd-ws-test", "-f") {
		t.Error("zonecfg -f was not called")
	}
	if !fr.called("zoneadm", "-z", "wsd-ws-test", "install") {
		t.Error("zoneadm install was not called")
	}
	if !fr.called("zoneadm", "-z", "wsd-ws-test", "boot") {
		t.Error("zoneadm boot was not called")
	}

	for _, want := range []string{
		"create\n",
		"set brand=ipkg",
		"set zonepath=" + filepath.Join(opts.ZonePath, "wsd-ws-test"),
		"set autoboot=false",
		"set ip-type=shared",
		"set physical=net0",
		"set address=192.168.1.50/24",
		"set defrouter=192.168.1.1",
		"set dir=/workspace/repo",
		"set special=" + filepath.Join(workspaceBase, "ws-test"),
		"set type=lofs",
		"set dir=/cache",
		"set special=" + cacheDir,
		"verify",
		"commit",
	} {
		if !strings.Contains(fr.zonecfg, want) {
			t.Errorf("zonecfg script missing %q:\n%s", want, fr.zonecfg)
		}
	}
}

func TestZonesProvisionerClonesGoldenZone(t *testing.T) {
	opts := testOptions(t)
	opts.BaseZone = "wsd-golden"
	p, fr := testProvisioner(t, opts)

	_, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID: "ws-test",
		WorkDir:     "/workspace/repo",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !fr.called("zoneadm", "-z", "wsd-ws-test", "clone", "wsd-golden") {
		t.Error("zoneadm clone was not called")
	}
	for _, want := range []string{"create -t wsd-golden", "remove net", "remove fs"} {
		if !strings.Contains(fr.zonecfg, want) {
			t.Errorf("zonecfg script missing %q:\n%s", want, fr.zonecfg)
		}
	}
}

func TestZonesProvisionerExclusiveStatic(t *testing.T) {
	opts := testOptions(t)
	opts.NetworkMode = "exclusive"
	opts.Interface = "e1000g0"
	opts.IPAddress = "192.168.1.50/24"
	opts.DefRouter = "192.168.1.1"
	opts.DNSServers = []string{"192.168.1.1"}
	p, fr := testProvisioner(t, opts)

	_, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID: "ws-test",
		WorkDir:     "/workspace/repo",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	vnic := vnicNameForZone("wsd-ws-test")
	if !fr.called("dladm", "create-vnic", "-l", "e1000g0", vnic) {
		t.Errorf("dladm create-vnic was not called for %s", vnic)
	}
	for _, want := range []string{
		"set ip-type=exclusive",
		"set physical=" + vnic,
	} {
		if !strings.Contains(fr.zonecfg, want) {
			t.Errorf("zonecfg script missing %q:\n%s", want, fr.zonecfg)
		}
	}
	if strings.Contains(fr.zonecfg, "set address=") {
		t.Errorf("exclusive zonecfg must not set an address:\n%s", fr.zonecfg)
	}
	if !fr.called("zlogin", "wsd-ws-test", "sh", "-c") {
		t.Error("exclusive networking was not configured with zlogin")
	}
}

func TestZonesProvisionerExclusiveDHCP(t *testing.T) {
	opts := testOptions(t)
	opts.NetworkMode = "exclusive"
	opts.Interface = "e1000g0"
	opts.IPAddress = ""
	opts.DefRouter = ""
	opts.DNSServers = nil
	p, fr := testProvisioner(t, opts)

	_, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID: "ws-test",
		WorkDir:     "/workspace/repo",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !fr.called("zlogin", "wsd-ws-test", "sh", "-c") {
		t.Error("DHCP configuration was not run with zlogin")
	}
}

func TestZonesProvisionerRequiresStaticNetworkingOptions(t *testing.T) {
	tests := []struct {
		name string
		opts func(*Options)
		want string
	}{
		{
			name: "exclusive static without router",
			opts: func(o *Options) { o.NetworkMode = "exclusive"; o.DefRouter = "" },
			want: "default router",
		},
		{
			name: "exclusive static without dns",
			opts: func(o *Options) { o.NetworkMode = "exclusive"; o.DNSServers = nil },
			want: "DNS",
		},
		{
			name: "exclusive without interface",
			opts: func(o *Options) { o.NetworkMode = "exclusive"; o.Interface = "" },
			want: "uplink interface",
		},
		{
			name: "shared without interface",
			opts: func(o *Options) { o.Interface = "" },
			want: "data link",
		},
		{
			name: "invalid network mode",
			opts: func(o *Options) { o.NetworkMode = "vnet" },
			want: "network mode",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := testOptions(t)
			tt.opts(&opts)
			p, _ := testProvisioner(t, opts)
			_, err := p.Provision(context.Background(), runtime.ProvisionSpec{
				WorkspaceID: "ws-test",
				WorkDir:     "/workspace/repo",
			})
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.want)) {
				t.Fatalf("Provision error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestZonesProvisionerInstallFailureCleansUp(t *testing.T) {
	opts := testOptions(t)
	p, fr := testProvisioner(t, opts)
	fr.fail = func(name string, args []string) error {
		if name == "zoneadm" && len(args) >= 3 && args[2] == "install" {
			return fmt.Errorf("install failed")
		}
		return nil
	}

	_, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID: "ws-test",
		WorkDir:     "/workspace/repo",
	})
	if err == nil {
		t.Fatal("Provision succeeded, want install failure")
	}
	if !fr.called("zoneadm", "-z", "wsd-ws-test", "uninstall", "-F") {
		t.Error("failed provision did not uninstall the zone")
	}
	if !fr.called("zonecfg", "-z", "wsd-ws-test", "delete", "-F") {
		t.Error("failed provision did not delete the zone configuration")
	}
}

func TestZonesProvisionerUsesEscalation(t *testing.T) {
	opts := testOptions(t)
	opts.Escalation = "pfexec"
	p, fr := testProvisioner(t, opts)

	_, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID: "ws-test",
		WorkDir:     "/workspace/repo",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !fr.called("pfexec", "zoneadm", "-z", "wsd-ws-test", "install") {
		t.Error("privileged command did not go through pfexec")
	}
}

func TestZonesProvisionerDestroyByID(t *testing.T) {
	opts := testOptions(t)
	opts.NetworkMode = "exclusive"
	p, fr := testProvisioner(t, opts)

	if err := p.DestroyByID(context.Background(), "wsd-ws-test"); err != nil {
		t.Fatalf("DestroyByID: %v", err)
	}
	if !fr.called("zoneadm", "-z", "wsd-ws-test", "halt") {
		t.Error("destroy did not halt the zone")
	}
	if !fr.called("zoneadm", "-z", "wsd-ws-test", "uninstall", "-F") {
		t.Error("destroy did not uninstall the zone")
	}
	if !fr.called("zonecfg", "-z", "wsd-ws-test", "delete", "-F") {
		t.Error("destroy did not delete the zone configuration")
	}
	if !fr.called("dladm", "delete-vnic", vnicNameForZone("wsd-ws-test")) {
		t.Error("destroy did not delete the VNIC")
	}
}

func TestZonesProvisionerValidate(t *testing.T) {
	opts := testOptions(t)
	p, fr := testProvisioner(t, opts)
	brand := filepath.Join(p.brandDir, "ipkg")
	if err := os.MkdirAll(brand, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brand, "pkgcreatezone"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(context.Background()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !fr.called("zfs", "list", "-H", "-o", "name", opts.ZonePath) {
		t.Error("Validate did not check that the zonepath is a ZFS dataset")
	}

	fr.fail = func(name string, args []string) error {
		if name == "zfs" {
			return fmt.Errorf("not a dataset")
		}
		return nil
	}
	if err := p.Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "not a ZFS dataset") {
		t.Fatalf("Validate error = %v, want ZFS dataset error", err)
	}
}

func TestZonesProvisionerValidateMissingBrandScript(t *testing.T) {
	opts := testOptions(t)
	p, _ := testProvisioner(t, opts)
	err := p.Validate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "installer") {
		t.Fatalf("Validate error = %v, want missing installer error", err)
	}
}

func TestGlobalZonePrimaryIPv4(t *testing.T) {
	opts := testOptions(t)
	p, _ := testProvisioner(t, opts)
	got, err := p.globalZonePrimaryIPv4(context.Background())
	if err != nil {
		t.Fatalf("globalZonePrimaryIPv4: %v", err)
	}
	if got != "192.168.1.10/24" {
		t.Errorf("got %q, want 192.168.1.10/24", got)
	}
}

func TestBuildZoneConfigQuotesValues(t *testing.T) {
	script := buildZoneConfig(zoneConfig{
		Name:             "wsd-ws-test",
		ZonePath:         "/zones/with space",
		Brand:            "ipkg",
		NetworkMode:      "shared",
		Physical:         "net0",
		Address:          "192.168.1.50/24",
		WorkDir:          "/workspace/repo",
		WorkspaceHostDir: "/host/with space/repo",
	})
	if !strings.Contains(script, `set zonepath="/zones/with space"`) {
		t.Errorf("zonepath was not quoted:\n%s", script)
	}
	if !strings.Contains(script, `set special="/host/with space/repo"`) {
		t.Errorf("special was not quoted:\n%s", script)
	}
}

func TestVnicNameForZone(t *testing.T) {
	if got := vnicNameForZone("wsd-ws-71670485b"); got != "wsdws71670485b0" {
		t.Errorf("vnic name ending in a letter = %q", got)
	}
	if got := vnicNameForZone("wsd-ws-716704856"); got != "wsdws716704856" {
		t.Errorf("vnic name ending in a digit = %q", got)
	}
	long := vnicNameForZone("wsd-" + strings.Repeat("a", 40))
	if len(long) > 31 {
		t.Errorf("vnic name %q exceeds illumos limit", long)
	}
	if long != vnicNameForZone("wsd-"+strings.Repeat("a", 40)) {
		t.Error("vnic name is not stable")
	}
	for _, zone := range []string{
		"wsd-ws-bacfd927d",
		"wsd-ws-e7fee6e06",
		"wsd-ws-000000000",
		"wsd-ws-abc00",
		"wsd-" + strings.Repeat("a", 40),
	} {
		if got := vnicNameForZone(zone); !validDladmLinkName(got) {
			t.Errorf("vnic name %q for %q is not a valid dladm link name", got, zone)
		}
	}
}

func TestIPAM(t *testing.T) {
	alloc, err := newIPAM("192.168.1.200/24", 3)
	if err != nil {
		t.Fatalf("newIPAM: %v", err)
	}
	for i, want := range []string{"192.168.1.200/24", "192.168.1.201/24", "192.168.1.202/24"} {
		got, err := alloc.allocate(fmt.Sprintf("ws-%d", i))
		if err != nil {
			t.Fatalf("allocate %d: %v", i, err)
		}
		if got != want {
			t.Errorf("allocate %d = %q, want %q", i, got, want)
		}
	}
	if _, err := alloc.allocate("ws-extra"); err == nil {
		t.Error("expected pool exhaustion")
	}
	if got := alloc.gateway().String(); got != "192.168.1.1" {
		t.Errorf("gateway = %s, want 192.168.1.1", got)
	}
}
