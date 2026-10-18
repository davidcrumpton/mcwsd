package freebsd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal/runtime"
)

func TestNewFreeBSDJailExecTransport(t *testing.T) {
	transport := NewFreeBSDJailExecTransport("test-jail")
	if transport == nil {
		t.Fatal("NewFreeBSDJailExecTransport returned nil")
	}
	if transport.jailName != "test-jail" {
		t.Errorf("jailName = %q, want %q", transport.jailName, "test-jail")
	}
}

func TestFreeBSDJailExecTransport_ImplementsInterface(t *testing.T) {
	transport := NewFreeBSDJailExecTransport("test-jail")
	var _ runtime.ExecTransport = transport
}

func TestFreeBSDJailExecTransport_ImplementsBootstrapper(t *testing.T) {
	transport := NewFreeBSDJailExecTransport("test-jail")
	var _ runtime.Bootstrapper = transport
}

func TestRedactJexecEnvironment(t *testing.T) {
	args := []string{"test-jail", "env", "PATH=/usr/bin", "TOKEN=secret-value", "sh", "-c", "true"}
	got := redactJexecEnvironment(args)
	if strings.Contains(strings.Join(got, " "), "secret-value") {
		t.Fatalf("environment value leaked in debug args: %q", got)
	}
	if got[2] != "PATH=[redacted]" || got[3] != "TOKEN=[redacted]" {
		t.Fatalf("unexpected redacted args: %#v", got)
	}
	if args[2] != "PATH=/usr/bin" || args[3] != "TOKEN=secret-value" {
		t.Fatalf("redaction mutated input args: %#v", args)
	}
}

// TestBootstrapScriptIncludesTrustStore guards the ordering that makes pkg
// work in a jail untarred from base.txz: the OpenSSL trust store must be
// rebuilt before pkg tries to bootstrap over HTTPS, and git must be installed
// for the commit flow.
func TestBootstrapScriptIncludesTrustStore(t *testing.T) {
	rehash := strings.Index(bootstrapScript, "certctl rehash")
	bootstrap := strings.Index(bootstrapScript, "pkg bootstrap")
	if rehash == -1 {
		t.Fatalf("bootstrapScript must run certctl rehash first: %s", bootstrapScript)
	}
	if bootstrap == -1 {
		t.Fatalf("bootstrapScript must bootstrap pkg: %s", bootstrapScript)
	}
	if rehash > bootstrap {
		t.Errorf("certctl rehash must precede pkg bootstrap: %s", bootstrapScript)
	}
	if !strings.Contains(bootstrapScript, "pkg install -y git") {
		t.Errorf("bootstrapScript must install git: %s", bootstrapScript)
	}
	if !strings.Contains(bootstrapScript, "safe.directory") {
		t.Errorf("bootstrapScript must allow the non-root-owned workspace repo: %s", bootstrapScript)
	}
}

func TestFreeBSDJailExecTransport_GetJailID(t *testing.T) {
	transport := NewFreeBSDJailExecTransport("my-jail")
	if id := transport.GetJailID(); id != "my-jail" {
		t.Errorf("GetJailID() = %q, want %q", id, "my-jail")
	}
}

func TestFreeBSDJailExecTransport_VerifyJailRunning(t *testing.T) {
	// This test just verifies the method exists and doesn't panic
	// Actual jail running verification would require a FreeBSD system
	transport := NewFreeBSDJailExecTransport("nonexistent-jail")
	// Should return false for non-existent jail
	result := transport.VerifyJailRunning(context.Background())
	// We can't guarantee the result without a FreeBSD system, so just check it doesn't panic
	t.Logf("VerifyJailRunning returned: %v", result)
}

func TestCreateTarArchive(t *testing.T) {
	// Create a temporary directory with some files
	tmpDir, err := os.MkdirTemp("", "wsd-test-tar-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create some test files
	testFile1 := filepath.Join(tmpDir, "file1.txt")
	testFile2 := filepath.Join(tmpDir, "subdir", "file2.txt")

	if err := os.WriteFile(testFile1, []byte("content1"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(testFile2), 0755); err != nil {
		t.Fatalf("Failed to create subdir: %v", err)
	}

	if err := os.WriteFile(testFile2, []byte("content2"), 0644); err != nil {
		t.Fatalf("Failed to create test file 2: %v", err)
	}

	// Create the tar archive
	tarPath, err := createTarArchive(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create tar archive: %v", err)
	}
	defer os.Remove(tarPath)

	// Verify the tar file was created
	if _, err := os.Stat(tarPath); os.IsNotExist(err) {
		t.Error("Tar file was not created")
	}

	// Verify the tar file has content
	info, err := os.Stat(tarPath)
	if err != nil {
		t.Fatalf("Failed to stat tar file: %v", err)
	}
	if info.Size() == 0 {
		t.Error("Tar file is empty")
	}
	t.Logf("Created tar archive at %s with size %d bytes", tarPath, info.Size())
}

func TestTransportIntegration(t *testing.T) {
	// This test verifies that the transport can be created and the interface is satisfied
	transport := NewFreeBSDJailExecTransport("test-jail")

	// Verify interface implementation
	var _ runtime.ExecTransport = transport

	// Verify the jail name is stored
	if transport.jailName != "test-jail" {
		t.Errorf("Expected jailName 'test-jail', got '%s'", transport.jailName)
	}
}

// TestBuildWriteFileArgs verifies that file content is never interpolated
// into the shell command: it is streamed on stdin, and the path is bound as
// $1 so it needs no quoting. This is what makes newlines and shell
// metacharacters safe to write.
func TestBuildWriteFileArgs(t *testing.T) {
	got := buildWriteFileArgs("my-jail", "/workspace/repo/node.c")
	want := []string{"my-jail", "sh", "-c", `cat > "$1"`, "sh", "/workspace/repo/node.c"}
	if len(got) != len(want) {
		t.Fatalf("buildWriteFileArgs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("buildWriteFileArgs()[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}

	// A path with spaces must survive verbatim as the $1 argument.
	spaced := buildWriteFileArgs("my-jail", "/workspace/my repo/a file.txt")
	if spaced[len(spaced)-1] != "/workspace/my repo/a file.txt" {
		t.Errorf("path with spaces was altered: %v", spaced)
	}
}

// TestTransportEscalation verifies that root-only transport commands are
// routed through the configured escalation command (e.g. mdo) when set, and
// run directly when it is not.
func TestTransportEscalation(t *testing.T) {
	escalated := NewFreeBSDJailExecTransport("my-jail").WithEscalationCommand("mdo")
	cmd := escalated.command(context.Background(), "jexec", "my-jail", "true")
	want := []string{"mdo", "jexec", "my-jail", "true"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("escalated args = %v, want %v", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Fatalf("escalated args[%d] = %q, want %q (full: %v)", i, cmd.Args[i], want[i], cmd.Args)
		}
	}

	direct := NewFreeBSDJailExecTransport("my-jail")
	cmd = direct.command(context.Background(), "jexec", "my-jail", "true")
	want = []string{"jexec", "my-jail", "true"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("direct args = %v, want %v", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Fatalf("direct args[%d] = %q, want %q (full: %v)", i, cmd.Args[i], want[i], cmd.Args)
		}
	}
}

// TestBuildSetNetworkArgs guards the jail -m argument form: the jail must be
// named via name=, never passed positionally, or jail treats it as a
// parameter ("unknown parameter").
func TestBuildSetNetworkArgs(t *testing.T) {
	got := buildSetNetworkArgs("wsd-ws-abc", "inherit")
	want := []string{"-m", "name=wsd-ws-abc", "ip4=inherit"}
	if len(got) != len(want) {
		t.Fatalf("buildSetNetworkArgs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("buildSetNetworkArgs()[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}

	got = buildSetNetworkArgs("wsd-ws-abc", "disable")
	if got[2] != "ip4=disable" {
		t.Errorf("disable args = %v, want ip4=disable", got)
	}
}

func TestBuildJexecArgs(t *testing.T) {
	tests := []struct {
		name    string
		cmd     []string
		workDir string
		env     []string
		want    []string
	}{
		{
			name:    "workdir and command",
			cmd:     []string{"sh", "-c", "pwd"},
			workDir: "/workspace/repo",
			want:    []string{"-d", "/workspace/repo", "my-jail", "env", "PATH=" + jailPath, "sh", "-c", "pwd"},
		},
		{
			name:    "no workdir",
			cmd:     []string{"ls"},
			workDir: "",
			want:    []string{"my-jail", "env", "PATH=" + jailPath, "ls"},
		},
		{
			name:    "with env",
			cmd:     []string{"sh", "-c", "echo $A"},
			workDir: "/w",
			env:     []string{"A=1", "B=2"},
			want:    []string{"-d", "/w", "my-jail", "env", "PATH=" + jailPath, "A=1", "B=2", "sh", "-c", "echo $A"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildJexecArgs("my-jail", tt.cmd, tt.workDir, tt.env)
			if len(got) != len(tt.want) {
				t.Fatalf("buildJexecArgs() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("buildJexecArgs()[%d] = %q, want %q (full: %v)", i, got[i], tt.want[i], got)
				}
			}
			// Guard against the historical double-jexec bug.
			if len(got) > 0 && got[0] == "jexec" {
				t.Fatalf("buildJexecArgs() must not prepend jexec: %v", got)
			}
		})
	}
}
