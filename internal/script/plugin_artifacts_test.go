package script

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests load the deployed TypeScript->JS plugins (plugins/*.js) and
// verify the compiled artifacts behave as expected inside the goja host. They
// are skipped when the artifacts have not been built (`npm run deploy`).
func deployedPlugin(t *testing.T, name string) *Runtime {
	t.Helper()
	path := filepath.Join("..", "..", "plugins", name+".js")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("plugin artifact %s not built (run `cd plugin-devel && npm run deploy`)", path)
	}
	r, err := Load(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return r
}

func TestDeployedPodmanPlugin(t *testing.T) {
	r := deployedPlugin(t, "podman")
	if r.Name != "podman" || r.OS != "linux" || r.Isolation != "container" {
		t.Fatalf("metadata = %q/%q/%q", r.Name, r.OS, r.Isolation)
	}
	if !r.binaries["podman"] {
		t.Fatalf("podman binary not allowed: %v", r.binaries)
	}

	tr := r.newTransport("abc", map[string]interface{}{"native_id": "abc"})
	argv, _, err := tr.execArgs([]string{"true"}, "/w", nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"podman", "exec", "--workdir", "/w", "abc", "true"}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, argv[i], want[i])
		}
	}
}

func TestDeployedDockerPluginOptions(t *testing.T) {
	r := deployedPlugin(t, "docker")
	found := false
	for _, o := range r.Options {
		if o.Flag == "docker-network" {
			found = true
			if o.Type != "string" || o.Default != "bridge" {
				t.Fatalf("docker-network option = %+v", o)
			}
		}
	}
	if !found {
		t.Fatal("docker-network option not declared")
	}

	tr := r.newTransport("abc", map[string]interface{}{"native_id": "abc"})
	argv, _, err := tr.execArgs([]string{"true"}, "/w", nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"docker", "exec", "--workdir", "/w", "abc", "true"}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
}

func TestDeployedOpenBSDVMDPlugin(t *testing.T) {
	r := deployedPlugin(t, "openbsd-vmd")
	if r.Name != "openbsd-vmd" || r.OS != "openbsd" || r.Isolation != "vm" {
		t.Fatalf("metadata = %q/%q/%q", r.Name, r.OS, r.Isolation)
	}
	for _, b := range []string{"vmctl", "ssh", "doas"} {
		if !r.binaries[b] {
			t.Fatalf("%s binary not allowed: %v", b, r.binaries)
		}
	}

	byFlag := make(map[string]Option, len(r.Options))
	for _, o := range r.Options {
		byFlag[o.Flag] = o
	}
	if byFlag["openbsd-vm-root"].Type != "string" || byFlag["openbsd-vm-root"].Default != "/var/wsd/vms/openbsd" {
		t.Fatalf("openbsd-vm-root option = %+v", byFlag["openbsd-vm-root"])
	}
	if byFlag["openbsd-ssh-port"].Type != "int" || byFlag["openbsd-ssh-port"].Default != "22" {
		t.Fatalf("openbsd-ssh-port option = %+v", byFlag["openbsd-ssh-port"])
	}
	if byFlag["openbsd-boot-timeout"].Type != "duration" {
		t.Fatalf("openbsd-boot-timeout option = %+v", byFlag["openbsd-boot-timeout"])
	}

	// The transport VM's host only exposes options; bind a minimal one so the
	// execArgs mapping can resolve the SSH port fallback.
	if err := r.bindTransportOptions(map[string]interface{}{
		"openbsd-ssh-port": int64(22),
	}); err != nil {
		t.Fatal(err)
	}

	// execArgs maps onto ssh(1) with a cd + PATH payload as the final argv
	// element (the remote command handed to the login shell).
	tr := r.newTransport("wsd-ws", map[string]interface{}{
		"native_id": "wsd-ws",
		"ip":        "100.64.6.3",
		"port":      22,
		"username":  "root",
		"key_path":  "/var/wsd/agent_ed25519",
		"escalation": "",
	})
	argv, _, err := tr.execArgs([]string{"echo", "hi"}, "/workspace/repo", nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=55",
		"-o", "LogLevel=ERROR",
		"-i", "/var/wsd/agent_ed25519",
		"-p", "22",
		"root@100.64.6.3",
		"cd /workspace/repo && PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin:/usr/local/bin echo hi",
	}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, argv[i], want[i])
		}
	}
}

func TestDeployedKubernetesPlugin(t *testing.T) {
	r := deployedPlugin(t, "kubernetes")
	if r.Name != "kubernetes" || r.OS != "linux" || r.Isolation != "container" {
		t.Fatalf("metadata = %q/%q/%q", r.Name, r.OS, r.Isolation)
	}
	if !r.binaries["kubectl"] {
		t.Fatalf("kubectl binary not allowed: %v", r.binaries)
	}

	byFlag := make(map[string]Option, len(r.Options))
	for _, o := range r.Options {
		byFlag[o.Flag] = o
	}
	if byFlag["kubernetes-namespace"].Type != "string" || byFlag["kubernetes-namespace"].Default != "default" {
		t.Fatalf("kubernetes-namespace option = %+v", byFlag["kubernetes-namespace"])
	}
	if byFlag["kubernetes-kubeconfig"].Type != "string" {
		t.Fatalf("kubernetes-kubeconfig option = %+v", byFlag["kubernetes-kubeconfig"])
	}

	// The transport VM's host only exposes options; bind a minimal one so the
	// execArgs mapping can resolve the namespace.
	if err := r.bindTransportOptions(map[string]interface{}{
		"kubernetes-namespace":  "default",
		"kubernetes-kubeconfig": "",
	}); err != nil {
		t.Fatal(err)
	}

	tr := r.newTransport("wsd-ws", map[string]interface{}{"native_id": "wsd-ws"})
	argv, _, err := tr.execArgs([]string{"true"}, "/w", nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"kubectl", "-n", "default", "exec", "wsd-ws", "--", "sh", "-c", "cd /w && true"}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, argv[i], want[i])
		}
	}

	// The shell-quoting quirk: workdir/env/args with spaces and semicolons must
	// be rendered as a single sh -c payload, not spliced into host argv.
	argv, _, err = tr.execArgs([]string{"printf", "%s", "a b;c"}, "/w rk dir", []string{"V=v w"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"kubectl", "-n", "default", "exec", "wsd-ws", "--", "sh", "-c",
		"cd '/w rk dir' && env 'V=v w' printf %s 'a b;c'"}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, argv[i], want[i])
		}
	}
}

func TestDeployedFreeBSDJailPlugin(t *testing.T) {
	r := deployedPlugin(t, "freebsd-jail")
	if r.Name != "freebsd-jail" || r.OS != "freebsd" || r.Isolation != "jail" {
		t.Fatalf("metadata = %q/%q/%q", r.Name, r.OS, r.Isolation)
	}
	for _, b := range []string{"jail", "jexec", "jls", "tar", "chroot", "mdo", "tee", "fetch"} {
		if !r.binaries[b] {
			t.Fatalf("%s binary not allowed: %v", b, r.binaries)
		}
	}

	byFlag := make(map[string]Option, len(r.Options))
	for _, o := range r.Options {
		byFlag[o.Flag] = o
	}
	if byFlag["freebsd-release"].Type != "string" || byFlag["freebsd-release"].Default != "15.1-RELEASE" {
		t.Fatalf("freebsd-release option = %+v", byFlag["freebsd-release"])
	}
	if byFlag["ip-base-network"].Type != "string" {
		t.Fatalf("ip-base-network option = %+v", byFlag["ip-base-network"])
	}
	if byFlag["ip-pool-size"].Type != "int" || byFlag["ip-pool-size"].Default != "0" {
		t.Fatalf("ip-pool-size option = %+v", byFlag["ip-pool-size"])
	}
	if byFlag["escalation-command"].Type != "string" || byFlag["escalation-command"].Default != "mdo" {
		t.Fatalf("escalation-command option = %+v", byFlag["escalation-command"])
	}
	if byFlag["verify-signature"].Type != "bool" {
		t.Fatalf("verify-signature option = %+v", byFlag["verify-signature"])
	}

	// The transport VM's host only exposes options; bind the escalator so the
	// execArgs mapping resolves it.
	if err := r.bindTransportOptions(map[string]interface{}{
		"escalation-command": "mdo",
	}); err != nil {
		t.Fatal(err)
	}

	tr := r.newTransport("wsd-ws", map[string]interface{}{"native_id": "wsd-ws"})
	argv, _, err := tr.execArgs([]string{"echo", "hi"}, "/w", []string{"A=1"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	const jailPath = "/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin:/usr/local/bin"
	want := []string{"mdo", "jexec", "-d", "/w", "wsd-ws", "env", "PATH=" + jailPath, "A=1", "echo", "hi"}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, argv[i], want[i])
		}
	}
}

func TestDeployedZonesPlugin(t *testing.T) {
	r := deployedPlugin(t, "zones")
	if r.Name != "zones" || r.OS != "illumos" || r.Isolation != "zone" {
		t.Fatalf("metadata = %q/%q/%q", r.Name, r.OS, r.Isolation)
	}
	for _, b := range []string{"zoneadm", "zonecfg", "zlogin", "zfs", "dladm", "ipadm", "pfexec"} {
		if !r.binaries[b] {
			t.Fatalf("%s binary not allowed: %v", b, r.binaries)
		}
	}

	byFlag := make(map[string]Option, len(r.Options))
	for _, o := range r.Options {
		byFlag[o.Flag] = o
	}
	if byFlag["zones-zonepath"].Type != "string" {
		t.Fatalf("zones-zonepath option = %+v", byFlag["zones-zonepath"])
	}
	if byFlag["ip-base-network"].Type != "string" {
		t.Fatalf("ip-base-network option = %+v", byFlag["ip-base-network"])
	}
	if byFlag["ip-pool-size"].Type != "int" || byFlag["ip-pool-size"].Default != "0" {
		t.Fatalf("ip-pool-size option = %+v", byFlag["ip-pool-size"])
	}
	if byFlag["zones-escalation"].Type != "string" || byFlag["zones-escalation"].Default != "pfexec" {
		t.Fatalf("zones-escalation option = %+v", byFlag["zones-escalation"])
	}

	// The transport VM's host only exposes options; bind the escalator so the
	// execArgs mapping resolves it.
	if err := r.bindTransportOptions(map[string]interface{}{
		"zones-escalation": "pfexec",
	}); err != nil {
		t.Fatal(err)
	}

	tr := r.newTransport("wsd-ws", map[string]interface{}{"native_id": "wsd-ws", "vnic": ""})

	// A plain exec streams the command script to `zlogin <zone> sh -s` on stdin.
	argv, stdin, err := tr.execArgs([]string{"git", "status"}, "/workspace/repo", []string{"FOO=bar"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pfexec", "zlogin", "wsd-ws", "sh", "-s"}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, argv[i], want[i])
		}
	}
	if stdin == nil {
		t.Fatal("execArgs returned no stdin payload for the zlogin sh -s form")
	}
	const zonesPath = "/usr/sbin:/usr/bin:/sbin:/bin:/usr/local/sbin:/usr/local/bin"
	for _, sub := range []string{"cd /workspace/repo &&", "env PATH=" + zonesPath, "FOO=bar", "git status"} {
		if !strings.Contains(*stdin, sub) {
			t.Fatalf("stdin payload missing %q: %q", sub, *stdin)
		}
	}

	// WriteFile maps to `zlogin <zone> tee <path>`; CopyDirIn maps to tar.
	argv, stdin, err = tr.execArgs([]string{"sh", "-c", `cat > "$1"`, "sh", "/w/file"}, "", nil, true, true)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"pfexec", "zlogin", "wsd-ws", "tee", "/w/file"}
	if len(argv) != len(want) {
		t.Fatalf("write argv = %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("write argv[%d] = %q, want %q", i, argv[i], want[i])
		}
	}
	if stdin != nil {
		t.Fatalf("write execArgs returned an unexpected stdin payload: %q", *stdin)
	}
}
