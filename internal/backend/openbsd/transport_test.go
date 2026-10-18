package openbsd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal/runtime"
)

func TestNewSSHExecTransport_Defaults(t *testing.T) {
	tr := NewSSHExecTransport("100.64.6.3", 0, "", "", "", "")
	if tr.port != defaultSSHPort {
		t.Errorf("port = %d, want %d", tr.port, defaultSSHPort)
	}
	if tr.user != defaultLoginUser {
		t.Errorf("user = %q, want %q", tr.user, defaultLoginUser)
	}
	if tr.os != runtime.OSOpenBSD {
		t.Errorf("os = %q, want openbsd", tr.os)
	}
	if tr.sshBinary != "ssh" {
		t.Errorf("sshBinary = %q, want ssh", tr.sshBinary)
	}
}

func TestSSHExecTransport_ImplementsInterface(t *testing.T) {
	var _ runtime.ExecTransport = NewSSHExecTransport("1.2.3.4", 22, "root", "/k", "", runtime.OSOpenBSD)
}

func TestBuildSSHArgs(t *testing.T) {
	args := buildSSHArgs("100.64.6.3", 2222, "wsd", "/keys/k", "echo hi")

	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-o StrictHostKeyChecking=no",
		"-o UserKnownHostsFile=/dev/null",
		"-o BatchMode=yes",
		"-i /keys/k",
		"-p 2222",
		"wsd@100.64.6.3",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("buildSSHArgs missing %q: %v", want, args)
		}
	}
	if args[len(args)-1] != "echo hi" {
		t.Errorf("payload must be the final argument, got %q", args[len(args)-1])
	}

	// No key path means no -i.
	noKey := buildSSHArgs("1.2.3.4", 22, "root", "", "true")
	if strings.Contains(strings.Join(noKey, " "), "-i") {
		t.Errorf("buildSSHArgs must omit -i when no key is configured: %v", noKey)
	}
}

// TestBuildRemoteCommand verifies cd and env handling. Env assignments must
// come after the cd so they apply to the command, not to cd itself.
func TestBuildRemoteCommand(t *testing.T) {
	got := buildRemoteCommand([]string{"sh", "-c", "pwd"}, "/workspace/repo", []string{"A=1", "B=2"})
	want := "cd /workspace/repo && A=1 B=2 sh -c pwd"
	if got != want {
		t.Fatalf("buildRemoteCommand = %q, want %q", got, want)
	}

	if got := buildRemoteCommand([]string{"ls"}, "", nil); got != "ls" {
		t.Fatalf("buildRemoteCommand(no workdir/env) = %q, want ls", got)
	}
}

// TestRemotePayloadEscalation verifies the escalation wrap runs the inner
// payload through a shell, so compound commands still work.
func TestRemotePayloadEscalation(t *testing.T) {
	direct := NewSSHExecTransport("1.2.3.4", 22, "root", "/k", "", runtime.OSOpenBSD)
	if got := direct.remotePayload([]string{"echo", "hi"}, "/w", nil); got != "cd /w && echo hi" {
		t.Errorf("direct payload = %q", got)
	}

	esc := NewSSHExecTransport("1.2.3.4", 22, "wsd", "/k", "doas", runtime.OSOpenBSD)
	got := esc.remotePayload([]string{"echo", "hi"}, "/w", nil)
	if !strings.HasPrefix(got, "doas sh -c ") {
		t.Errorf("escalated payload = %q, want it wrapped in doas sh -c", got)
	}
	if !strings.Contains(got, "cd /w && echo hi") {
		t.Errorf("escalated payload lost the inner command: %q", got)
	}
}

// TestExecRunsSSH verifies Exec invokes the configured ssh binary with the
// assembled argument vector and captures its output.
func TestExecRunsSSH(t *testing.T) {
	ssh := fakeSSH(t, "#!/bin/sh\nprintf '%s\\n' \"$@\"\n")
	tr := NewSSHExecTransport("100.64.6.3", 22, "root", "/k", "", runtime.OSOpenBSD)
	tr.sshBinary = ssh

	res, err := tr.Exec(context.Background(), []string{"echo", "hi"}, "/workspace/repo", nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("Exec exit = %d, want 0 (stderr %q)", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "root@100.64.6.3") {
		t.Errorf("Exec did not target the guest: %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "cd /workspace/repo && PATH=") {
		t.Errorf("Exec payload missing cd/PATH: %q", res.Stdout)
	}
}

// TestReadFile verifies ReadFile runs cat in the guest and returns stdout.
func TestReadFile(t *testing.T) {
	ssh := fakeSSH(t, "#!/bin/sh\nprintf '%s\\n' \"$@\"\n")
	tr := NewSSHExecTransport("100.64.6.3", 22, "root", "/k", "", runtime.OSOpenBSD)
	tr.sshBinary = ssh

	content, err := tr.ReadFile(context.Background(), "/workspace/repo/a.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(content), "/workspace/repo/a.txt") {
		t.Errorf("ReadFile output = %q, want it to mention the path", content)
	}
}

// TestWriteFile verifies WriteFile streams content on stdin without an error.
func TestWriteFile(t *testing.T) {
	ssh := fakeSSH(t, "#!/bin/sh\ncat >/dev/null 2>&1\nexit 0\n")
	tr := NewSSHExecTransport("100.64.6.3", 22, "root", "/k", "", runtime.OSOpenBSD)
	tr.sshBinary = ssh

	if err := tr.WriteFile(context.Background(), "/workspace/repo/a.txt", []byte("line1\nline2\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestSetNetworkIsNoop(t *testing.T) {
	tr := NewSSHExecTransport("1.2.3.4", 22, "root", "/k", "", runtime.OSOpenBSD)
	if err := tr.SetNetwork(context.Background(), false); err != nil {
		t.Fatalf("SetNetwork should be a no-op, got %v", err)
	}
}

// TestRootLoginSkipsEscalation verifies that a root login does not inherit a
// guest escalation setting: root needs no escalation, and wrapping it would
// only break commands in the guest.
func TestRootLoginSkipsEscalation(t *testing.T) {
	root := NewSSHExecTransport("1.2.3.4", 22, "root", "/k", "doas", runtime.OSOpenBSD)
	if root.escalationCommand != "" {
		t.Fatalf("root transport must not escalate, got %q", root.escalationCommand)
	}
	if got := root.remotePayload([]string{"true"}, "", nil); strings.HasPrefix(got, "doas") {
		t.Fatalf("root payload must not be wrapped in doas: %q", got)
	}

	nonRoot := NewSSHExecTransport("1.2.3.4", 22, "wsd", "/k", "doas", runtime.OSOpenBSD)
	if nonRoot.escalationCommand != "doas" {
		t.Fatalf("non-root transport should keep escalation, got %q", nonRoot.escalationCommand)
	}
}

func TestCreateTarArchive(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	tarPath, err := createTarArchive(src)
	if err != nil {
		t.Fatalf("createTarArchive: %v", err)
	}
	defer os.Remove(tarPath)

	info, err := os.Stat(tarPath)
	if err != nil || info.Size() == 0 {
		t.Fatalf("tar archive missing or empty: %v", err)
	}
}

// fakeSSH writes an executable shell script that stands in for ssh(1).
func fakeSSH(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ssh")
	if err := os.WriteFile(path, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}
