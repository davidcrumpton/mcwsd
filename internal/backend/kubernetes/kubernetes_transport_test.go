package kubernetes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal/runtime"
)

func TestKubectlExecTransportImplementsInterface(t *testing.T) {
	var _ runtime.ExecTransport = NewKubectlExecTransport("kubectl", "config", "default", "wsd-ws")
}

func TestBuildRemoteCommandQuotesArguments(t *testing.T) {
	got := buildRemoteCommand([]string{"printf", "%s", "value with spaces; touch /tmp/nope"}, "/workspace/repo with spaces", []string{"VALUE=two words"})
	want := "cd '/workspace/repo with spaces' && env 'VALUE=two words' printf %s 'value with spaces; touch /tmp/nope'"
	if got != want {
		t.Fatalf("buildRemoteCommand() = %q, want %q", got, want)
	}
	if got := buildRemoteCommand(nil, "", nil); got != ":" {
		t.Fatalf("empty command = %q, want ':'", got)
	}
}

func TestExecCapturesOutputAndExitCode(t *testing.T) {
	fake := newFakeKubectl(t)
	transport := NewKubectlExecTransport(fake.binary, "config", "dev", "wsd-test")

	result, err := transport.Exec(context.Background(), []string{"printf", "%s", "literal; touch /tmp/not-created"}, "", nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "literal; touch /tmp/not-created" || result.ExitCode != 0 {
		t.Fatalf("unexpected command result: %#v", result)
	}
	result, err = transport.Exec(context.Background(), []string{"sh", "-c", `printf %s "$VALUE"`}, "", []string{"VALUE=two words"})
	if err != nil {
		t.Fatalf("Exec with environment: %v", err)
	}
	if result.Stdout != "two words" || result.ExitCode != 0 {
		t.Fatalf("environment command result = %#v, want spaced value", result)
	}

	result, err = transport.Exec(context.Background(), []string{"sh", "-c", "printf failure >&2; exit 7"}, "", nil)
	if err != nil {
		t.Fatalf("Exec nonzero: %v", err)
	}
	if result.ExitCode != 7 || result.Stderr != "failure" {
		t.Fatalf("nonzero command result = %#v, want exit 7 and stderr", result)
	}
}

func TestSetNetworkAppliesAndDeletesPolicy(t *testing.T) {
	fake := newFakeKubectl(t)
	transport := NewKubectlExecTransport(fake.binary, "config", "team-a", "wsd-ws-123")
	if err := transport.SetNetwork(context.Background(), false); err != nil {
		t.Fatalf("SetNetwork(false): %v", err)
	}
	data, err := os.ReadFile(fake.manifest)
	if err != nil {
		t.Fatalf("read policy manifest: %v", err)
	}
	var policy struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			PodSelector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"podSelector"`
			PolicyTypes []string          `json:"policyTypes"`
			Ingress     []json.RawMessage `json:"ingress"`
			Egress      []json.RawMessage `json:"egress"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(data, &policy); err != nil {
		t.Fatalf("invalid policy JSON: %v\n%s", err, data)
	}
	if policy.APIVersion != "networking.k8s.io/v1" || policy.Kind != "NetworkPolicy" || policy.Metadata.Namespace != "team-a" {
		t.Fatalf("unexpected policy metadata: %#v", policy)
	}
	if policy.Metadata.Name != "wsd-net-"+workspaceKey(transport.podName) {
		t.Fatalf("policy name = %q", policy.Metadata.Name)
	}
	if policy.Spec.PodSelector.MatchLabels["wsd.io/workspace-key"] != workspaceKey(transport.podName) {
		t.Fatalf("policy does not select its pod: %#v", policy.Spec.PodSelector.MatchLabels)
	}
	if strings.Join(policy.Spec.PolicyTypes, ",") != "Ingress,Egress" || len(policy.Spec.Ingress) != 0 || len(policy.Spec.Egress) != 0 {
		t.Fatalf("policy is not deny-all: %#v", policy.Spec)
	}

	if err := transport.SetNetwork(context.Background(), true); err != nil {
		t.Fatalf("SetNetwork(true): %v", err)
	}
	args, err := os.ReadFile(fake.argsLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "delete\nnetworkpolicy\n"+policy.Metadata.Name+"\n--ignore-not-found") {
		t.Fatalf("policy delete command missing from kubectl args:\n%s", args)
	}
}

func TestWriteAndReadFileWithSpecialPath(t *testing.T) {
	fake := newFakeKubectl(t)
	transport := NewKubectlExecTransport(fake.binary, "config", "default", "wsd-file")
	path := filepath.Join(t.TempDir(), "file with 'quotes'.txt")
	content := []byte("line one\nline 'two'\n")
	if err := transport.WriteFile(context.Background(), path, content, 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("file content = %q, want %q", got, content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("file mode = %o, want 640", info.Mode().Perm())
	}
	readBack, err := transport.ReadFile(context.Background(), path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(readBack) != string(content) {
		t.Fatalf("ReadFile = %q, want %q", readBack, content)
	}
}

func TestCopyDirInStreamsArchive(t *testing.T) {
	fake := newFakeKubectl(t)
	transport := NewKubectlExecTransport(fake.binary, "config", "default", "wsd-copy")
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "hello.txt"), []byte("copied"), 0o644); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "destination with spaces")
	if err := transport.CopyDirIn(context.Background(), source, destination); err != nil {
		t.Fatalf("CopyDirIn: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "hello.txt"))
	if err != nil {
		t.Fatalf("read copied file: %v", err)
	}
	if string(got) != "copied" {
		t.Fatalf("copied content = %q, want copied", got)
	}
}
