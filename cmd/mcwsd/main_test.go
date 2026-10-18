package main

import (
	"context"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal/backend/freebsd"
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

func TestBuildProvisionerWiresTemplate(t *testing.T) {
	p, err := buildProvisioner("jail", "organization.tgz", "", "", "shared", "", "", 0, "", "",
		"", "", "", "", "", 22, "", "", "", 0, 0,
		freebsd.TemplateOptions{BaseURL: "https://example.invalid/templates", VerifySignature: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Init(context.Background()); err == nil {
		t.Fatal("template signature settings did not reach the provisioner")
	}
}
