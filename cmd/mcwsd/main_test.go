package main

import (
	"os/user"
	"path/filepath"
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
