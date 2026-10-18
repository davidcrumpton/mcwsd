package podman

import (
	"context"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal/runtime"
)

func TestNewPodmanProvisioner(t *testing.T) {
	p := NewPodmanProvisioner()
	if p == nil {
		t.Fatal("NewPodmanProvisioner returned nil")
	}
}

func TestPodmanProvisioner_Name(t *testing.T) {
	p := NewPodmanProvisioner()
	if name := p.Name(); name != "podman" {
		t.Errorf("Name() = %q, want %q", name, "podman")
	}
}

func TestPodmanProvisioner_ImplementsInterface(t *testing.T) {
	p := NewPodmanProvisioner()
	var _ runtime.Provisioner = p
}

// TestProvision_WithCacheMountPath tests container creation with cache mount.
// NOTE: This is an integration test requiring 'podman' to be installed and running.
// Run with: go test -run TestProvision_WithCacheMountPath -tags integration
func TestProvision_WithCacheMountPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	p := NewPodmanProvisioner()
	ctx := context.Background()

	spec := runtime.ProvisionSpec{
		WorkspaceID:     "test-workspace-123",
		Image:           "alpine:latest",
		WorkDir:         "/workspace",
		CacheMountPath:  "/tmp/wsd-cache",
		WorkspaceBasePath: "",
	}

	handle, err := p.Provision(ctx, spec)
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}

	if handle == nil {
		t.Fatal("Provision returned nil handle")
	}

	// Verify handle properties
	if handle.NativeID == "" {
		t.Error("NativeID is empty")
	}
	if handle.WorkDir != spec.WorkDir {
		t.Errorf("WorkDir = %q, want %q", handle.WorkDir, spec.WorkDir)
	}
	if handle.OS != runtime.OSLinux {
		t.Errorf("OS = %v, want %v", handle.OS, runtime.OSLinux)
	}
	if handle.Isolation != runtime.IsolationContainer {
		t.Errorf("Isolation = %v, want %v", handle.Isolation, runtime.IsolationContainer)
	}
	if handle.Transport == nil {
		t.Error("Transport is nil")
	}

	// Cleanup
	if err := p.Destroy(ctx, handle); err != nil {
		t.Logf("warn: Destroy failed during cleanup: %v", err)
	}
}

// TestProvision_WithWorkspaceBasePath tests container creation with workspace mount.
// NOTE: This is an integration test requiring 'podman' to be installed and running.
func TestProvision_WithWorkspaceBasePath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	p := NewPodmanProvisioner()
	ctx := context.Background()

	spec := runtime.ProvisionSpec{
		WorkspaceID:      "test-workspace-456",
		Image:            "alpine:latest",
		WorkDir:          "/workspace",
		CacheMountPath:   "",
		WorkspaceBasePath: "/tmp/wsd-workspaces",
	}

	handle, err := p.Provision(ctx, spec)
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}

	if handle == nil {
		t.Fatal("Provision returned nil handle")
	}

	// Cleanup
	if err := p.Destroy(ctx, handle); err != nil {
		t.Logf("warn: Destroy failed during cleanup: %v", err)
	}
}

// TestProvision_BothMounts tests container creation with both cache and workspace mounts.
// NOTE: This is an integration test requiring 'podman' to be installed and running.
func TestProvision_BothMounts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	p := NewPodmanProvisioner()
	ctx := context.Background()

	spec := runtime.ProvisionSpec{
		WorkspaceID:       "test-workspace-789",
		Image:             "alpine:latest",
		WorkDir:           "/workspace",
		CacheMountPath:    "/tmp/wsd-cache",
		WorkspaceBasePath: "/tmp/wsd-workspaces",
	}

	handle, err := p.Provision(ctx, spec)
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}

	if handle == nil {
		t.Fatal("Provision returned nil handle")
	}

	// Cleanup
	if err := p.Destroy(ctx, handle); err != nil {
		t.Logf("warn: Destroy failed during cleanup: %v", err)
	}
}

// TestDestroy_ContainerNotFound tests destroying a non-existent container.
// Should not return an error for missing containers.
func TestDestroy_ContainerNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	p := NewPodmanProvisioner()
	ctx := context.Background()

	handle := &runtime.RuntimeHandle{
		NativeID: "nonexistent-container-id-" + "abcdef123456",
	}

	// Should not error on non-existent container
	err := p.Destroy(ctx, handle)
	if err != nil {
		t.Errorf("Destroy of non-existent container returned error: %v", err)
	}
}

// TestDestroyByID tests the DestroyByID method directly.
// Should not return an error for missing containers.
func TestDestroyByID_ContainerNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	p := NewPodmanProvisioner()
	ctx := context.Background()

	// Should not error on non-existent container
	err := p.DestroyByID(ctx, "nonexistent-container-id-abcdef123456")
	if err != nil {
		t.Errorf("DestroyByID of non-existent container returned error: %v", err)
	}
}

// TestProvision_ContainerNameFormat verifies the container name format.
// NOTE: This is an integration test requiring 'podman' to be installed and running.
func TestProvision_ContainerNameFormat(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	p := NewPodmanProvisioner()
	ctx := context.Background()

	workspaceID := "test-ws-name-format"
	spec := runtime.ProvisionSpec{
		WorkspaceID:    workspaceID,
		Image:          "alpine:latest",
		WorkDir:        "/workspace",
		CacheMountPath: "",
	}

	handle, err := p.Provision(ctx, spec)
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}

	if handle == nil {
		t.Fatal("Provision returned nil handle")
	}

	// Verify container exists (if we could inspect it)
	// For now, just verify we got a non-empty ID
	if handle.NativeID == "" {
		t.Error("NativeID is empty")
	}

	// Cleanup
	if err := p.Destroy(ctx, handle); err != nil {
		t.Logf("warn: Destroy failed during cleanup: %v", err)
	}
}

// TestProvision_TransportBound verifies that the transport is properly bound.
// NOTE: This is an integration test requiring 'podman' to be installed and running.
func TestProvision_TransportBound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	p := NewPodmanProvisioner()
	ctx := context.Background()

	spec := runtime.ProvisionSpec{
		WorkspaceID: "test-transport-binding",
		Image:       "alpine:latest",
		WorkDir:     "/workspace",
	}

	handle, err := p.Provision(ctx, spec)
	if err != nil {
		t.Fatalf("Provision failed: %v", err)
	}

	if handle == nil {
		t.Fatal("Provision returned nil handle")
	}

	// Verify transport is a PodmanExecTransport
	transport, ok := handle.Transport.(*PodmanExecTransport)
	if !ok {
		t.Errorf("Transport is not PodmanExecTransport, got %T", handle.Transport)
	}

	if transport != nil && transport.containerID != handle.NativeID {
		t.Errorf("Transport containerID %q != handle NativeID %q", transport.containerID, handle.NativeID)
	}

	// Cleanup
	if err := p.Destroy(ctx, handle); err != nil {
		t.Logf("warn: Destroy failed during cleanup: %v", err)
	}
}
