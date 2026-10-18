package podman

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal/runtime"
)

func TestNewPodmanExecTransport(t *testing.T) {
	containerID := "test-container-123"
	tr := NewPodmanExecTransport(containerID)
	if tr == nil {
		t.Fatal("NewPodmanExecTransport returned nil")
	}
	if tr.containerID != containerID {
		t.Errorf("containerID = %q, want %q", tr.containerID, containerID)
	}
}

func TestPodmanExecTransport_ImplementsInterface(t *testing.T) {
	tr := NewPodmanExecTransport("test-container")
	var _ runtime.ExecTransport = tr
}

// TestExec_SimpleCommand tests executing a simple command in a container.
// NOTE: This is an integration test requiring 'podman' to be installed and running.
func TestExec_SimpleCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// First, create a container to test with
	p := NewPodmanProvisioner()
	ctx := context.Background()

	spec := runtime.ProvisionSpec{
		WorkspaceID: "test-exec-simple",
		Image:       "alpine:latest",
		WorkDir:     "/workspace",
	}

	handle, err := p.Provision(ctx, spec)
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	defer func() {
		if err := p.Destroy(ctx, handle); err != nil {
			t.Logf("warn: Destroy failed during cleanup: %v", err)
		}
	}()

	transport := handle.Transport.(*PodmanExecTransport)

	result, err := transport.Exec(ctx, []string{"echo", "hello"}, "/", nil)
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}

	if result == nil {
		t.Fatal("Exec returned nil result")
	}

	// Check for "hello" in stdout (output may have newline)
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
}

// TestExec_WithWorkDir tests executing a command with a specific working directory.
// NOTE: This is an integration test requiring 'podman' to be installed and running.
func TestExec_WithWorkDir(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	p := NewPodmanProvisioner()
	ctx := context.Background()

	spec := runtime.ProvisionSpec{
		WorkspaceID: "test-exec-workdir",
		Image:       "alpine:latest",
		WorkDir:     "/workspace",
	}

	handle, err := p.Provision(ctx, spec)
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	defer func() {
		if err := p.Destroy(ctx, handle); err != nil {
			t.Logf("warn: Destroy failed during cleanup: %v", err)
		}
	}()

	transport := handle.Transport.(*PodmanExecTransport)

	result, err := transport.Exec(ctx, []string{"pwd"}, "/tmp", nil)
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}

	if result == nil {
		t.Fatal("Exec returned nil result")
	}

	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
}

// TestExec_WithEnvironment tests executing a command with environment variables.
// NOTE: This is an integration test requiring 'podman' to be installed and running.
func TestExec_WithEnvironment(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	p := NewPodmanProvisioner()
	ctx := context.Background()

	spec := runtime.ProvisionSpec{
		WorkspaceID: "test-exec-env",
		Image:       "alpine:latest",
		WorkDir:     "/workspace",
	}

	handle, err := p.Provision(ctx, spec)
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	defer func() {
		if err := p.Destroy(ctx, handle); err != nil {
			t.Logf("warn: Destroy failed during cleanup: %v", err)
		}
	}()

	transport := handle.Transport.(*PodmanExecTransport)

	env := []string{"MY_VAR=test_value"}
	result, err := transport.Exec(ctx, []string{"sh", "-c", "echo $MY_VAR"}, "/", env)
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}

	if result == nil {
		t.Fatal("Exec returned nil result")
	}

	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
}

// TestExec_NonZeroExit tests command that exits with non-zero status.
// NOTE: This is an integration test requiring 'podman' to be installed and running.
func TestExec_NonZeroExit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	p := NewPodmanProvisioner()
	ctx := context.Background()

	spec := runtime.ProvisionSpec{
		WorkspaceID: "test-exec-nonzero",
		Image:       "alpine:latest",
		WorkDir:     "/workspace",
	}

	handle, err := p.Provision(ctx, spec)
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	defer func() {
		if err := p.Destroy(ctx, handle); err != nil {
			t.Logf("warn: Destroy failed during cleanup: %v", err)
		}
	}()

	transport := handle.Transport.(*PodmanExecTransport)

	// 'false' command exits with status 1
	result, err := transport.Exec(ctx, []string{"false"}, "/", nil)
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}

	if result == nil {
		t.Fatal("Exec returned nil result")
	}

	if result.ExitCode == 0 {
		t.Errorf("ExitCode = 0, want non-zero")
	}
}

// TestSetNetwork_Enabled tests enabling network (should be no-op for Podman).
func TestSetNetwork_Enabled(t *testing.T) {
	transport := NewPodmanExecTransport("test-container")
	ctx := context.Background()

	err := transport.SetNetwork(ctx, true)
	if err != nil {
		t.Errorf("SetNetwork(true) failed: %v", err)
	}
}

// TestSetNetwork_Disabled tests disabling network (should be no-op for Podman).
func TestSetNetwork_Disabled(t *testing.T) {
	transport := NewPodmanExecTransport("test-container")
	ctx := context.Background()

	err := transport.SetNetwork(ctx, false)
	if err != nil {
		t.Errorf("SetNetwork(false) failed: %v", err)
	}
}

// TestCopyDirIn_ToWorkspace tests copying a directory into container.
// NOTE: This is an integration test requiring 'podman' to be installed and running.
func TestCopyDirIn_ToWorkspace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	p := NewPodmanProvisioner()
	ctx := context.Background()

	spec := runtime.ProvisionSpec{
		WorkspaceID: "test-copyin",
		Image:       "alpine:latest",
		WorkDir:     "/workspace",
	}

	handle, err := p.Provision(ctx, spec)
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	defer func() {
		if err := p.Destroy(ctx, handle); err != nil {
			t.Logf("warn: Destroy failed during cleanup: %v", err)
		}
	}()

	transport := handle.Transport.(*PodmanExecTransport)

	// Create a temporary directory with test files
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("test content"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	// Copy into container
	err = transport.CopyDirIn(ctx, tmpDir, "/tmp/test-copy")
	if err != nil {
		t.Fatalf("CopyDirIn failed: %v", err)
	}

	// Verify file exists in container
	result, err := transport.Exec(ctx, []string{"ls", "/tmp/test-copy"}, "/", nil)
	if err != nil {
		t.Fatalf("Exec ls failed: %v", err)
	}

	if result.ExitCode != 0 {
		t.Errorf("File listing failed with exit code %d", result.ExitCode)
	}
}

// TestReadFile tests reading a file from container.
// NOTE: This is an integration test requiring 'podman' to be installed and running.
func TestReadFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	p := NewPodmanProvisioner()
	ctx := context.Background()

	spec := runtime.ProvisionSpec{
		WorkspaceID: "test-readfile",
		Image:       "alpine:latest",
		WorkDir:     "/workspace",
	}

	handle, err := p.Provision(ctx, spec)
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	defer func() {
		if err := p.Destroy(ctx, handle); err != nil {
			t.Logf("warn: Destroy failed during cleanup: %v", err)
		}
	}()

	transport := handle.Transport.(*PodmanExecTransport)

	// Write a file in container
	testContent := []byte("hello from container")
	if err := transport.WriteFile(ctx, "/tmp/test-file.txt", testContent, 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Read it back
	content, err := transport.ReadFile(ctx, "/tmp/test-file.txt")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	if string(content) != string(testContent) {
		t.Errorf("ReadFile content = %q, want %q", string(content), string(testContent))
	}
}

// TestWriteFile tests writing a file to container.
// NOTE: This is an integration test requiring 'podman' to be installed and running.
func TestWriteFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	p := NewPodmanProvisioner()
	ctx := context.Background()

	spec := runtime.ProvisionSpec{
		WorkspaceID: "test-writefile",
		Image:       "alpine:latest",
		WorkDir:     "/workspace",
	}

	handle, err := p.Provision(ctx, spec)
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	defer func() {
		if err := p.Destroy(ctx, handle); err != nil {
			t.Logf("warn: Destroy failed during cleanup: %v", err)
		}
	}()

	transport := handle.Transport.(*PodmanExecTransport)

	// Write a file
	testContent := []byte("test file content")
	err = transport.WriteFile(ctx, "/tmp/write-test.txt", testContent, 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Verify it exists and has correct content
	result, err := transport.Exec(ctx, []string{"cat", "/tmp/write-test.txt"}, "/", nil)
	if err != nil {
		t.Fatalf("Exec cat failed: %v", err)
	}

	if result.ExitCode != 0 {
		t.Errorf("cat failed with exit code %d, stderr: %s", result.ExitCode, result.Stderr)
	}
}

// TestWriteFile_WithDifferentPermissions tests file mode is applied correctly.
// NOTE: This is an integration test requiring 'podman' to be installed and running.
func TestWriteFile_WithDifferentPermissions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	p := NewPodmanProvisioner()
	ctx := context.Background()

	spec := runtime.ProvisionSpec{
		WorkspaceID: "test-writefile-perms",
		Image:       "alpine:latest",
		WorkDir:     "/workspace",
	}

	handle, err := p.Provision(ctx, spec)
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	defer func() {
		if err := p.Destroy(ctx, handle); err != nil {
			t.Logf("warn: Destroy failed during cleanup: %v", err)
		}
	}()

	transport := handle.Transport.(*PodmanExecTransport)

	// Write a file with executable permission
	testContent := []byte("#!/bin/sh\necho hello")
	err = transport.WriteFile(ctx, "/tmp/script.sh", testContent, 0755)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Verify it's executable
	result, err := transport.Exec(ctx, []string{"sh", "/tmp/script.sh"}, "/", nil)
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}

	if result.ExitCode != 0 {
		t.Errorf("Script execution failed with exit code %d", result.ExitCode)
	}
}

// TestExec_CapturesStdout tests that stdout is properly captured.
// NOTE: This is an integration test requiring 'podman' to be installed and running.
func TestExec_CapturesStdout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	p := NewPodmanProvisioner()
	ctx := context.Background()

	spec := runtime.ProvisionSpec{
		WorkspaceID: "test-stdout",
		Image:       "alpine:latest",
		WorkDir:     "/workspace",
	}

	handle, err := p.Provision(ctx, spec)
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	defer func() {
		if err := p.Destroy(ctx, handle); err != nil {
			t.Logf("warn: Destroy failed during cleanup: %v", err)
		}
	}()

	transport := handle.Transport.(*PodmanExecTransport)

	result, err := transport.Exec(ctx, []string{"echo", "test output"}, "/", nil)
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}

	if len(result.Stdout) == 0 {
		t.Error("Stdout is empty")
	}

	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
}

// TestExec_CapturesStderr tests that stderr is properly captured.
// NOTE: This is an integration test requiring 'podman' to be installed and running.
func TestExec_CapturesStderr(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	p := NewPodmanProvisioner()
	ctx := context.Background()

	spec := runtime.ProvisionSpec{
		WorkspaceID: "test-stderr",
		Image:       "alpine:latest",
		WorkDir:     "/workspace",
	}

	handle, err := p.Provision(ctx, spec)
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}
	defer func() {
		if err := p.Destroy(ctx, handle); err != nil {
			t.Logf("warn: Destroy failed during cleanup: %v", err)
		}
	}()

	transport := handle.Transport.(*PodmanExecTransport)

	result, err := transport.Exec(ctx, []string{"sh", "-c", "echo error message >&2"}, "/", nil)
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}

	if len(result.Stderr) == 0 {
		t.Error("Stderr is empty")
	}

	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
}
