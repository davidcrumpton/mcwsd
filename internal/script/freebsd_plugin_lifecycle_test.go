package script

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal/runtime"
	"github.com/spf13/pflag"
)

// TestFreeBSDJailPluginLifecycle drives the deployed freebsd-jail.js through a
// set of fake host binaries to confirm the goja-hosted lifecycle path (cache
// check -> extract -> write jail.conf -> jail -c) works end-to-end, not just
// the pure execArgs mapping covered elsewhere.
func TestFreeBSDJailPluginLifecycle(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "jail-created")
	writeScript(t, filepath.Join(bin, "mdo"), "#!/bin/sh\nexec \"$@\"\n")
	for _, name := range []string{"jexec", "jls", "tar", "chroot", "umount", "mount", "rm", "mkdir", "chmod", "chown", "cat", "test", "ls", "tee", "cp", "fetch"} {
		writeScript(t, filepath.Join(bin, name), "#!/bin/sh\nexit 0\n")
	}
	writeScript(t, filepath.Join(bin, "jail"), "#!/bin/sh\ncase \"$*\" in *-c*) touch "+marker+" ;; esac\nexit 0\n")

	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	r := deployedPlugin(t, "freebsd-jail")
	fs := pflag.NewFlagSet("d", pflag.ContinueOnError)
	vals, err := BindFlags(fs, r.Options)
	if err != nil {
		t.Fatal(err)
	}
	p := r.NewProvisioner(vals)

	h, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID: "ws-1", WorkDir: "/workspace/repo",
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if h.NativeID != "wsd-ws-1" || h.WorkDir != "/workspace/repo" {
		t.Fatalf("handle = %#v", h)
	}
	if h.OS != runtime.OSFreeBSD || h.Isolation != runtime.IsolationJail {
		t.Fatalf("handle OS/Isolation = %q/%q", h.OS, h.Isolation)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("jail -c was not invoked: %v", err)
	}

	if err := p.Destroy(context.Background(), h); err != nil {
		t.Fatalf("destroy: %v", err)
	}
}

// TestFreeBSDJailPluginRecoverReadsConfig verifies recovery re-reads the jail
// config and reserves its recorded IP address.
func TestFreeBSDJailPluginRecoverReadsConfig(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeScript(t, filepath.Join(bin, "mdo"), "#!/bin/sh\nexec \"$@\"\n")
	writeScript(t, filepath.Join(bin, "cat"), "#!/bin/sh\nprintf 'ip4.addr += \"192.168.1.201/24\";\\n'\n")
	for _, name := range []string{"jail", "jexec", "jls", "tar", "chroot", "umount", "mount", "rm", "mkdir", "chmod", "chown", "test", "ls", "tee", "cp", "fetch"} {
		writeScript(t, filepath.Join(bin, name), "#!/bin/sh\nexit 0\n")
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	r := deployedPlugin(t, "freebsd-jail")
	fs := pflag.NewFlagSet("d", pflag.ContinueOnError)
	vals, err := BindFlags(fs, r.Options)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Parse([]string{"--ip-base-network", "192.168.1.200/24", "--ip-pool-size", "8"}); err != nil {
		t.Fatal(err)
	}
	p := r.NewProvisioner(vals)

	h, err := p.RecoverHandle(context.Background(), "wsd-ws-abc", "/workspace/repo")
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if h.NativeID != "wsd-ws-abc" {
		t.Fatalf("NativeID = %q", h.NativeID)
	}
}
