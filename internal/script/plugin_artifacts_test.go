package script

import (
	"os"
	"path/filepath"
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
	argv, err := tr.execArgs([]string{"true"}, "/w", nil, false, false)
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
	argv, err := tr.execArgs([]string{"true"}, "/w", nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"docker", "exec", "--workdir", "/w", "abc", "true"}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
}
