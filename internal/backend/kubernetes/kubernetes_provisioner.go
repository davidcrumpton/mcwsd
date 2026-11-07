package kubernetes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
)

func debugf(format string, args ...interface{}) {
	internal.Log.Debug(fmt.Sprintf(format, args...))
}

// KubernetesProvisioner implements runtime.Provisioner by creating one Pod
// per workspace and driving it entirely through the kubectl binary. Like
// the SSH transport, it shells out rather than linking client-go, so the
// only dependencies are a kubectl binary on PATH and a kubeconfig path —
// no in-process API client to version alongside the cluster.
type KubernetesProvisioner struct {
	kubectlBinary string
	kubeconfig    string
	namespace     string
	defaultImage  string // used if spec.Image is empty; optional
}

func NewKubernetesProvisioner(kubeconfig, namespace string) *KubernetesProvisioner {
	if namespace == "" {
		namespace = "default"
	}
	return &KubernetesProvisioner{
		kubectlBinary: "kubectl",
		kubeconfig:    kubeconfig,
		namespace:     namespace,
	}
}

var _ runtime.Provisioner = (*KubernetesProvisioner)(nil)

func (p *KubernetesProvisioner) Name() string { return "kubernetes" }

// Init creates the configured namespace if it does not already exist.
func (p *KubernetesProvisioner) Init(ctx context.Context) error {
	manifest, err := json.Marshal(struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}{
		APIVersion: "v1",
		Kind:       "Namespace",
		Metadata: struct {
			Name string `json:"name"`
		}{Name: p.namespace},
	})
	if err != nil {
		return fmt.Errorf("failed to build namespace manifest: %w", err)
	}
	if _, err := p.runKubectl(ctx, manifest, "apply", "-f", "-"); err != nil {
		return fmt.Errorf("failed to initialize namespace %s: %w", p.namespace, err)
	}
	return nil
}

func podName(workspaceID string) string { return fmt.Sprintf("wsd-%s", workspaceID) }

// runKubectl runs kubectl with the shared --kubeconfig/-n flags prepended.
// Mirrors runSSH in the OpenBSD transport: a local failure (binary missing,
// context cancelled) is an error; kubectl's own stderr is folded into the
// error message so a bad manifest or RBAC denial is visible immediately.
func (p *KubernetesProvisioner) runKubectl(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	full := append([]string{"--kubeconfig", p.kubeconfig, "-n", p.namespace}, args...)
	debugf("kubectl %s", strings.Join(full, " "))

	cmd := exec.CommandContext(ctx, p.kubectlBinary, full...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Provision applies a minimal single-container Pod manifest — entrypoint
// `sleep infinity`, same reason the Docker provisioner uses it: something
// has to keep PID 1 alive for exec to attach to later — and waits for the
// pod to reach Running, the k8s analog of waiting for a container to start.
func (p *KubernetesProvisioner) Provision(ctx context.Context, spec runtime.ProvisionSpec) (*runtime.RuntimeHandle, error) {
	if spec.WorkspaceID == "" {
		return nil, fmt.Errorf("workspace ID must not be empty")
	}
	name := podName(spec.WorkspaceID)
	image := spec.Image
	if image == "" {
		image = p.defaultImage
	}
	if image == "" {
		return nil, fmt.Errorf("image must be set in the provision spec or provisioner defaults")
	}

	debugf("provisioning pod %s: image=%s workDir=%s", name, image, spec.WorkDir)

	manifest := buildPodManifest(name, image, spec.WorkDir, p.namespace)
	if _, err := p.runKubectl(ctx, []byte(manifest), "apply", "-f", "-"); err != nil {
		return nil, fmt.Errorf("failed to create pod %s: %w", name, err)
	}

	if err := p.waitForRunning(ctx, name, 2*time.Minute); err != nil {
		debugf("pod %s never became ready, cleaning up: %v", name, err)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.DestroyByID(cleanupCtx, name)
		return nil, fmt.Errorf("pod %s did not become ready: %w", name, err)
	}
	debugf("pod %s is running", name)

	return &runtime.RuntimeHandle{
		OS:        runtime.OSLinux,
		Isolation: runtime.IsolationContainer,
		Username:  "root", // the image's default user; set securityContext.runAsNonRoot in the manifest if you want otherwise
		Privilege: runtime.PrivilegeRoot,
		WorkDir:   spec.WorkDir,
		NativeID:  name,
		Transport: NewKubectlExecTransport(p.kubectlBinary, p.kubeconfig, p.namespace, name),
	}, nil
}

// waitForRunning polls pod phase the same way the OpenBSD provisioner polls
// ARP/SSH readiness — there's no blocking "create and wait" verb in kubectl
// itself (short of `kubectl wait`, which this could use instead; polling
// `-o jsonpath` is shown here since it matches your existing poll-loop style
// more closely).
func (p *KubernetesProvisioner) waitForRunning(ctx context.Context, name string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		out, err := p.runKubectl(waitCtx, nil, "get", "pod", name, "-o", "jsonpath={.status.phase}")
		if err == nil && strings.TrimSpace(string(out)) == "Running" {
			return nil
		}
		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("timed out waiting for pod %s to become Running", name)
		case <-ticker.C:
		}
	}
}

// Destroy deletes the pod behind handle.
func (p *KubernetesProvisioner) Destroy(ctx context.Context, handle *runtime.RuntimeHandle) error {
	if handle == nil {
		return fmt.Errorf("runtime handle must not be nil")
	}
	return p.DestroyByID(ctx, handle.NativeID)
}

// DestroyByID deletes a pod by name directly — the leftover-cleanup path
// across a daemon restart, same role as the Docker provisioner's
// DestroyByID.
func (p *KubernetesProvisioner) DestroyByID(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("pod name must not be empty")
	}
	debugf("deleting pod %s", name)
	_, err := p.runKubectl(ctx, nil, "delete", "pod", name, "--grace-period=5", "--ignore-not-found")
	if err != nil {
		return fmt.Errorf("failed to delete pod %s: %w", name, err)
	}
	debugf("pod %s deleted", name)
	return nil
}

// RecoverHandle re-attaches to a pod that was already created by a previous
// daemon run, the same role as DockerProvisioner.RecoverHandle.
func (p *KubernetesProvisioner) RecoverHandle(ctx context.Context, nativeID string, workDir string) (*runtime.RuntimeHandle, error) {
	if nativeID == "" {
		return nil, fmt.Errorf("pod name must not be empty")
	}
	if _, err := p.runKubectl(ctx, nil, "get", "pod", nativeID, "-o", "name"); err != nil {
		return nil, fmt.Errorf("pod %s not found: %w", nativeID, err)
	}
	return &runtime.RuntimeHandle{
		OS:        runtime.OSLinux,
		Isolation: runtime.IsolationContainer,
		Username:  "root",
		Privilege: runtime.PrivilegeRoot,
		WorkDir:   workDir,
		NativeID:  nativeID,
		Transport: NewKubectlExecTransport(p.kubectlBinary, p.kubeconfig, p.namespace, nativeID),
	}, nil
}

// buildPodManifest renders a minimal single-container pod as JSON, which is
// accepted by kubectl and safely escapes values supplied by workspace specs.
func buildPodManifest(name, image, workDir, namespace string) string {
	manifest := struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name      string            `json:"name"`
			Namespace string            `json:"namespace,omitempty"`
			Labels    map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			RestartPolicy string `json:"restartPolicy"`
			Containers    []struct {
				Name       string   `json:"name"`
				Image      string   `json:"image"`
				WorkingDir string   `json:"workingDir,omitempty"`
				Command    []string `json:"command"`
			} `json:"containers"`
		} `json:"spec"`
	}{}
	manifest.APIVersion = "v1"
	manifest.Kind = "Pod"
	manifest.Metadata.Name = name
	manifest.Metadata.Namespace = namespace
	manifest.Metadata.Labels = map[string]string{
		"app":                  "wsd",
		"wsd.io/workspace-key": workspaceKey(name),
	}
	manifest.Spec.RestartPolicy = "Never"
	manifest.Spec.Containers = append(manifest.Spec.Containers, struct {
		Name       string   `json:"name"`
		Image      string   `json:"image"`
		WorkingDir string   `json:"workingDir,omitempty"`
		Command    []string `json:"command"`
	}{Name: "workspace", Image: image, WorkingDir: workDir, Command: []string{"sleep", "infinity"}})
	data, _ := json.Marshal(manifest)
	return string(data)
}

func workspaceKey(name string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(name)))[:32]
}
