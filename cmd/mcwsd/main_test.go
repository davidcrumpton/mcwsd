package main

import (
	"context"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal/backend/freebsd"
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

func TestDaemonTemplateFlags(t *testing.T) {
	cmd := newDaemonCmd()
	for _, name := range []string{
		"base-template-url", "base-template-user", "base-template-password-env",
		"base-template-header", "base-template-header-env", "verify-signature", "template-public-key",
	} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("missing daemon flag %s", name)
		}
	}
	if got := cmd.Flags().Lookup("verify-signature").DefValue; got != "false" {
		t.Errorf("signature verification default = %q", got)
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
	p, err := buildProvisioner("zones", "", "", "", "", "", "", 0, "", "",
		zones.Options{ZonePath: "/rpool1/zones"}, freebsd.TemplateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "zones" {
		t.Fatalf("provisioner name = %q, want zones", p.Name())
	}
}

func TestBuildProvisionerWiresTemplate(t *testing.T) {
	p, err := buildProvisioner("jail", "organization.tgz", "", "", "shared", "", "", 0, "", "",
		zones.Options{},
		freebsd.TemplateOptions{BaseURL: "https://example.invalid/templates", VerifySignature: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Init(context.Background()); err == nil {
		t.Fatal("template signature settings did not reach the provisioner")
	}
}
