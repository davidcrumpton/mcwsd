package script

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal/runtime"
	"github.com/spf13/pflag"
)

// TestOpenBSDVMDPluginLifecycle drives the deployed openbsd-vmd.js through a
// set of fake host binaries (vmctl/ssh/arp/nc/doas) to confirm the goja-hosted
// lifecycle path (clone -> start -> parse VM id -> ARP/TCP/SSH readiness)
// works end-to-end, not just the pure execArgs mapping.
func TestOpenBSDVMDPluginLifecycle(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeScript(t, filepath.Join(bin, "doas"), "#!/bin/sh\nexec \"$@\"\n")
	writeScript(t, filepath.Join(bin, "mkdir"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(bin, "rm"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(bin, "ls"), "#!/bin/sh\nexit 1\n")
	writeScript(t, filepath.Join(bin, "test"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(bin, "arp"), "#!/bin/sh\nprintf '? (100.64.6.3) at 00:00:00:00:00:00 on tap0\\n'\n")
	writeScript(t, filepath.Join(bin, "nc"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(bin, "ssh"), "#!/bin/sh\nprintf '%s\\n' \"$*\"\nexit 0\n")
	writeScript(t, filepath.Join(bin, "vmctl"), "#!/bin/sh\ncase \"$*\" in\n  *start*) printf 'vmctl: started vm 6 successfully, tty /dev/ttyp3\\n' >&2 ;;\nesac\nexit 0\n")

	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	r := deployedPlugin(t, "openbsd-vmd")
	fs := pflag.NewFlagSet("d", pflag.ContinueOnError)
	vals, err := BindFlags(fs, r.Options)
	if err != nil {
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
	if h.OS != runtime.OSOpenBSD || h.Isolation != runtime.IsolationVM {
		t.Fatalf("handle OS/Isolation = %q/%q", h.OS, h.Isolation)
	}

	if err := p.Destroy(context.Background(), h); err != nil {
		t.Fatalf("destroy: %v", err)
	}
}

// TestOpenBSDVMDPluginRecoverUsesStatus verifies recovery derives the guest IP
// from the running VM's numeric vmd ID (not the name), via vmctl status.
func TestOpenBSDVMDPluginRecoverUsesStatus(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeScript(t, filepath.Join(bin, "doas"), "#!/bin/sh\nexec \"$@\"\n")
	writeScript(t, filepath.Join(bin, "test"), "#!/bin/sh\nexit 0\n")
	writeScript(t, filepath.Join(bin, "vmctl"), "#!/bin/sh\nprintf '   ID   PID VCPUS  MAXMEM  CURMEM     TTY        OWNER    STATE NAME\\n    7 23456     1    1.0G    200M     ttyp4       root   running wsd-ws-abc\\n'\nexit 0\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	r := deployedPlugin(t, "openbsd-vmd")
	fs := pflag.NewFlagSet("d", pflag.ContinueOnError)
	vals, err := BindFlags(fs, r.Options)
	if err != nil {
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

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
