package main

import (
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestCurrentUserHomeDirIgnoresHOMEEnvironment(t *testing.T) {
	expectedUser, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}

	t.Setenv("HOME", "/")
	homeDir, err := currentUserHomeDir()
	if err != nil {
		t.Fatalf("currentUserHomeDir: %v", err)
	}
	if got, want := filepath.Join(homeDir, "audit.jsonl"), filepath.Join(expectedUser.HomeDir, "audit.jsonl"); got != want {
		t.Fatalf("default audit path = %q, want %q", got, want)
	}
}

func TestResolvePerRuntimePathExplicitWins(t *testing.T) {
	got, err := resolvePerRuntimePath("/custom/state.json", "/base", "docker", ".state.json")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/custom/state.json" {
		t.Fatalf("got %q, want explicit path", got)
	}
}

func TestResolvePerRuntimePathDerivesPerPlugin(t *testing.T) {
	base := t.TempDir()
	got, err := resolvePerRuntimePath("", base, "podman", ".state.json")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(base, "podman.state.json") {
		t.Fatalf("got %q", got)
	}
}

func TestResolvePerRuntimePathRequiresPlugin(t *testing.T) {
	if _, err := resolvePerRuntimePath("", "/base", "", ".state.json"); err == nil {
		t.Fatal("expected error when no plugin name is available")
	} else if !strings.Contains(err.Error(), "no runtime plugin loaded") {
		t.Fatalf("error = %v, want 'no runtime plugin loaded'", err)
	}
}

func TestResolvePerRuntimePathRejectsUnsafeName(t *testing.T) {
	for _, name := range []string{"../x", "a/b", "a\\b", "..", "."} {
		if _, err := safePluginName(name); err == nil {
			t.Errorf("safePluginName(%q) = nil, want error", name)
		}
	}
}
