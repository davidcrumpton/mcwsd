package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
)

// KubectlExecTransport implements runtime.ExecTransport for a single,
// already-running pod.
type KubectlExecTransport struct {
	kubectlBinary string
	kubeconfig    string
	namespace     string
	podName       string
}

func NewKubectlExecTransport(kubectlBinary, kubeconfig, namespace, podName string) *KubectlExecTransport {
	return &KubectlExecTransport{
		kubectlBinary: kubectlBinary,
		kubeconfig:    kubeconfig,
		namespace:     namespace,
		podName:       podName,
	}
}

var _ runtime.ExecTransport = (*KubectlExecTransport)(nil)

// buildRemoteCommand mirrors the OpenBSD transport's helper of the same
// name: env assignments, then cd, then the command, all interpreted by the
// container's shell rather than the local one.
func buildRemoteCommand(cmd []string, workDir string, env []string) string {
	parts := make([]string, 0, len(cmd))
	for _, arg := range cmd {
		parts = append(parts, quoteArg(arg))
	}
	cmdStr := strings.Join(parts, " ")
	if cmdStr == "" {
		cmdStr = ":"
	}
	if len(env) > 0 {
		assignments := make([]string, 0, len(env))
		for _, assignment := range env {
			assignments = append(assignments, quoteArg(assignment))
		}
		cmdStr = "env " + strings.Join(assignments, " ") + " " + cmdStr
	}
	if workDir != "" {
		cmdStr = "cd " + quoteArg(workDir) + " && " + cmdStr
	}
	return cmdStr
}

func quoteArg(arg string) string {
	if arg == "" {
		return "''"
	}
	if !strings.ContainsAny(arg, " \t\n\"'\\$&|;<>()*?[]{}!#~`") {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

// runKubectlExec runs `kubectl exec <pod> -- sh -c '<payload>'`, optionally
// streaming stdin (used by CopyDirIn/WriteFile). A non-zero exit from the
// payload is reported in the result, not as a Go error — same contract as
// SSHExecTransport.runSSH.
func (t *KubectlExecTransport) runKubectlExec(ctx context.Context, payload string, stdin io.Reader, shellArgs ...string) (*internal.ExecResult, error) {
	args := []string{
		"--kubeconfig", t.kubeconfig,
		"-n", t.namespace,
		"exec",
	}
	if stdin != nil {
		args = append(args, "-i")
	}
	args = append(args, t.podName, "--", "sh", "-c", payload)
	args = append(args, shellArgs...)

	cmd := exec.CommandContext(ctx, t.kubectlBinary, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	durationMs := time.Since(start).Milliseconds()
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, err // kubectl itself failed to run (binary missing, context cancelled, pod gone)
		}
	}

	return &internal.ExecResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		ExitCode:   exitCode,
		DurationMs: durationMs,
	}, nil
}

// Exec runs a command in the pod.
func (t *KubectlExecTransport) Exec(ctx context.Context, cmd []string, workDir string, env []string) (*internal.ExecResult, error) {
	start := time.Now()
	internal.Log.Debug("transport_exec_start", "transport", "kubernetes",
		"namespace", t.namespace, "pod", t.podName, "command", fmt.Sprintf("%q", cmd),
		"work_dir", workDir, "env_count", len(env))
	result, err := t.runKubectlExec(ctx, buildRemoteCommand(cmd, workDir, env), nil)
	durationMs := time.Since(start).Milliseconds()
	if err != nil {
		internal.Log.Debug("transport_exec_failed", "transport", "kubernetes",
			"namespace", t.namespace, "pod", t.podName, "duration_ms", durationMs, "error", err)
		return nil, err
	}
	internal.Log.Debug("transport_exec_complete", "transport", "kubernetes",
		"namespace", t.namespace, "pod", t.podName, "exit_code", result.ExitCode, "duration_ms", durationMs)
	return result, nil
}

// SetNetwork applies or removes a deny-all NetworkPolicy selecting this pod.
// Enforcement requires a NetworkPolicy-capable CNI; policies are additive,
// so another policy allowing traffic to the pod can still permit it.
func (t *KubectlExecTransport) SetNetwork(ctx context.Context, enabled bool) error {
	policyName := "wsd-net-" + workspaceKey(t.podName)
	if enabled {
		_, err := t.runKubectlCommand(ctx, nil, "delete", "networkpolicy", policyName, "--ignore-not-found")
		if err != nil {
			return fmt.Errorf("failed to enable network for pod %s: %w", t.podName, err)
		}
		return nil
	}
	manifest := buildNetworkPolicyManifest(policyName, t.podName, t.namespace)
	if _, err := t.runKubectlCommand(ctx, []byte(manifest), "apply", "-f", "-"); err != nil {
		return fmt.Errorf("failed to disable network for pod %s: %w", t.podName, err)
	}
	return nil
}

func (t *KubectlExecTransport) runKubectlCommand(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	fullArgs := append([]string{"--kubeconfig", t.kubeconfig, "-n", t.namespace}, args...)
	cmd := exec.CommandContext(ctx, t.kubectlBinary, fullArgs...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func buildNetworkPolicyManifest(name, podName, namespace string) string {
	metadata := map[string]interface{}{
		"name":   name,
		"labels": map[string]string{"app": "mcwsd"},
	}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	manifest := map[string]interface{}{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata":   metadata,
		"spec": map[string]interface{}{
			"podSelector": map[string]interface{}{
				"matchLabels": map[string]string{
					"app":                    "mcwsd",
					"mcwsd.io/workspace-key": workspaceKey(podName),
				},
			},
			"policyTypes": []string{"Ingress", "Egress"},
			"ingress":     []interface{}{},
			"egress":      []interface{}{},
		},
	}
	data, _ := json.Marshal(manifest)
	return string(data)
}

// CopyDirIn tars hostSrcDir locally and streams it into the pod via
// `kubectl exec -i ... tar -xf -`, the same trick as the SSH transport
// (and, not coincidentally, what `kubectl cp` does under the hood — this
// just avoids shelling to a second subcommand with its own flag surface).
func (t *KubectlExecTransport) CopyDirIn(ctx context.Context, hostSrcDir, destPath string) error {
	result, err := t.Exec(ctx, []string{"mkdir", "-p", destPath}, "", nil)
	if err != nil {
		return fmt.Errorf("failed to create %s in pod %s: %w", destPath, t.podName, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("failed to create %s in pod %s (exit %d): %s", destPath, t.podName, result.ExitCode, strings.TrimSpace(result.Stderr))
	}

	tmpTar, err := createTarArchive(hostSrcDir)
	if err != nil {
		return fmt.Errorf("failed to create tar archive of %s: %w", hostSrcDir, err)
	}
	defer os.Remove(tmpTar)

	tarFile, err := os.Open(tmpTar)
	if err != nil {
		return fmt.Errorf("failed to open tar archive: %w", err)
	}
	defer tarFile.Close()

	payload := `tar -xf - -C "$1"`
	res, err := t.runKubectlExec(ctx, payload, tarFile, "sh", destPath)
	if err != nil {
		return fmt.Errorf("failed to stream tar into pod %s: %w", t.podName, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("tar extraction in pod %s failed (exit %d): %s", t.podName, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// ReadFile reads a single file out of the pod.
func (t *KubectlExecTransport) ReadFile(ctx context.Context, path string) ([]byte, error) {
	res, err := t.Exec(ctx, []string{"cat", path}, "", nil)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("failed to read %s in pod %s (exit %d): %s", path, t.podName, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return []byte(res.Stdout), nil
}

// WriteFile streams content to path via stdin, same reasoning as the SSH
// transport: avoids interpolating file contents into a shell command.
func (t *KubectlExecTransport) WriteFile(ctx context.Context, path string, content []byte, mode int64) error {
	result, err := t.Exec(ctx, []string{"mkdir", "-p", filepath.Dir(path)}, "", nil)
	if err != nil {
		return fmt.Errorf("failed to create parent dir for %s in pod %s: %w", path, t.podName, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("failed to create parent dir for %s in pod %s (exit %d): %s", path, t.podName, result.ExitCode, strings.TrimSpace(result.Stderr))
	}

	payload := `cat > "$1"`
	res, err := t.runKubectlExec(ctx, payload, bytes.NewReader(content), "sh", path)
	if err != nil {
		return fmt.Errorf("failed to write %s in pod %s: %w", path, t.podName, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("failed to write %s in pod %s (exit %d): %s", path, t.podName, res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	result, err = t.Exec(ctx, []string{"chmod", fmt.Sprintf("%o", mode), path}, "", nil)
	if err != nil {
		return fmt.Errorf("failed to chmod %s in pod %s: %w", path, t.podName, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("failed to chmod %s in pod %s (exit %d): %s", path, t.podName, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// createTarArchive is identical to the OpenBSD transport's helper of the
// same name — worth hoisting into a shared internal package once a second
// provisioner needs it, rather than copy-pasted a third time.
func createTarArchive(srcDir string) (string, error) {
	tmpFile, err := os.CreateTemp("", "wsd-k8s-copy-*.tar")
	if err != nil {
		return "", err
	}
	tmpFile.Close()

	cmd := exec.Command("tar", "-cf", tmpFile.Name(), "-C", srcDir, ".")
	if err := cmd.Run(); err != nil {
		os.Remove(tmpFile.Name())
		return "", err
	}
	return tmpFile.Name(), nil
}
