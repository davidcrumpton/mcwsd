// Package gitpush handles the one privileged operation in mcwsd: taking a
// git bundle produced inside an untrusted container and pushing it to the
// real remote, then opening a merge request. Nothing in this package ever
// touches a container. It only ever runs `git fetch` against a bundle file
// (which does not execute hooks or smudge filters) and then a plain push
// from a disposable local clone. It never checks out the container's
// working tree, and never runs `git commit`/`git checkout` against
// anything the container wrote directly.
package gitpush

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/forge"
	forgejo "github.com/davidcrumpton/mcwsd/internal/forge/forgejo"
	"github.com/davidcrumpton/mcwsd/internal/forge/gitea"
	"github.com/davidcrumpton/mcwsd/internal/forge/github"
	"github.com/davidcrumpton/mcwsd/internal/forge/gitlab"
)

// AuthMode selects how the daemon authenticates git push traffic to the
// remote. Exactly one mode is active per daemon instance — this is a
// startup-time choice, never something a request can influence. It governs
// git transport auth only; the GitLab REST API call used to open the merge
// request always needs its own token regardless of mode, since an SSH key
// doesn't grant REST API access.
type AuthMode string

const (
	AuthModeSSH   AuthMode = "ssh"
	AuthModeToken AuthMode = "token"
)

// Config holds host-level settings needed to reach GitLab. These should be
// sourced from GKM at daemon startup, never hardcoded, and never passed in
// per-request. The container this config's credentials belong to never
// sees any of these values — only the gitpush package (running purely on
// the host) ever reads them.
type Config struct {
	AuthMode  AuthMode
	ForgeType string

	// SSH mode fields — used for git push transport only.
	SSHKeyPath        string // path to a private key file, e.g. from GKM
	SSHKnownHostsPath string // required in ssh mode; we fail closed rather than disabling host key checking

	// Token mode fields — used for git push transport AND (in both modes)
	// for the selected forge REST API call. The token is sent as an HTTP
	// Basic Authorization header built at request time when used for git
	// push in token mode—it is never embedded in the remote URL.
	GitLabToken         string
	GitLabTokenUsername string
	GitHubToken         string
	GiteaToken          string
	ForgeJOToken        string

	// Forge API config used for opening the merge request after push.
	GitLabAPIBase      string
	GitLabProjectPath  string
	GitHubAPIBase      string
	GitHubProjectPath  string
	GiteaAPIBase       string
	GiteaProjectPath   string
	ForgejoAPIBase     string
	ForgejoProjectPath string
}

// Validate checks that the config is complete enough to actually push and
// open an MR. Call this at daemon startup once an auth mode is configured,
// and again defensively before use, so a misconfiguration surfaces as a
// clear error rather than an opaque git failure three steps into a commit.
//
// It has a pointer receiver because it fills in defaults (e.g. the public
// GitHub API base). With a value receiver those defaults were written to a
// discarded copy, leaving the caller with an empty API base and an MR request
// to a relative URL ("unsupported protocol scheme \"\"").
func (c *Config) Validate() error {
	switch c.AuthMode {
	case AuthModeSSH:
		if c.SSHKeyPath != "" {
			if _, err := os.Stat(c.SSHKeyPath); err != nil {
				return fmt.Errorf("SSH key not readable at %s: %w", c.SSHKeyPath, err)
			}
		}
		// if SSH Known Hosts path is undefined, we allow the system to follow the system
		// known_hosts path.  We do however, validate that if a path is defined, that it is
		// readable.
		if c.SSHKnownHostsPath != "" {
			if _, err := os.Stat(c.SSHKnownHostsPath); err != nil {
				return fmt.Errorf("known_hosts not readable at %s: %w", c.SSHKnownHostsPath, err)
			}
		}
	case AuthModeToken:
		// The token username is forge-specific and validated with the rest
		// of the forge config below (only GitLab lets it be operator-chosen;
		// the others use a documented fixed placeholder).
	default:
		return fmt.Errorf("invalid git-auth-mode %q: must be %q or %q", c.AuthMode, AuthModeSSH, AuthModeToken)
	}

	forgeType := strings.ToLower(strings.TrimSpace(c.ForgeType))
	if forgeType == "" {
		forgeType = "gitlab"
	}

	switch forgeType {
	case "gitlab":
		if c.AuthMode == AuthModeToken && c.GitLabTokenUsername == "" {
			return fmt.Errorf("git-auth-mode=token requires a GitLab token username (e.g. \"oauth2\")")
		}
		if c.GitLabToken == "" {
			return fmt.Errorf("a GitLab API token is required (needed to open the merge request even in ssh mode)")
		}
		if c.GitLabAPIBase == "" {
			return fmt.Errorf("gitlab API base URL is required")
		}
	case "github":
		if c.GitHubToken == "" {
			return fmt.Errorf("a GitHub API token is required")
		}
		// If undefined assume public GitHub API
		if c.GitHubAPIBase == "" {
			c.GitHubAPIBase = "https://api.github.com"
		}
	case "gitea", "forgejo":
		if c.GiteaToken == "" && forgeType == "gitea" {
			return fmt.Errorf("a Gitea API token is required")
		}
		if c.ForgeJOToken == "" && forgeType == "forgejo" {
			return fmt.Errorf("a Forgejo API token is required")
		}
		if forgeType == "gitea" && c.GiteaAPIBase == "" {
			return fmt.Errorf("gitea API base URL is required")
		}
		if forgeType == "forgejo" && c.ForgejoAPIBase == "" {
			return fmt.Errorf("forgejo API base URL is required")
		}
	default:
		return fmt.Errorf("unsupported forge type %q", forgeType)
	}
	return nil
}

// ValidateForge checks that the configuration needed to open a merge request
// against a specific forge is present.
//
// It is deliberately separate from Validate. Validate runs at daemon startup,
// before any repo is known, and falls back to GitLab for backward
// compatibility when ForgeType is unset. The forge actually used is the
// daemon's configured ForgeType (see detectForgeType); this check exists so a
// missing token surfaces as a clear "no GitHub token configured" message
// rather than a confusing API 404 — GitHub answers an unauthenticated request
// for a private repo with 404, hiding the repo rather than returning 403.
func (c Config) ValidateForge(forgeType string) error {
	switch strings.ToLower(strings.TrimSpace(forgeType)) {
	case "gitlab":
		if c.GitLabToken == "" {
			return fmt.Errorf("a GitLab token is required to open a merge request (set the env var named by -gitlab-token-env-var, default MCWSD_GITLAB_TOKEN)")
		}
		if c.GitLabAPIBase == "" {
			return fmt.Errorf("a GitLab API base URL is required (set -gitlab-api-base or MCWSD_GITLAB_API_BASE)")
		}
	case "github":
		if c.GitHubToken == "" {
			return fmt.Errorf("a GitHub token is required to open a pull request (set the env var named by -github-token-env-var, default MCWSD_GITHUB_TOKEN)")
		}
	case "gitea":
		if c.GiteaToken == "" {
			return fmt.Errorf("a Gitea token is required to open a pull request (set the env var named by -gitea-token-env-var, default MCWSD_GITEA_TOKEN)")
		}
		if c.GiteaAPIBase == "" {
			return fmt.Errorf("a Gitea API base URL is required (set -gitea-api-base or MCWSD_GITEA_API_BASE)")
		}
	case "forgejo":
		if c.ForgeJOToken == "" {
			return fmt.Errorf("a Forgejo token is required to open a pull request (set the env var named by -forgejo-token-env-var, default MCWSD_FORGEJO_TOKEN)")
		}
		if c.ForgejoAPIBase == "" {
			return fmt.Errorf("a Forgejo API base URL is required (set -forgejo-api-base or MCWSD_FORGEJO_API_BASE)")
		}
	default:
		return fmt.Errorf("unsupported forge type %q", forgeType)
	}
	return nil
}

// Request describes a push-and-open-MR operation.
type Request struct {
	RepoURL           string // e.g. git@gitlab.crumpton.org:group/project.git (ssh mode) or https://gitlab.../repo.git (token mode)
	BaseBranch        string // e.g. "main" — where the MR targets
	Branch            string // server-chosen branch name, e.g. agent/ws-xxx/add-check-route
	Bundle            []byte // output of `git bundle create` inside the container
	Title             string
	Description       string
	ProjectPath       string
	GitLabProjectPath string // Deprecated compatibility field.
}

// Result is returned on success.
type Result struct {
	Branch string `json:"branch"`
	MRURL  string `json:"mr_url"`
}

// PushAndOpenMR performs the full host-side flow:
//  1. write the bundle to a scratch file
//  2. clone the real repo into a disposable dir
//  3. fetch the branch out of the bundle (data only — no hook execution)
//  4. refuse if the branch already exists on the remote, refuse force-push
//  5. push the branch
//  6. open a GitLab merge request via the REST API
func PushAndOpenMR(ctx context.Context, cfg Config, req Request) (*Result, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("git push config invalid: %w", err)
	}
	if err := ValidateRepoURL(req.RepoURL); err != nil {
		return nil, err
	}
	if req.Branch == "" {
		return nil, fmt.Errorf("branch name must be set by the server, not left empty")
	}
	if strings.Contains(req.Branch, "..") || strings.HasPrefix(req.Branch, "-") {
		return nil, fmt.Errorf("invalid branch name %q", req.Branch)
	}

	scratchDir, err := os.MkdirTemp("", "wsd-push-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create scratch dir: %w", err)
	}
	defer os.RemoveAll(scratchDir)

	bundlePath := filepath.Join(scratchDir, "out.bundle")
	if err := os.WriteFile(bundlePath, req.Bundle, 0o600); err != nil {
		return nil, fmt.Errorf("failed to write bundle: %w", err)
	}

	cloneDir := filepath.Join(scratchDir, "repo")
	env := GitEnv(cfg)
	authArgs := GitAuthArgs(cfg, req.RepoURL)

	cloneArgs := append(append([]string{}, authArgs...), "clone", "--no-checkout", req.RepoURL, cloneDir)
	if err := runGit(ctx, "", env, cloneArgs...); err != nil {
		return nil, fmt.Errorf("failed to clone repo: %w", err)
	}

	// Refuse if the branch already exists on the remote. New branches only.
	lsRemoteArgs := append(append([]string{}, authArgs...), "ls-remote", "--heads", "origin", req.Branch)
	existsOut, err := runGitOutput(ctx, cloneDir, env, lsRemoteArgs...)
	if err != nil {
		return nil, fmt.Errorf("failed to check remote branches: %w", err)
	}
	if strings.TrimSpace(existsOut) != "" {
		return nil, fmt.Errorf("branch %q already exists on remote — refusing to overwrite", req.Branch)
	}

	// Fetch is data-only: it does not execute hooks or smudge/clean filters,
	// unlike checkout or commit would. core.hooksPath is also nulled out
	// defensively in case any future git version changes that assumption.
	// No auth needed here — the bundle is a local file, not a remote. The
	// remote-oriented protocol restriction from GitEnv would reject the local
	// file transport, so allow only "file" for this one invocation.
	bundleEnv := envWithAllowedProtocols(env, "file")
	if err := runGit(ctx, cloneDir, bundleEnv, "-c", "core.hooksPath=/dev/null",
		"fetch", bundlePath, fmt.Sprintf("%s:%s", req.Branch, req.Branch)); err != nil {
		return nil, fmt.Errorf("failed to fetch bundle: %w", err)
	}

	// Push — never force.
	pushArgs := append(append([]string{}, authArgs...), "push", "origin", req.Branch)
	if err := runGit(ctx, cloneDir, env, pushArgs...); err != nil {
		return nil, fmt.Errorf("failed to push branch: %w", err)
	}

	// Opening the merge request is the one hosting-provider-specific step —
	// everything above is generic git plumbing. The forge is a daemon-level,
	// startup-time choice (cfg.ForgeType); it is deliberately NOT taken from
	// the request, so an agent cannot redirect a commit to a different forge
	// than the operator configured. When cfg.ForgeType is unset we fall back
	// to inferring it from the repo URL for backward compatibility.
	selectedForgeType := detectForgeType(req.RepoURL, cfg.ForgeType)
	if err := cfg.ValidateForge(selectedForgeType); err != nil {
		return nil, fmt.Errorf("cannot open a merge request for %s using forge %q: %w", req.RepoURL, selectedForgeType, err)
	}
	forgeInstance, err := newForge(cfg, selectedForgeType)
	if err != nil {
		return nil, fmt.Errorf("failed to build forge client: %w", err)
	}
	projectPath := req.ProjectPath
	if projectPath == "" {
		projectPath = req.GitLabProjectPath
	}
	mrResult, err := forgeInstance.OpenMergeRequest(ctx, forge.OpenMRRequest{
		RepoURL:     req.RepoURL,
		ProjectPath: projectPath,
		Branch:      req.Branch,
		BaseBranch:  req.BaseBranch,
		Title:       req.Title,
		Description: req.Description,
	})
	if err != nil {
		return nil, fmt.Errorf("branch pushed but MR creation failed: %w", err)
	}

	internal.Log.Info("commit_and_pr_complete", "branch", req.Branch, "mr_url", mrResult.URL)
	return &Result{Branch: req.Branch, MRURL: mrResult.URL}, nil
}

// GitEnv builds the environment for git subprocesses. In SSH mode, it
// constructs GIT_SSH_COMMAND from discrete, validated fields rather than
// accepting an arbitrary pre-built string — that keeps "what ssh flags are
// in play" fully in this function instead of being whatever a caller
// happened to pass in.
func GitEnv(cfg Config) []string {
	env := os.Environ()
	if cfg.AuthMode == AuthModeSSH {
		var sshCmdBuilder strings.Builder
		sshCmdBuilder.WriteString("ssh")
		if cfg.SSHKeyPath != "" {
			sshCmdBuilder.WriteString(fmt.Sprintf(" -i %s -o IdentitiesOnly=yes", cfg.SSHKeyPath))
		}
		sshCmdBuilder.WriteString(" -o StrictHostKeyChecking=yes")
		// Only pin a known_hosts file when one is configured. An empty path
		// means "fall back to the system default" (see Validate); emitting
		// `-o UserKnownHostsFile=` with no argument makes ssh abort with
		// `no argument after keyword "userknownhostsfile"`.
		if cfg.SSHKnownHostsPath != "" {
			sshCmdBuilder.WriteString(fmt.Sprintf(" -o UserKnownHostsFile=%s", cfg.SSHKnownHostsPath))
		}
		sshCmdBuilder.WriteString(" -o BatchMode=yes")

		env = append(env, "GIT_SSH_COMMAND="+sshCmdBuilder.String())
	}
	// Never prompt interactively — fail instead of hanging if auth is wrong.
	env = append(env, "GIT_TERMINAL_PROMPT=0")
	// Only allow transports the daemon legitimately uses: HTTPS/HTTP for token
	// auth and SSH for key auth. This blocks file:// and other local transports
	// from being used to read arbitrary host repositories. Commands that must
	// read a local file (the bundle fetch) override this with
	// envWithAllowedProtocols.
	env = append(env, "GIT_ALLOW_PROTOCOL="+gitAllowedProtocols)
	return env
}

// gitAllowedProtocols is the transport allow-list for git subprocesses that
// touch a remote.
const gitAllowedProtocols = "https:http:ssh"

// envWithAllowedProtocols returns a copy of env with GIT_ALLOW_PROTOCOL set to
// protocols, replacing any existing value. Used for the one local-file git
// invocation (fetching the bundle) that must not be restricted to the remote
// transports.
func envWithAllowedProtocols(env []string, protocols string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_ALLOW_PROTOCOL=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GIT_ALLOW_PROTOCOL="+protocols)
}

// ValidateRepoURL rejects repository URLs that would be unsafe to hand to git
// on the host. Only the transports the daemon actually uses are accepted:
// https://, http:// and scp-style git@host:path. In particular this rejects
// option-like values (a leading "-" would be parsed as a git option, e.g.
// --upload-pack=...) and local paths / file:// URLs that would let a caller
// clone an arbitrary repository on the host.
func ValidateRepoURL(raw string) error {
	u := strings.TrimSpace(raw)
	if u == "" {
		return fmt.Errorf("repository URL is empty")
	}
	if u != raw {
		return fmt.Errorf("repository URL %q has leading or trailing whitespace", raw)
	}
	if strings.HasPrefix(u, "-") {
		return fmt.Errorf("repository URL %q must not begin with '-'", raw)
	}
	if strings.ContainsAny(u, "\x00\n\r\t") {
		return fmt.Errorf("repository URL %q contains control characters", raw)
	}

	if strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
		if strings.ContainsAny(u, " \t") {
			return fmt.Errorf("repository URL %q contains whitespace", raw)
		}
		return nil
	}

	// scp-style SSH form: git@host:path. Reject a host or path that starts
	// with '-' so it can never be interpreted as an option by a downstream
	// command, and require a non-empty host and path.
	if strings.HasPrefix(u, "git@") {
		rest := strings.TrimPrefix(u, "git@")
		host, path, ok := strings.Cut(rest, ":")
		if ok && host != "" && path != "" &&
			!strings.HasPrefix(host, "-") && !strings.HasPrefix(path, "-") &&
			!strings.ContainsAny(host, "/ \t") && !strings.ContainsAny(path, " \t") {
			return nil
		}
	}

	return fmt.Errorf("unsupported repository URL %q: only https://, http:// and git@host:path are allowed", raw)
}

// gitHTTPUsername returns the basic-auth username paired with the token for
// the given forge. GitLab lets the operator choose it (e.g. "oauth2" for OAuth
// tokens, or a real username for a project access token); GitHub and
// Gitea/Forgejo use the fixed placeholder each host documents for token auth
// ("x-access-token" and "oauth2" respectively). An empty or unknown forge
// falls back to GitLab, matching detectForgeType.
func (c Config) gitHTTPUsername(forgeType string) string {
	switch strings.ToLower(strings.TrimSpace(forgeType)) {
	case "github":
		return "x-access-token"
	case "gitea", "forgejo":
		return "oauth2"
	default: // gitlab
		if c.GitLabTokenUsername != "" {
			return c.GitLabTokenUsername
		}
		return "oauth2"
	}
}

// gitHTTPToken returns the git-transport token for the given forge, and whether
// one is configured. This is the fix for a daemon configured for a non-GitLab
// forge (e.g. github) that previously always sent the GitLab token — the wrong
// credential, so a private repo could never authenticate. The API tokens are
// also the git-transport tokens; the daemon is configured for exactly one
// forge at startup, so exactly one of these is meaningfully set.
func (c Config) gitHTTPToken(forgeType string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(forgeType)) {
	case "github":
		return c.GitHubToken, c.GitHubToken != ""
	case "gitea":
		return c.GiteaToken, c.GiteaToken != ""
	case "forgejo":
		return c.ForgeJOToken, c.ForgeJOToken != ""
	default: // gitlab
		return c.GitLabToken, c.GitLabToken != ""
	}
}

// GitAuthArgs returns extra `-c` flags to prepend to git invocations that
// actually touch the remote (clone/ls-remote/push). In SSH mode this is
// empty — auth is handled entirely via GIT_SSH_COMMAND in the environment.
// In token mode, the token for the forge serving repoURL is sent as a Basic
// Authorization header via http.extraHeader, which git only applies for the
// lifetime of that single invocation — it is never written into the clone's
// on-disk git config, so it can't be read back out of .git/config later.
//
// The token is selected by forge (github → GitHubToken, gitea → GiteaToken,
// forgejo → ForgeJOToken, otherwise GitLabToken), using the same
// detectForgeType precedence as the merge-request path: the daemon's configured
// forge wins, falling back to inference from the repo URL. When no token is
// configured for that forge the args are nil, so a public repo still clones and
// a private one fails with git's own auth error rather than a bogus header.
func GitAuthArgs(cfg Config, repoURL string) []string {
	if cfg.AuthMode != AuthModeToken {
		return nil
	}
	forgeType := detectForgeType(repoURL, cfg.ForgeType)
	token, ok := cfg.gitHTTPToken(forgeType)
	if !ok {
		return nil
	}
	basicAuth := base64.StdEncoding.EncodeToString([]byte(cfg.gitHTTPUsername(forgeType) + ":" + token))
	return []string{"-c", "http.extraHeader=Authorization: Basic " + basicAuth}
}

func runGit(ctx context.Context, dir string, env []string, args ...string) error {
	_, err := runGitOutput(ctx, dir, env, args...)
	return err
}

func runGitOutput(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s failed: %w: %s", strings.Join(redactArgs(args), " "), err, stderr.String())
	}
	return stdout.String(), nil
}

// redactArgs prevents a token from ever appearing in an error message or
// log line, since runGitOutput's error path otherwise echoes the full
// argv back verbatim.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.HasPrefix(a, "http.extraHeader=Authorization:") {
			out[i] = "http.extraHeader=Authorization: [redacted]"
		} else {
			out[i] = a
		}
	}
	return out
}

// RedactArgs is the exported form of redactArgs for callers outside this
// package that build their own git command lines (e.g. the provisioning
// clone in the backend). Use it before embedding an argv in any error or log
// message so a token-mode Authorization header can never leak.
func RedactArgs(args []string) []string {
	return redactArgs(args)
}

// detectForgeType resolves which forge to use. The daemon's configured forge
// (cfgForgeType) is authoritative: forge selection is a startup-time operator
// choice that no request may influence. Only when it is unset do we fall back
// to inferring the forge from the repo URL, for backward compatibility.
func detectForgeType(repoURL, cfgForgeType string) string {
	if v := strings.TrimSpace(strings.ToLower(cfgForgeType)); v != "" {
		return v
	}
	for _, host := range []string{"github.com", "gitlab.com", "gitea.com", "forgejo.org"} {
		if strings.Contains(strings.ToLower(repoURL), host) {
			switch host {
			case "github.com":
				return "github"
			case "gitea.com":
				return "gitea"
			case "forgejo.org":
				return "forgejo"
			default:
				return "gitlab"
			}
		}
	}
	lowerURL := strings.ToLower(repoURL)
	switch {
	case strings.Contains(lowerURL, "github"):
		return "github"
	case strings.Contains(lowerURL, "forgejo"):
		return "forgejo"
	case strings.Contains(lowerURL, "gitea") || strings.Contains(lowerURL, "gittea"):
		return "gitea"
	default:
		return "gitlab"
	}
}

func newForge(cfg Config, forgeType string) (forge.Forge, error) {
	switch strings.ToLower(forgeType) {
	case "gitlab":
		return gitlab.New(cfg.GitLabAPIBase, cfg.GitLabToken, cfg.GitLabProjectPath), nil
	case "github":
		return github.New(cfg.GitHubAPIBase, cfg.GitHubToken, cfg.GitHubProjectPath), nil
	case "gitea":
		return gitea.New(cfg.GiteaAPIBase, cfg.GiteaToken, cfg.GiteaProjectPath), nil
	case "forgejo":
		return forgejo.New(cfg.ForgejoAPIBase, cfg.ForgeJOToken, cfg.ForgejoProjectPath), nil
	default:
		return nil, fmt.Errorf("unsupported forge type %q", forgeType)
	}
}

// extractProjectPathFromURL previously lived here; GitLab project-path
// derivation is now part of the gitlab.Forge implementation (see
// internal/forge/gitlab), since it's specific to how GitLab identifies
// projects.
