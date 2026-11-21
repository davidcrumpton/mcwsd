package main

import (
	"bytes"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal"
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

func TestTokenCmdGeneratesDistinctWSDTokens(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 2; i++ {
		var out bytes.Buffer
		cmd := newTokenCmd()
		cmd.SetOut(&out)
		cmd.SetArgs(nil)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("token command: %v", err)
		}
		token := strings.TrimSpace(out.String())
		if !strings.HasPrefix(token, "wsd-") {
			t.Fatalf("token = %q, want wsd- prefix", token)
		}
		if got, want := len(strings.TrimPrefix(token, "wsd-")), 64; got != want {
			t.Fatalf("token entropy length = %d, want %d", got, want)
		}
		if seen[token] {
			t.Fatalf("token command returned duplicate token %q", token)
		}
		seen[token] = true
	}
}

func TestTokenCmdRejectsArguments(t *testing.T) {
	cmd := newTokenCmd()
	cmd.SetArgs([]string{"extra"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for unexpected argument")
	}
}

// TestAuditFlagsDefaultRetention pins the operator-facing defaults: rotate at
// 10 MiB and keep no more than 7 rotated files.
func TestAuditFlagsDefaultRetention(t *testing.T) {
	var cfg auditFlags
	cmd := newDaemonCmd()
	registerAuditFlags(cmd, &cfg)

	want := map[string]string{
		"audit-keep":      strconv.Itoa(internal.DefaultAuditKeep),
		"audit-max-bytes": strconv.FormatInt(internal.DefaultAuditMaxBytes, 10),
	}
	for name, defValue := range want {
		flag := cmd.Flags().Lookup(name)
		if flag == nil {
			t.Fatalf("flag --%s is not registered on the daemon command", name)
		}
		if flag.DefValue != defValue {
			t.Errorf("--%s default = %q, want %q", name, flag.DefValue, defValue)
		}
	}
	if internal.DefaultAuditKeep != 7 {
		t.Errorf("internal.DefaultAuditKeep = %d, want 7", internal.DefaultAuditKeep)
	}

	if err := cmd.Flags().Parse([]string{
		"--audit", "--audit-file", "/tmp/audit.jsonl",
		"--audit-max-bytes", "2048", "--audit-keep", "2",
	}); err != nil {
		t.Fatalf("parse audit flags: %v", err)
	}
	if !cfg.enabled {
		t.Error("--audit did not enable audit logging")
	}
	if cfg.file != "/tmp/audit.jsonl" {
		t.Errorf("audit file = %q, want /tmp/audit.jsonl", cfg.file)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	opts := cfg.options()
	if opts.Path != "/tmp/audit.jsonl" || opts.MaxBytes != 2048 || opts.Keep != 2 {
		t.Errorf("options() = %+v, want path=/tmp/audit.jsonl max=2048 keep=2", opts)
	}
}

func TestAuditFlagsRejectUnboundedRetention(t *testing.T) {
	if err := (auditFlags{maxBytes: -1}).validate(); err == nil {
		t.Error("expected error for negative --audit-max-bytes")
	} else if !strings.Contains(err.Error(), "--audit-max-bytes") {
		t.Errorf("error = %v, want it to name --audit-max-bytes", err)
	}
	for _, keep := range []int{0, -1} {
		if err := (auditFlags{keep: keep}).validate(); err == nil {
			t.Errorf("expected error for --audit-keep=%d", keep)
		} else if !strings.Contains(err.Error(), "--audit-keep") {
			t.Errorf("error = %v, want it to name --audit-keep", err)
		}
	}
}
