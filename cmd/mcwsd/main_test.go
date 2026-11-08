package main

import (
	"os/user"
	"path/filepath"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal/backend/zones"
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

func TestDaemonZonesFlags(t *testing.T) {
	cmd := newDaemonCmd()
	for _, name := range []string{
		"zones-zonepath", "zones-brand", "zones-base-zone", "zones-network-mode",
		"zones-interface", "zones-ip-address", "zones-defrouter",
		"zones-ip-base-network", "zones-ip-pool-size", "zones-dns-servers",
		"zones-escalation", "zones-boot-timeout",
	} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("missing daemon flag %s", name)
		}
	}
	for flag, want := range map[string]string{
		"zones-brand":        "ipkg",
		"zones-network-mode": "shared",
		"zones-escalation":   "pfexec",
	} {
		if got := cmd.Flags().Lookup(flag).DefValue; got != want {
			t.Errorf("%s default = %q, want %q", flag, got, want)
		}
	}
}

func TestBuildProvisionerWiresZones(t *testing.T) {
	p, err := buildProvisioner("zones", zones.Options{ZonePath: "/rpool1/zones"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "zones" {
		t.Fatalf("provisioner name = %q, want zones", p.Name())
	}
}
