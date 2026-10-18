package docker

import (
	"testing"

	"github.com/davidcrumpton/mcwsd/internal/runtime"
)

func TestDockerExecTransport_ImplementsBootstrapper(t *testing.T) {
	transport := NewDockerExecTransport(nil, "test-container", "test-network")
	var _ runtime.Bootstrapper = transport
}

// TestDockerBootstrapMarksRepoOwnershipSafe guards the fix for git refusing to
// operate on the bind-mounted workspace repo ("detected dubious ownership"):
// the bootstrap must whitelist the repo path via safe.directory.
func TestDockerBootstrapMarksRepoOwnershipSafe(t *testing.T) {
	if len(dockerBootstrapArgs) < 2 || dockerBootstrapArgs[0] != "git" || dockerBootstrapArgs[1] != "config" {
		t.Fatalf("bootstrap must invoke git config: %v", dockerBootstrapArgs)
	}
	found := false
	for i, arg := range dockerBootstrapArgs {
		if arg == "safe.directory" && i+1 < len(dockerBootstrapArgs) && dockerBootstrapArgs[i+1] == "*" {
			found = true
		}
	}
	if !found {
		t.Errorf("bootstrap must add safe.directory '*': %v", dockerBootstrapArgs)
	}
}
