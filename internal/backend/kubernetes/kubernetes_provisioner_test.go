package kubernetes

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal/runtime"
)

func TestKubernetesProvisionerInterfaceAndName(t *testing.T) {
	p := NewKubernetesProvisioner("/tmp/kube config", "workspaces")
	var _ runtime.Provisioner = p
	if p.Name() != "kubernetes" {
		t.Fatalf("Name() = %q, want kubernetes", p.Name())
	}
	if p.kubectlBinary != "kubectl" || p.kubeconfig != "/tmp/kube config" || p.namespace != "workspaces" {
		t.Fatalf("unexpected provisioner configuration: %#v", p)
	}
	if got := NewKubernetesProvisioner("", "").namespace; got != "default" {
		t.Fatalf("empty namespace default = %q, want default", got)
	}
}

func TestInitAppliesConfiguredNamespace(t *testing.T) {
	fake := newFakeKubectl(t)
	p := NewKubernetesProvisioner("config", "team-workspaces")
	p.kubectlBinary = fake.binary

	for range 2 {
		if err := p.Init(context.Background()); err != nil {
			t.Fatalf("Init: %v", err)
		}
	}

	data, err := os.ReadFile(fake.manifest)
	if err != nil {
		t.Fatalf("read namespace manifest: %v", err)
	}
	var manifest struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("namespace manifest is not valid JSON: %v\n%s", err, data)
	}
	if manifest.APIVersion != "v1" || manifest.Kind != "Namespace" || manifest.Metadata.Name != p.namespace {
		t.Fatalf("unexpected namespace manifest: %#v", manifest)
	}

	args, err := os.ReadFile(fake.argsLog)
	if err != nil {
		t.Fatalf("read kubectl arguments: %v", err)
	}
	if got := strings.Count(string(args), "\napply\n"); got != 2 {
		t.Fatalf("Init applied namespace %d times, want 2; args:\n%s", got, args)
	}
}

func TestProvisionBuildsPodAndReturnsBoundTransport(t *testing.T) {
	fake := newFakeKubectl(t)
	p := NewKubernetesProvisioner("/tmp/kube config", "team namespace")
	p.kubectlBinary = fake.binary
	p.defaultImage = "alpine:latest"
	workDir := `/workspace/a "quoted"`

	handle, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID: "ws-123",
		WorkDir:     workDir,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if handle.NativeID != "wsd-ws-123" || handle.WorkDir != workDir {
		t.Fatalf("unexpected handle identity/workdir: %#v", handle)
	}
	if handle.OS != runtime.OSLinux || handle.Isolation != runtime.IsolationContainer || handle.Privilege != runtime.PrivilegeRoot {
		t.Fatalf("unexpected runtime metadata: %#v", handle)
	}
	transport, ok := handle.Transport.(*KubectlExecTransport)
	if !ok || transport.podName != handle.NativeID || transport.namespace != p.namespace {
		t.Fatalf("transport is not bound to provisioned pod: %#v", handle.Transport)
	}

	data, err := os.ReadFile(fake.manifest)
	if err != nil {
		t.Fatalf("read applied manifest: %v", err)
	}
	var manifest struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name      string            `json:"name"`
			Namespace string            `json:"namespace"`
			Labels    map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			RestartPolicy string `json:"restartPolicy"`
			Containers    []struct {
				Image      string   `json:"image"`
				WorkingDir string   `json:"workingDir"`
				Command    []string `json:"command"`
			} `json:"containers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("pod manifest is not valid JSON: %v\n%s", err, data)
	}
	if manifest.APIVersion != "v1" || manifest.Kind != "Pod" || manifest.Metadata.Name != handle.NativeID || manifest.Metadata.Namespace != p.namespace {
		t.Fatalf("unexpected manifest metadata: %#v", manifest)
	}
	if manifest.Metadata.Labels["app"] != "wsd" || manifest.Metadata.Labels["wsd.io/workspace-key"] != workspaceKey(handle.NativeID) {
		t.Fatalf("missing workspace selector labels: %#v", manifest.Metadata.Labels)
	}
	if manifest.Spec.RestartPolicy != "Never" || len(manifest.Spec.Containers) != 1 {
		t.Fatalf("unexpected pod spec: %#v", manifest.Spec)
	}
	container := manifest.Spec.Containers[0]
	if container.Image != p.defaultImage || container.WorkingDir != workDir || strings.Join(container.Command, " ") != "sleep infinity" {
		t.Fatalf("unexpected workspace container: %#v", container)
	}
}

func TestProvisionRejectsMissingWorkspaceIDOrImage(t *testing.T) {
	p := NewKubernetesProvisioner("", "default")
	for _, spec := range []runtime.ProvisionSpec{
		{Image: "alpine"},
		{WorkspaceID: "ws-123"},
	} {
		if _, err := p.Provision(context.Background(), spec); err == nil {
			t.Errorf("Provision(%#v) succeeded without required identity/image", spec)
		}
	}
}

func TestRecoverHandleAndDestroy(t *testing.T) {
	fake := newFakeKubectl(t)
	p := NewKubernetesProvisioner("config", "default")
	p.kubectlBinary = fake.binary

	handle, err := p.RecoverHandle(context.Background(), "wsd-existing", "/workspace/repo")
	if err != nil {
		t.Fatalf("RecoverHandle: %v", err)
	}
	if handle.NativeID != "wsd-existing" || handle.WorkDir != "/workspace/repo" {
		t.Fatalf("unexpected recovered handle: %#v", handle)
	}
	if err := p.Destroy(context.Background(), handle); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if err := p.DestroyByID(context.Background(), "wsd-leftover"); err != nil {
		t.Fatalf("DestroyByID: %v", err)
	}
	args, err := os.ReadFile(fake.argsLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"get\npod\nwsd-existing", "delete\npod\nwsd-existing", "delete\npod\nwsd-leftover", "--ignore-not-found"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("kubectl arguments missing %q:\n%s", want, args)
		}
	}
}
