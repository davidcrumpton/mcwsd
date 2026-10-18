package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFreeBSDRCOptions(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("POSIX shell is unavailable")
	}
	path := filepath.Join("..", "..", "examples", "freebsd", "rc.d", "mcwsd")
	if out, err := exec.Command(shell, "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("rc script syntax: %v\n%s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Stub only FreeBSD's rc.subr entry points; execute the real argument logic.
	script := strings.Replace(string(data), ". /etc/rc.subr", `
load_rc_config() { :; }
checkyesno() {
    case "$1" in
        mcwsd_verify_signature)
            case "$mcwsd_verify_signature" in YES|yes|TRUE|true|1) return 0;; esac ;;
    esac
    return 1
}
run_rc_command() { printf '%s\n' "$command_args"; }
`, 1)
	for _, tc := range []struct {
		name    string
		runtime string
		auth    string
		enabled bool
	}{
		{"official release", "jail", "", false},
		{"anonymous signed template", "jail", "", true},
		{"Basic authentication", "freebsd", "basic", true},
		{"header authentication", "jail", "header", true},
		{"non-jail ignores template settings", "docker", "header", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("mcwsd_source_env_file", filepath.Join(t.TempDir(), "absent-env"))
			t.Setenv("mcwsd_runtime", tc.runtime)
			for _, name := range []string{"base_template_url", "base_template_user", "base_template_password_env", "base_template_header", "base_template_header_env", "template_public_key"} {
				t.Setenv("mcwsd_"+name, "")
			}
			t.Setenv("mcwsd_verify_signature", "NO")
			t.Setenv("mcwsd_freebsd_release", "15.1-RELEASE")
			if tc.enabled {
				t.Setenv("mcwsd_base_template_url", "https://templates.example.org/freebsd")
				t.Setenv("mcwsd_freebsd_release", "organization.tgz")
				t.Setenv("mcwsd_verify_signature", "YES")
				t.Setenv("mcwsd_template_public_key", "/etc/wsd/template.pub")
			}
			t.Setenv("MCWSD_TEMPLATE_PASSWORD", "test-password")
			t.Setenv("MCWSD_TEMPLATE_AUTH", "Bearer test-header-secret")
			switch tc.auth {
			case "basic":
				t.Setenv("mcwsd_base_template_user", "mcwsd")
				t.Setenv("mcwsd_base_template_password_env", "MCWSD_TEMPLATE_PASSWORD")
			case "header":
				t.Setenv("mcwsd_base_template_header", "Authorization")
				t.Setenv("mcwsd_base_template_header_env", "MCWSD_TEMPLATE_AUTH")
			}
			cmd := exec.Command(shell, "-s", "--", "start")
			cmd.Stdin = strings.NewReader(script)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("execute rc argument logic: %v\n%s", err, out)
			}
			args := strings.Fields(string(out))
			has := func(want string) bool {
				for _, arg := range args {
					if arg == want {
						return true
					}
				}
				return false
			}
			jailTemplate := tc.enabled && tc.runtime != "docker"
			for _, arg := range []string{
				"--base-template-url=https://templates.example.org/freebsd",
				"--verify-signature=true",
				"--template-public-key=/etc/wsd/template.pub",
			} {
				if has(arg) != jailTemplate {
					t.Errorf("presence of %s = %t, want %t", arg, has(arg), jailTemplate)
				}
			}
			for arg, want := range map[string]bool{
				"--base-template-user=mcwsd":                           jailTemplate && tc.auth == "basic",
				"--base-template-password-env=MCWSD_TEMPLATE_PASSWORD": jailTemplate && tc.auth == "basic",
				"--base-template-header=Authorization":                 jailTemplate && tc.auth == "header",
				"--base-template-header-env=MCWSD_TEMPLATE_AUTH":       jailTemplate && tc.auth == "header",
			} {
				if has(arg) != want {
					t.Errorf("presence of %s = %t, want %t", arg, has(arg), want)
				}
			}
			if jailTemplate && !has("--freebsd-release=organization.tgz") {
				t.Error("template filename was not forwarded")
			}
			if !tc.enabled && !has("--freebsd-release=15.1-RELEASE") {
				t.Error("official release default was not preserved")
			}
			for _, secret := range []string{"test-password", "test-header-secret"} {
				if strings.Contains(string(out), secret) {
					t.Error("credential value was placed in daemon arguments")
				}
			}
		})
	}
}
