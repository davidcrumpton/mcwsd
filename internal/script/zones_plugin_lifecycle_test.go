package script

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal/runtime"
	"github.com/spf13/pflag"
)

// TestZonesPluginLifecycle drives the deployed zones.js through a set of fake
// host binaries to confirm the goja-hosted lifecycle path (zonecfg -> install ->
// boot -> readiness poll) works end-to-end, not just the pure execArgs mapping.
func TestZonesPluginLifecycle(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeScript(t, filepath.Join(bin, "pfexec"), "#!/bin/sh\nexec \"$@\"\n")
	writeScript(t, filepath.Join(bin, "zoneadm"), "#!/bin/sh\ncase \"$*\" in\n  *\"list -pc\"*) printf '0:global:running:/::ipkg:shared\\n1:wsd-ws-1:running:/x:u:ipkg:shared\\n' ;;\nesac\nexit 0\n")
	for _, name := range []string{"zonecfg", "zlogin", "zfs", "dladm", "ipadm", "chroot", "umount", "mkdir", "tee", "rm", "test", "cat"} {
		writeScript(t, filepath.Join(bin, name), "#!/bin/sh\nexit 0\n")
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	r := deployedPlugin(t, "zones")
	fs := pflag.NewFlagSet("d", pflag.ContinueOnError)
	vals, err := BindFlags(fs, r.Options)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Parse([]string{"--zones-zonepath", "/rpool1/zones", "--zones-interface", "net0", "--zones-ip-address", "192.168.1.50/24", "--zones-escalation", "pfexec"}); err != nil {
		t.Fatal(err)
	}
	p, err := r.NewProvisioner(vals)
	if err != nil {
		t.Fatal(err)
	}

	h, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID: "ws-1", WorkDir: "/workspace/repo",
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if h.NativeID != "wsd-ws-1" || h.WorkDir != "/workspace/repo" {
		t.Fatalf("handle = %#v", h)
	}
	if h.OS != runtime.OSIllumos || h.Isolation != runtime.IsolationZone {
		t.Fatalf("handle OS/Isolation = %q/%q", h.OS, h.Isolation)
	}

	if err := p.Destroy(context.Background(), h); err != nil {
		t.Fatalf("destroy: %v", err)
	}
}

// TestZonesPluginRecover verifies recovery derives a handle from a running
// zone via zoneadm list -pc.
func TestZonesPluginRecover(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeScript(t, filepath.Join(bin, "pfexec"), "#!/bin/sh\nexec \"$@\"\n")
	writeScript(t, filepath.Join(bin, "zoneadm"), "#!/bin/sh\ncase \"$*\" in\n  *\"list -pc\"*) printf '0:global:running:/::ipkg:shared\\n1:wsd-ws-abc:running:/x:u:ipkg:shared\\n' ;;\nesac\nexit 0\n")
	for _, name := range []string{"zonecfg", "zlogin", "zfs", "dladm", "ipadm", "chroot", "umount", "mkdir", "tee", "rm", "test", "cat"} {
		writeScript(t, filepath.Join(bin, name), "#!/bin/sh\nexit 0\n")
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	r := deployedPlugin(t, "zones")
	fs := pflag.NewFlagSet("d", pflag.ContinueOnError)
	vals, err := BindFlags(fs, r.Options)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Parse([]string{"--zones-zonepath", "/rpool1/zones", "--zones-interface", "net0", "--zones-escalation", "pfexec"}); err != nil {
		t.Fatal(err)
	}
	p, err := r.NewProvisioner(vals)
	if err != nil {
		t.Fatal(err)
	}

	h, err := p.RecoverHandle(context.Background(), "wsd-ws-abc", "/workspace/repo")
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if h.NativeID != "wsd-ws-abc" {
		t.Fatalf("NativeID = %q", h.NativeID)
	}
}
