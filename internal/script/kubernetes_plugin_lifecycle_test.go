package script

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal/runtime"
	"github.com/spf13/pflag"
)

// TestKubernetesPluginLifecycle drives the deployed kubernetes.js through a
// fake kubectl to confirm the goja-hosted lifecycle path (JSON manifest,
// host.run with stdin, the poll loop) works end-to-end, not just the pure
// execArgs mapping covered elsewhere.
func TestKubernetesPluginLifecycle(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "kubectl")
	manifestPath := filepath.Join(dir, "manifest.json")
	fake := `#!/bin/sh
case "$*" in
  *"jsonpath={.status.phase}"*) printf 'Running' ;;
  *"apply"*) cat > "$KUBECTL_MANIFEST" ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECTL_MANIFEST", manifestPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	r := deployedPlugin(t, "kubernetes")
	fs := pflag.NewFlagSet("d", pflag.ContinueOnError)
	vals, err := BindFlags(fs, r.Options)
	if err != nil {
		t.Fatal(err)
	}
	p := r.NewProvisioner(vals)
	h, err := p.Provision(context.Background(), runtime.ProvisionSpec{
		WorkspaceID: "ws-1", Image: "alpine", WorkDir: "/w",
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if h.NativeID != "wsd-ws-1" || h.WorkDir != "/w" {
		t.Fatalf("handle = %#v", h)
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read applied manifest: %v", err)
	}
	var manifest struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Containers []struct {
				Image string `json:"image"`
			} `json:"containers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("manifest is not valid JSON: %v\n%s", err, data)
	}
	if manifest.Kind != "Pod" || manifest.Metadata.Name != "wsd-ws-1" || len(manifest.Spec.Containers) != 1 || manifest.Spec.Containers[0].Image != "alpine" {
		t.Fatalf("unexpected manifest: %#v", manifest)
	}

	if err := p.Destroy(context.Background(), h); err != nil {
		t.Fatalf("destroy: %v", err)
	}
}
