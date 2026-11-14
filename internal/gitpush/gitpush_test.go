package gitpush

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Config.Validate
// ---------------------------------------------------------------------------

func TestValidate_SSHMode_Valid(t *testing.T) {
	keyFile := tempFileWithContent(t, "fake-ssh-key")
	knownHostsFile := tempFileWithContent(t, "github.com ssh-rsa AAAA...")

	cfg := Config{
		AuthMode:          AuthModeSSH,
		SSHKeyPath:        keyFile,
		SSHKnownHostsPath: knownHostsFile,
		GitLabToken:       "glpat-token",
		GitLabAPIBase:     "https://gitlab.example.com/api/v4",
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate valid SSH config: %v", err)
	}
}

func TestValidate_SSHMode_MissingKeyPath_IsValid(t *testing.T) {
	cfg := Config{
		AuthMode:          AuthModeSSH,
		SSHKeyPath:        "", // missing, but optional
		SSHKnownHostsPath: tempFileWithContent(t, "hosts"),
		GitLabToken:       "token",
		GitLabAPIBase:     "https://gitlab.example.com/api/v4",
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected no error for missing SSH key path, got %v", err)
	}
}

func TestValidate_SSHMode_MissingKnownHosts_IsValid(t *testing.T) {
	cfg := Config{
		AuthMode:          AuthModeSSH,
		SSHKeyPath:        tempFileWithContent(t, "key"),
		SSHKnownHostsPath: "", // missing: fall back to the system known_hosts
		GitLabToken:       "token",
		GitLabAPIBase:     "https://gitlab.example.com/api/v4",
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected no error for missing known_hosts path, got %v", err)
	}
}

func TestValidate_SSHMode_UnreadableKey(t *testing.T) {
	cfg := Config{
		AuthMode:          AuthModeSSH,
		SSHKeyPath:        "/tmp/no-such-file-wsd-test-12345",
		SSHKnownHostsPath: tempFileWithContent(t, "hosts"),
		GitLabToken:       "token",
		GitLabAPIBase:     "https://gitlab.example.com/api/v4",
	}
	if cfg.Validate() == nil {
		t.Error("expected error for unreadable SSH key, got nil")
	}
}

func TestValidate_TokenMode_Valid(t *testing.T) {
	cfg := Config{
		AuthMode:            AuthModeToken,
		GitLabTokenUsername: "oauth2",
		GitLabToken:         "glpat-token",
		GitLabAPIBase:       "https://gitlab.example.com/api/v4",
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate valid token config: %v", err)
	}
}

func TestValidate_TokenMode_MissingUsername(t *testing.T) {
	cfg := Config{
		AuthMode:            AuthModeToken,
		GitLabTokenUsername: "",
		GitLabToken:         "glpat-token",
		GitLabAPIBase:       "https://gitlab.example.com/api/v4",
	}
	if cfg.Validate() == nil {
		t.Error("expected error for missing token username, got nil")
	}
}

func TestValidate_MissingToken(t *testing.T) {
	cfg := Config{
		AuthMode:            AuthModeToken,
		GitLabTokenUsername: "oauth2",
		GitLabToken:         "",
		GitLabAPIBase:       "https://gitlab.example.com/api/v4",
	}
	if cfg.Validate() == nil {
		t.Error("expected error for missing token, got nil")
	}
}

func TestValidate_MissingAPIBase(t *testing.T) {
	cfg := Config{
		AuthMode:            AuthModeToken,
		GitLabTokenUsername: "oauth2",
		GitLabToken:         "token",
		GitLabAPIBase:       "",
	}
	if cfg.Validate() == nil {
		t.Error("expected error for missing API base, got nil")
	}
}

func TestValidate_InvalidAuthMode(t *testing.T) {
	cfg := Config{
		AuthMode:      "magic",
		GitLabToken:   "token",
		GitLabAPIBase: "https://gitlab.example.com/api/v4",
	}
	if cfg.Validate() == nil {
		t.Error("expected error for invalid auth mode, got nil")
	}
}

// TestValidate_GitHubDefaultsAPIBase verifies that validating a GitHub config
// without an explicit API base fills in the public GitHub API on the caller's
// config. A value receiver here silently discarded the default, which produced
// a relative MR URL ("unsupported protocol scheme \"\"").
func TestValidate_GitHubDefaultsAPIBase(t *testing.T) {
	cfg := Config{
		AuthMode:            AuthModeToken,
		GitLabTokenUsername: "oauth2",
		ForgeType:           "github",
		GitHubToken:         "ghp-token",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.GitHubAPIBase != "https://api.github.com" {
		t.Errorf("GitHubAPIBase = %q, want %q", cfg.GitHubAPIBase, "https://api.github.com")
	}
}

// ---------------------------------------------------------------------------
// Config.ValidateForge
// ---------------------------------------------------------------------------

func TestValidateForge_GitHubRequiresToken(t *testing.T) {
	cfg := Config{}
	if err := cfg.ValidateForge("github"); err == nil {
		t.Error("expected error for github forge without a token")
	}
	cfg.GitHubToken = "ghp-token"
	if err := cfg.ValidateForge("github"); err != nil {
		t.Errorf("ValidateForge(github) with token: %v", err)
	}
}

func TestValidateForge_GitLabRequiresTokenAndBase(t *testing.T) {
	cfg := Config{}
	if err := cfg.ValidateForge("gitlab"); err == nil {
		t.Error("expected error for gitlab forge without a token")
	}
	cfg.GitLabToken = "glpat-token"
	if err := cfg.ValidateForge("gitlab"); err == nil {
		t.Error("expected error for gitlab forge without an API base")
	}
	cfg.GitLabAPIBase = "https://gitlab.example.com/api/v4"
	if err := cfg.ValidateForge("gitlab"); err != nil {
		t.Errorf("ValidateForge(gitlab) with token and base: %v", err)
	}
}

func TestValidateForge_Unsupported(t *testing.T) {
	cfg := Config{}
	if err := cfg.ValidateForge("bitbucket"); err == nil {
		t.Error("expected error for unsupported forge type")
	}
}

// ---------------------------------------------------------------------------
// GitAuthArgs
// ---------------------------------------------------------------------------

func TestGitAuthArgs_SSHMode_Nil(t *testing.T) {
	args := GitAuthArgs(Config{AuthMode: AuthModeSSH}, "https://github.com/owner/repo.git")
	if args != nil {
		t.Errorf("GitAuthArgs SSH mode: expected nil, got %v", args)
	}
}

func TestGitAuthArgs_TokenMode_ContainsBasicAuth(t *testing.T) {
	cfg := Config{
		AuthMode:            AuthModeToken,
		GitLabTokenUsername: "oauth2",
		GitLabToken:         "mytoken",
		GitLabAPIBase:       "https://gitlab.example.com/api/v4",
	}
	args := GitAuthArgs(cfg, "https://gitlab.example.com/group/repo.git")
	if len(args) == 0 {
		t.Fatal("GitAuthArgs token mode: expected non-empty args")
	}

	want := base64.StdEncoding.EncodeToString([]byte("oauth2:mytoken"))
	found := false
	for _, a := range args {
		if strings.HasPrefix(a, "http.extraHeader=Authorization:") {
			found = true
			if !strings.Contains(a, want) {
				t.Errorf("GitAuthArgs: expected base64 %q in header arg %q", want, a)
			}
		}
	}
	if !found {
		t.Errorf("GitAuthArgs token mode: no Authorization header arg in %v", args)
	}
}

// authHeaderPayload extracts and base64-decodes the Basic Authorization value
// from a GitAuthArgs result, for asserting which username:token was sent.
func authHeaderPayload(t *testing.T, args []string) string {
	t.Helper()
	const prefix = "http.extraHeader=Authorization: Basic "
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(a, prefix))
			if err != nil {
				t.Fatalf("authHeaderPayload: decode %q: %v", a, err)
			}
			return string(decoded)
		}
	}
	t.Fatalf("authHeaderPayload: no Authorization header in %v", args)
	return ""
}

// TestGitAuthArgs_TokenMode_UsesForgeToken is the regression test for the bug
// where token mode always sent GitLabToken regardless of the configured forge.
func TestGitAuthArgs_TokenMode_UsesForgeToken(t *testing.T) {
	cases := []struct {
		name     string
		forge    string
		repoURL  string
		cfg      Config
		wantUser string
		wantTok  string
	}{
		{
			name:     "github config selects GitHubToken",
			forge:    "github",
			repoURL:  "https://github.com/owner/repo.git",
			cfg:      Config{AuthMode: AuthModeToken, GitHubToken: "ghp-token", GitLabToken: "glpat-wrong"},
			wantUser: "x-access-token",
			wantTok:  "ghp-token",
		},
		{
			name:     "github inferred from URL when forge unset",
			forge:    "",
			repoURL:  "https://github.com/owner/repo.git",
			cfg:      Config{AuthMode: AuthModeToken, GitHubToken: "ghp-token", GitLabToken: "glpat-wrong"},
			wantUser: "x-access-token",
			wantTok:  "ghp-token",
		},
		{
			name:     "gitea config selects GiteaToken",
			forge:    "gitea",
			repoURL:  "https://gitea.example.com/owner/repo.git",
			cfg:      Config{AuthMode: AuthModeToken, GiteaToken: "gitea-token", GitLabToken: "glpat-wrong", GiteaAPIBase: "https://gitea.example.com/api/v1"},
			wantUser: "oauth2",
			wantTok:  "gitea-token",
		},
		{
			name:     "forgejo config selects ForgeJOToken",
			forge:    "forgejo",
			repoURL:  "https://forgejo.example.com/owner/repo.git",
			cfg:      Config{AuthMode: AuthModeToken, ForgeJOToken: "forgejo-token", GitLabToken: "glpat-wrong", ForgejoAPIBase: "https://forgejo.example.com/api/v1"},
			wantUser: "oauth2",
			wantTok:  "forgejo-token",
		},
		{
			name:     "gitlab config still selects GitLabToken",
			forge:    "gitlab",
			repoURL:  "https://gitlab.example.com/group/repo.git",
			cfg:      Config{AuthMode: AuthModeToken, GitLabTokenUsername: "oauth2", GitLabToken: "glpat-token", GitLabAPIBase: "https://gitlab.example.com/api/v4"},
			wantUser: "oauth2",
			wantTok:  "glpat-token",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.ForgeType = tc.forge
			args := GitAuthArgs(tc.cfg, tc.repoURL)
			got := authHeaderPayload(t, args)
			want := tc.wantUser + ":" + tc.wantTok
			if got != want {
				t.Errorf("GitAuthArgs payload = %q, want %q", got, want)
			}
		})
	}
}

// TestGitAuthArgs_TokenMode_NoForgeToken_Nil verifies that with no token
// configured for the selected forge, no Authorization header is emitted — a
// public repo still clones and a private one gets git's real auth error.
func TestGitAuthArgs_TokenMode_NoForgeToken_Nil(t *testing.T) {
	cfg := Config{AuthMode: AuthModeToken, ForgeType: "github"}
	if args := GitAuthArgs(cfg, "https://github.com/owner/repo.git"); args != nil {
		t.Errorf("GitAuthArgs with no token: expected nil, got %v", args)
	}
}

// TestGitAuthArgs_TokenMode_RejectsCrossHostRepoURL is the regression test for
// credential exfiltration: a request-controlled repo_url on a host other than
// the configured forge must never receive the daemon's token.
func TestGitAuthArgs_TokenMode_RejectsCrossHostRepoURL(t *testing.T) {
	cfg := Config{
		AuthMode:      AuthModeToken,
		ForgeType:     "gitlab",
		GitLabToken:   "glpat-secret",
		GitLabAPIBase: "https://gitlab.example.com/api/v4",
	}
	cases := []string{
		"https://evil.example.com/group/repo.git",
		"https://gitlab.example.com.evil.example/group/repo.git",
		"https://api.github.com/group/repo.git",
		"git@evil.example.com:group/repo.git",
		// Cleartext HTTP to the right host must not carry the token either.
		"http://gitlab.example.com/group/repo.git",
	}
	for _, repoURL := range cases {
		if args := GitAuthArgs(cfg, repoURL); args != nil {
			t.Errorf("GitAuthArgs(%q) = %v, want nil (token must not leave the configured host)", repoURL, args)
		}
	}
}

// ---------------------------------------------------------------------------
// GitEnv
// ---------------------------------------------------------------------------

func TestGitEnv_AlwaysContainsNoPrompt(t *testing.T) {
	env := GitEnv(Config{AuthMode: AuthModeToken})
	found := false
	for _, e := range env {
		if e == "GIT_TERMINAL_PROMPT=0" {
			found = true
			break
		}
	}
	if !found {
		t.Error("GitEnv: GIT_TERMINAL_PROMPT=0 not found in environment")
	}
}

func TestGitEnv_SSHMode_ContainsGitSSHCommand(t *testing.T) {
	cfg := Config{
		AuthMode:          AuthModeSSH,
		SSHKeyPath:        "/home/user/.ssh/id_ed25519",
		SSHKnownHostsPath: "/home/user/.ssh/known_hosts",
	}
	env := GitEnv(cfg)
	found := false
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
			found = true
			// Key path and known_hosts path must appear in the command.
			if !strings.Contains(e, cfg.SSHKeyPath) {
				t.Errorf("GIT_SSH_COMMAND missing key path %q: %q", cfg.SSHKeyPath, e)
			}
			if !strings.Contains(e, cfg.SSHKnownHostsPath) {
				t.Errorf("GIT_SSH_COMMAND missing known_hosts %q: %q", cfg.SSHKnownHostsPath, e)
			}
		}
	}
	if !found {
		t.Error("GitEnv SSH mode: GIT_SSH_COMMAND not set")
	}
}

func TestGitEnv_SSHMode_NoKeyPath_ContainsGitSSHCommand(t *testing.T) {
	cfg := Config{
		AuthMode:          AuthModeSSH,
		SSHKeyPath:        "",
		SSHKnownHostsPath: "/home/user/.ssh/known_hosts",
	}
	env := GitEnv(cfg)
	found := false
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
			found = true
			if !strings.Contains(e, cfg.SSHKnownHostsPath) {
				t.Errorf("GIT_SSH_COMMAND missing known_hosts %q: %q", cfg.SSHKnownHostsPath, e)
			}
			if strings.Contains(e, "-i ") {
				t.Errorf("GIT_SSH_COMMAND should not contain -i: %q", e)
			}
		}
	}
	if !found {
		t.Error("GitEnv SSH mode: GIT_SSH_COMMAND not set")
	}
}

func TestGitEnv_SSHMode_NoKnownHostsPath(t *testing.T) {
	cfg := Config{
		AuthMode:          AuthModeSSH,
		SSHKeyPath:        "/home/user/.ssh/id_ed25519",
		SSHKnownHostsPath: "",
	}
	env := GitEnv(cfg)
	found := false
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
			found = true
			// An empty known_hosts path must omit the option entirely rather
			// than emit `-o UserKnownHostsFile=`, which ssh rejects.
			if strings.Contains(e, "UserKnownHostsFile=") {
				t.Errorf("GIT_SSH_COMMAND should not set UserKnownHostsFile when path is empty: %q", e)
			}
			if !strings.Contains(e, "-o BatchMode=yes") {
				t.Errorf("GIT_SSH_COMMAND missing BatchMode: %q", e)
			}
		}
	}
	if !found {
		t.Error("GitEnv SSH mode: GIT_SSH_COMMAND not set")
	}
}

func TestGitEnv_TokenMode_NoGitSSHCommand(t *testing.T) {
	env := GitEnv(Config{AuthMode: AuthModeToken})
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
			t.Errorf("GitEnv token mode: unexpected GIT_SSH_COMMAND: %q", e)
		}
	}
}

// ---------------------------------------------------------------------------
// redactArgs
// ---------------------------------------------------------------------------

func TestRedactArgs_RedactsAuthHeader(t *testing.T) {
	secretB64 := base64.StdEncoding.EncodeToString([]byte("oauth2:supersecret"))
	args := []string{
		"-c",
		"http.extraHeader=Authorization: Basic " + secretB64,
		"push", "origin", "my-branch",
	}
	redacted := redactArgs(args)

	for i, a := range redacted {
		if strings.Contains(a, secretB64) {
			t.Errorf("redactArgs: token not redacted at index %d: %q", i, a)
		}
	}
	// The auth header slot should still contain the key, just redacted value.
	if !strings.Contains(redacted[1], "http.extraHeader=Authorization:") {
		t.Errorf("redactArgs: header prefix stripped entirely at index 1: %q", redacted[1])
	}
}

func TestRedactArgs_PassthroughNonAuth(t *testing.T) {
	args := []string{"push", "origin", "my-branch"}
	redacted := redactArgs(args)
	for i, a := range redacted {
		if a != args[i] {
			t.Errorf("redactArgs: non-auth arg[%d] changed: want %q, got %q", i, args[i], a)
		}
	}
}

func TestRedactArgs_EmptySlice(t *testing.T) {
	if redacted := redactArgs([]string{}); len(redacted) != 0 {
		t.Errorf("redactArgs empty: expected empty slice, got %v", redacted)
	}
}

// ---------------------------------------------------------------------------
// PushAndOpenMR — early-exit gates (no actual git subprocess needed)
// ---------------------------------------------------------------------------

func TestPushAndOpenMR_InvalidConfig_ReturnsError(t *testing.T) {
	_, err := PushAndOpenMR(t.Context(), Config{AuthMode: "bad"}, Request{
		Branch:  "agent/ws-123/fix",
		RepoURL: "https://gitlab.example.com/group/repo.git",
		Bundle:  []byte("fake bundle"),
	})
	if err == nil {
		t.Fatal("expected error from PushAndOpenMR with invalid config, got nil")
	}
}

func TestPushAndOpenMR_EmptyBranch_ReturnsError(t *testing.T) {
	cfg := Config{
		AuthMode:            AuthModeToken,
		GitLabTokenUsername: "oauth2",
		GitLabToken:         "token",
		GitLabAPIBase:       "https://gitlab.example.com/api/v4",
	}
	_, err := PushAndOpenMR(t.Context(), cfg, Request{
		Branch:  "",
		RepoURL: "https://gitlab.example.com/group/repo.git",
		Bundle:  []byte("fake bundle"),
	})
	if err == nil {
		t.Fatal("expected error for empty branch, got nil")
	}
}

func TestPushAndOpenMR_InvalidBranchNames_ReturnError(t *testing.T) {
	cfg := Config{
		AuthMode:            AuthModeToken,
		GitLabTokenUsername: "oauth2",
		GitLabToken:         "token",
		GitLabAPIBase:       "https://gitlab.example.com/api/v4",
	}
	for _, branch := range []string{"../etc/passwd", "-bad-branch"} {
		_, err := PushAndOpenMR(t.Context(), cfg, Request{
			Branch:  branch,
			RepoURL: "https://gitlab.example.com/group/repo.git",
			Bundle:  []byte("fake"),
		})
		if err == nil {
			t.Errorf("expected error for invalid branch %q, got nil", branch)
		}
	}
}

// ---------------------------------------------------------------------------
// detectForgeType
// ---------------------------------------------------------------------------

// TestDetectForgeType_ConfigIsAuthoritative verifies that the daemon's
// configured forge wins over any inference from the repo URL. Forge selection
// is an operator decision made at startup; a request (or a repo URL pointing at
// a different host) must never redirect a commit to another forge.
func TestDetectForgeType_ConfigIsAuthoritative(t *testing.T) {
	// Configured gitea, repo on an unrelated host → gitea, not github.
	if got := detectForgeType("git@git.crumpton.org:bear/udpFriend.git", "gitea"); got != "gitea" {
		t.Errorf("detectForgeType(config=gitea) = %q, want %q", got, "gitea")
	}
	// Config is case/whitespace-insensitive.
	if got := detectForgeType("git@git.crumpton.org:bear/udpFriend.git", "  Gitea "); got != "gitea" {
		t.Errorf("detectForgeType(config=\"  Gitea \") = %q, want %q", got, "gitea")
	}
	// No configured forge → fall back to the repo URL.
	if got := detectForgeType("git@github.com:owner/repo.git", ""); got != "github" {
		t.Errorf("detectForgeType(empty config, github URL) = %q, want %q", got, "github")
	}
	if got := detectForgeType("git@git.crumpton.org:bear/udpFriend.git", ""); got != "gitlab" {
		t.Errorf("detectForgeType(empty config, unknown URL) = %q, want %q", got, "gitlab")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// tempFileWithContent creates a temporary file, writes content to it, and
// registers cleanup. Returns the file path.
func tempFileWithContent(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "wsd-test-*")
	if err != nil {
		t.Fatalf("tempFileWithContent: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		t.Fatalf("tempFileWithContent write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("tempFileWithContent close: %v", err)
	}
	return f.Name()
}

// ---------------------------------------------------------------------------
// ValidateRepoURL / GIT_ALLOW_PROTOCOL
// ---------------------------------------------------------------------------

func TestValidateRepoURL(t *testing.T) {
	valid := []string{
		"https://github.com/user/repo.git",
		"http://example.com/user/repo.git",
		"https://gitlab.example.com/group/sub/repo",
		"git@github.com:user/repo.git",
		"git@git.example.com:group/repo",
	}
	for _, url := range valid {
		if err := ValidateRepoURL(url); err != nil {
			t.Errorf("ValidateRepoURL(%q) = %v, want nil", url, err)
		}
	}

	invalid := []string{
		"",
		" ",
		" https://github.com/user/repo.git",
		"https://github.com/user/repo.git ",
		"--upload-pack=touch /tmp/pwned",
		"-c core.pager=less",
		"file:///etc",
		"/home/user/repo",
		"../repo",
		"ssh://git@github.com/user/repo.git",
		"ftp://example.com/repo.git",
		"git@-host:repo",
		"git@host:-repo",
		"git@host:",
		"git@:repo",
		"git@host/path:repo",
		"https://example.com/repo with space.git",
	}
	for _, url := range invalid {
		if err := ValidateRepoURL(url); err == nil {
			t.Errorf("ValidateRepoURL(%q) = nil, want error", url)
		}
	}
}

func TestGitEnv_RestrictsProtocols(t *testing.T) {
	env := GitEnv(Config{AuthMode: AuthModeToken})
	found := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_ALLOW_PROTOCOL=") {
			found = strings.TrimPrefix(kv, "GIT_ALLOW_PROTOCOL=")
		}
	}
	if found != gitAllowedProtocols {
		t.Fatalf("GIT_ALLOW_PROTOCOL = %q, want %q", found, gitAllowedProtocols)
	}

	// The bundle-fetch override must replace the value, not duplicate it.
	overridden := envWithAllowedProtocols(env, "file")
	count := 0
	for _, kv := range overridden {
		if strings.HasPrefix(kv, "GIT_ALLOW_PROTOCOL=") {
			count++
			if kv != "GIT_ALLOW_PROTOCOL=file" {
				t.Errorf("override value = %q, want file", kv)
			}
		}
	}
	if count != 1 {
		t.Fatalf("override left %d GIT_ALLOW_PROTOCOL entries, want 1", count)
	}
}
