package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/api"
	appbackend "github.com/davidcrumpton/mcwsd/internal/backend"
	"github.com/davidcrumpton/mcwsd/internal/backend/docker"
	"github.com/davidcrumpton/mcwsd/internal/backend/kubernetes"
	"github.com/davidcrumpton/mcwsd/internal/backend/podman"
	"github.com/davidcrumpton/mcwsd/internal/backend/zones"
	"github.com/davidcrumpton/mcwsd/internal/capacity"
	"github.com/davidcrumpton/mcwsd/internal/gitpush"
	"github.com/davidcrumpton/mcwsd/internal/jobs"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
	"github.com/davidcrumpton/mcwsd/internal/script"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var (
	ApplicationName = "mcwsd"
	Version         = "0.4.000"
)

// Script plugins are the future of the runtime layer: one goja-backed .js
// plugin supplies both lifecycle and transport for a runtime. A single plugin
// backs a single daemon instance; operators run separate instances in
// separate accounts/paths for separate runtimes.
//
// These package-level vars hold the loaded plugin for the running subcommand.
// They are populated during flag setup (so the plugin's declared options can
// be registered before cobra parses), and consumed by buildProvisioner.
var (
	scriptPluginRuntime *script.Runtime
	scriptPluginOptions *script.OptionValues
	scriptPluginErr     error
)

func isRoot() bool {
	return os.Getuid() == 0
}

func currentUserHomeDir() (string, error) {
	currentUser, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve current user's home directory: %w", err)
	}
	if currentUser.HomeDir == "" {
		return "", fmt.Errorf("current user has no home directory")
	}
	return currentUser.HomeDir, nil
}

func main() {
	var debug bool
	var audit bool
	var auditFile string
	// Error and exit if running as root
	if isRoot() {
		internal.Log.Error("ERROR: This program is not designed to run as root.\n")
		os.Exit(1)
	}
	homeDir, err := currentUserHomeDir()
	if err != nil {
		panic(err)
	}
	daemonCmd := newDaemonCmd()
	root := &cobra.Command{
		Use:           "mcwsd",
		Short:         "Mr. Crumpton's Workspace Daemon — ephemeral AI workspace manager",
		SilenceUsage:  true,
		SilenceErrors: true,
		// --debug is a single on/off switch (no levels) applied once, before
		// any subcommand runs, so every provisioner's [DEBUG] output is either
		// present or absent for the whole process.
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			internal.Log.SetDebug(debug)
			if cmd == daemonCmd {
				internal.Log.SetAudit(audit, auditFile)
			}
			internal.Log.Debug("command_start", "command", cmd.CommandPath())
		},
	}

	root.PersistentFlags().BoolVar(&debug, "debug", false,
		"Enable verbose DEBUG logging (default off)")
	daemonCmd.Flags().BoolVar(&audit, "audit", false,
		"Enable structured JSON audit logging (default off)")
	daemonCmd.Flags().StringVar(&auditFile, "audit-file", filepath.Join(homeDir, "audit.jsonl"),
		"Audit log destination (default: ~/audit.jsonl)")

	root.AddCommand(
		daemonCmd,
		newCleanupCmd(),
		newStatusCmd(),
		newVersionCmd(),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// mcwsd daemon
// ---------------------------------------------------------------------------

func newDaemonCmd() *cobra.Command {
	// ---- provisioner flags ----
	var (
		runtimeFlag string
		pluginFlag  string

		// illumos / OpenIndiana zones (runtime=zones|openindiana only)
		zonesOpts       zones.Options
		zonesDNSServers string
	)

	// ---- backend / daemon flags ----
	var (
		provisioningHintsFile string
		workspaceBasePath     string
		bindAddress           string
		port                  int
		cacheDir              string
		maxCacheGB            int
		stateFile             string
		workspaceTTL          time.Duration
		pruneInterval         time.Duration
		pruneOnStartup        bool
		tlsCertFile           string
		tlsKeyFile            string
	)

	// ---- capacity gating flags ----
	var (
		capacityMemoryPercent float64
		capacitySwapPercent   float64
		capacityCPUPercent    float64
		capacityFilesystems   string
	)

	// ---- background job limits ----
	var (
		jobsFile             string
		maxJobsPerWorkspace  int
		maxJobsTotal         int
		jobMaxRuntime        time.Duration
		jobLogMaxBytes       int64
		jobRetention         time.Duration
		execWaitSeconds      int
		provisionWaitSeconds int
		provisionTimeout     time.Duration
	)

	// ---- forge / git auth flags ----
	var (
		gitAuthMode            string
		gitForgeType           string
		gitSSHKeyPathEnvVar    string
		gitSSHKnownHostsEnvVar string
		gitlabTokenEnvVar      string
		gitlabTokenUsername    string
		gitlabAPIBase          string
		gitlabProjectPath      string
		gitHubTokenEnvVar      string
		gitHubAPIBase          string
		gitHubProjectPath      string
		giteaTokenEnvVar       string
		giteaAPIBase           string
		giteaProjectPath       string
		forgejoTokenEnvVar     string
		forgejoAPIBase         string
		forgejoProjectPath     string
		gitCommitEmail         string
		gitCommitName          string
	)

	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the workspace daemon (default mode)",
		Long: `Start the mcwsd HTTP daemon. The daemon provisions ephemeral workspaces on demand,
serves the workspace API, and cleans up stale workspaces periodically.

Flags are grouped into three concerns:

	Provisioner  -- which runtime backs workspaces (docker, podman, kubernetes, zones)
  Backend      -- daemon lifecycle, networking, cache, capacity gating
  Forge        -- git forge auth for commit_and_pr (optional; omit to disable)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if scriptPluginErr != nil {
				return scriptPluginErr
			}
			zonesOpts.DNSServers = splitCommaList(zonesDNSServers)
			return runDaemon(
				runtimeFlag,
				zonesOpts,
				provisioningHintsFile, workspaceBasePath, bindAddress, port, cacheDir, maxCacheGB,
				stateFile, workspaceTTL, pruneInterval, pruneOnStartup,
				tlsCertFile, tlsKeyFile,
				capacityMemoryPercent, capacitySwapPercent, capacityCPUPercent, capacityFilesystems,
				gitAuthMode, gitForgeType,
				gitSSHKeyPathEnvVar, gitSSHKnownHostsEnvVar,
				gitlabTokenEnvVar, gitlabTokenUsername, gitlabAPIBase, gitlabProjectPath,
				gitHubTokenEnvVar, gitHubAPIBase, gitHubProjectPath,
				giteaTokenEnvVar, giteaAPIBase, giteaProjectPath,
				forgejoTokenEnvVar, forgejoAPIBase, forgejoProjectPath,
				gitCommitEmail, gitCommitName,
				jobsFile,
				jobs.Limits{
					MaxPerWorkspace: maxJobsPerWorkspace,
					MaxTotal:        maxJobsTotal,
					MaxRuntime:      jobMaxRuntime,
					MaxLogBytes:     jobLogMaxBytes,
					Retention:       jobRetention,
				},
				execWaitSeconds,
				provisionWaitSeconds, provisionTimeout,
			)
		},
	}

	f := cmd.Flags()

	// -- Provisioner --
	f.StringVar(&runtimeFlag, "runtime", "default", "Workspace provisioner: docker, podman, kubernetes, or zones/openindiana (illumos); Kubernetes uses KUBECONFIG and MCWSD_KUBERNETES_NAMESPACE")
	f.StringVar(&pluginFlag, "plugin", "", "Path to a single .js runtime plugin (compiled from TypeScript); overrides --runtime. One plugin per daemon instance — run separate instances for separate runtimes.")

	// illumos / OpenIndiana zones (zones runtime only). The zonepath parent
	// must already be a ZFS dataset; mcwsd creates per-zone datasets with
	// zoneadm but never creates the pool or the parent.
	f.StringVar(&zonesOpts.ZonePath, "zones-zonepath", "",
		"Parent ZFS dataset for workspace zone roots, e.g. /rpool1/zones (zones runtime only)")
	f.StringVar(&zonesOpts.Brand, "zones-brand", "ipkg",
		"Zone brand used by zoneadm install (zones runtime only)")
	f.StringVar(&zonesOpts.BaseZone, "zones-base-zone", "",
		"Installed, halted golden zone cloned for each workspace; empty uses zoneadm install (zones runtime only)")
	f.StringVar(&zonesOpts.NetworkMode, "zones-network-mode", "shared",
		"Zone network mode: shared or exclusive (zones runtime only)")
	f.StringVar(&zonesOpts.Interface, "zones-interface", "",
		"Global-zone data link for shared-IP zones, or the VNIC uplink for exclusive-IP zones (zones runtime only)")
	f.StringVar(&zonesOpts.IPAddress, "zones-ip-address", "",
		"Shared-IP address, or exclusive-IP static address/prefix; empty uses the global address (shared) or DHCP (exclusive) (zones runtime only)")
	f.StringVar(&zonesOpts.DefRouter, "zones-defrouter", "",
		"Default router for static zone addressing (zones runtime only)")
	f.StringVar(&zonesOpts.IPBaseNetwork, "zones-ip-base-network", "",
		"Base network in CIDR notation for automatic address allocation, e.g. 192.168.1.200/24 (zones runtime only)")
	f.IntVar(&zonesOpts.IPPoolSize, "zones-ip-pool-size", 0,
		"Number of addresses to allocate from --zones-ip-base-network (zones runtime only)")
	f.StringVar(&zonesDNSServers, "zones-dns-servers", "",
		"Comma-separated DNS server IPs for zones; defaults to the global zone resolver (zones runtime only)")
	f.StringVar(&zonesOpts.Escalation, "zones-escalation", "pfexec",
		"Privilege escalation command for zonecfg/zoneadm/zlogin/dladm: pfexec, sudo, doas, or empty to run directly (zones runtime only)")
	f.DurationVar(&zonesOpts.BootTimeout, "zones-boot-timeout", 5*time.Minute,
		"How long to wait for a freshly booted zone to accept zlogin (zones runtime only)")

	// -- Backend --
	f.StringVar(&provisioningHintsFile, "provisioning-hints-file", "/etc/wsd/provisioning_hints.json",
		"Path to a JSON file of environment hints handed to the agent on every workspace (optional; empty disables)")

	homeDir, err := currentUserHomeDir()
	if err != nil {
		panic(err)
	}
	f.StringVar(&workspaceBasePath, "workspace-base-path", homeDir+"/wsd-workspaces",
		"Base directory for workspace repos on the host")
	f.StringVar(&bindAddress, "bind-address", "127.0.0.1",
		"IP address to bind the HTTP server to")
	f.IntVar(&port, "port", 8080,
		"HTTP port to listen on")
	f.StringVar(&cacheDir, "cache-dir", homeDir+"/wsd/cache",
		"Directory for dependency cache")
	f.IntVar(&maxCacheGB, "max-cache-gb", 20,
		"Maximum cache size in GB")
	f.StringVar(&stateFile, "state-file", homeDir+"/wsd/state.json",
		"Path to the workspace tracking file used to find and clean up leftover containers")
	f.StringVar(&jobsFile, "jobs-file", homeDir+"/wsd/jobs.json",
		"Path to the background-job tracking file (empty disables job persistence)")
	f.IntVar(&maxJobsPerWorkspace, "max-jobs-per-workspace", 1,
		"Maximum concurrent background jobs per workspace (0 disables the limit)")
	f.IntVar(&maxJobsTotal, "max-jobs-total", 0,
		"Maximum concurrent background jobs across the whole daemon (0 disables the limit)")
	f.DurationVar(&jobMaxRuntime, "job-max-runtime", 0,
		"Daemon-wide cap on how long any background job may run (0 disables)")
	f.Int64Var(&jobLogMaxBytes, "job-log-max-bytes", 10*1024*1024,
		"Per-stream byte cap on a background job's captured output (0 disables)")
	f.DurationVar(&jobRetention, "job-retention", 24*time.Hour,
		"How long finished job records and log tails are retained (0 keeps until workspace cleanup)")
	f.IntVar(&execWaitSeconds, "exec-wait-seconds", 0,
		"Default seconds an exec blocks before promoting to a background job (0 = never promote; requests can override with wait_seconds/async)")
	f.IntVar(&provisionWaitSeconds, "provision-wait-seconds", 0,
		"Default seconds a provision blocks before promoting the whole pipeline to a background job (0 = never promote; requests can override with wait_seconds/async)")
	f.DurationVar(&provisionTimeout, "provision-timeout", 0,
		"Daemon-wide cap on how long any provisioning pipeline may run (0 disables; requests can override with timeout_seconds)")
	f.DurationVar(&workspaceTTL, "workspace-ttl", 24*time.Hour,
		"Destroy workspaces that haven't been accessed within this long")
	f.DurationVar(&pruneInterval, "prune-interval", 1*time.Hour,
		"How often the daemon prunes stale workspaces while running")
	f.BoolVar(&pruneOnStartup, "prune-on-startup", true,
		"Prune stale workspaces once on daemon startup, before serving requests")
	f.StringVar(&tlsCertFile, "tls-cert-file", "",
		"TLS certificate file (leave empty for plain HTTP)")
	f.StringVar(&tlsKeyFile, "tls-key-file", "",
		"TLS key file (leave empty for plain HTTP)")

	// -- Capacity gating --
	// Before provisioning a new workspace (container, jail, VM, or directory)
	// the daemon can refuse when the host is already under pressure, so a burst
	// of requests cannot push it into swap or fill a pool. Every threshold
	// defaults to 0, which disables that check.
	f.Float64Var(&capacityMemoryPercent, "capacity-memory-percent", 0,
		"Refuse to provision when host memory usage is at or above this percent (0 disables)")
	f.Float64Var(&capacitySwapPercent, "capacity-swap-percent", 0,
		"Refuse to provision when host swap usage is at or above this percent (0 disables; no-op when no swap is configured)")
	f.Float64Var(&capacityCPUPercent, "capacity-cpu-percent", 0,
		"Refuse to provision when host CPU usage is at or above this percent (0 disables)")
	f.StringVar(&capacityFilesystems, "capacity-filesystems", "",
		`Comma-separated path=maxpercent filesystem rules, e.g. "/=90,/var=85,/usr/local/jails=95" (empty disables)`)

	// -- Forge / git auth --
	// Governs the ONE privileged operation in mcwsd (commit_and_pr) and is
	// deliberately a startup-time choice, not something any request can
	// influence. Secret values are read from env vars — populated by GKM
	// before this process starts — never passed as CLI flags, since flags are
	// visible to any local user via `ps`.
	f.StringVar(&gitAuthMode, "git-auth-mode", os.Getenv("MCWSD_GIT_AUTH_MODE"),
		`Git push auth mode: "ssh" or "token". Leave unset to disable commit_and_pr.`)
	f.StringVar(&gitForgeType, "forge-type", os.Getenv("MCWSD_GIT_FORGE_TYPE"),
		"Forge type: gitlab, github, gitea, or forgejo")
	f.StringVar(&gitSSHKeyPathEnvVar, "git-ssh-key-path-env-var", "MCWSD_GIT_SSH_KEY_PATH",
		"Name of the env var holding the path to the SSH private key (ssh mode)")
	f.StringVar(&gitSSHKnownHostsEnvVar, "git-ssh-known-hosts-env-var", "MCWSD_GIT_SSH_KNOWN_HOSTS",
		"Name of the env var holding the path to known_hosts (ssh mode)")
	f.StringVar(&gitlabTokenEnvVar, "gitlab-token-env-var", "MCWSD_GITLAB_TOKEN",
		"Name of the env var holding the GitLab token. Required in both auth modes when using GitLab.")
	f.StringVar(&gitlabTokenUsername, "gitlab-token-username", "oauth2",
		"Username paired with the GitLab token for git push basic auth (token mode only)")
	f.StringVar(&gitlabAPIBase, "gitlab-api-base", os.Getenv("MCWSD_GITLAB_API_BASE"),
		"GitLab API base URL, e.g. https://gitlab.example.com/api/v4")
	f.StringVar(&gitlabProjectPath, "gitlab-project-path", os.Getenv("MCWSD_GITLAB_PROJECT_PATH"),
		"URL-encoded namespace/project path, or numeric project ID (optional — can be provided per-request)")
	f.StringVar(&gitHubTokenEnvVar, "github-token-env-var", "MCWSD_GITHUB_TOKEN",
		"Name of the env var holding the GitHub token. Required when using GitHub.")
	f.StringVar(&gitHubAPIBase, "github-api-base", os.Getenv("MCWSD_GITHUB_API_BASE"),
		"GitHub API base URL, e.g. https://api.github.com")
	f.StringVar(&gitHubProjectPath, "github-project-path", os.Getenv("MCWSD_GITHUB_PROJECT_PATH"),
		"Owner/repo path, optional when it can be derived from the repo URL")
	f.StringVar(&giteaTokenEnvVar, "gitea-token-env-var", "MCWSD_GITEA_TOKEN",
		"Name of the env var holding the Gitea token. Required when using Gitea.")
	f.StringVar(&giteaAPIBase, "gitea-api-base", os.Getenv("MCWSD_GITEA_API_BASE"),
		"Gitea API base URL, e.g. https://gitea.example.com/api/v1")
	f.StringVar(&giteaProjectPath, "gitea-project-path", os.Getenv("MCWSD_GITEA_PROJECT_PATH"),
		"Owner/repo path, optional when it can be derived from the repo URL")
	f.StringVar(&forgejoTokenEnvVar, "forgejo-token-env-var", "MCWSD_FORGEJO_TOKEN",
		"Name of the env var holding the Forgejo token. Required when using Forgejo.")
	f.StringVar(&forgejoAPIBase, "forgejo-api-base", os.Getenv("MCWSD_FORGEJO_API_BASE"),
		"Forgejo API base URL, e.g. https://forgejo.example.com/api/v1")
	f.StringVar(&forgejoProjectPath, "forgejo-project-path", os.Getenv("MCWSD_FORGEJO_PROJECT_PATH"),
		"Owner/repo path, optional when it can be derived from the repo URL")
	f.StringVar(&gitCommitEmail, "git-commit-email", "mcwsd-agent@localhost",
		"Author/committer email recorded on commits created by commit_and_pr")
	f.StringVar(&gitCommitName, "git-commit-name", "mcwsd-agent",
		"Author/committer name recorded on commits created by commit_and_pr")

	// If --plugin was supplied, load it now and register its declared options
	// onto the daemon flagset before cobra parses, so the plugin's flags show
	// up in `mcwsd daemon --help`.
	if p := preScanPluginPath(os.Args); p != "" && invokedSubcommand(os.Args) == "daemon" {
		loadScriptPlugin(p, f)
	}

	return cmd
}

func runDaemon(
	runtimeFlag string,
	zonesOpts zones.Options,
	provisioningHintsFile, workspaceBasePath string,
	bindAddress string, port int, cacheDir string, maxCacheGB int,
	stateFile string, workspaceTTL, pruneInterval time.Duration, pruneOnStartup bool,
	tlsCertFile, tlsKeyFile string,
	capacityMemoryPercent, capacitySwapPercent, capacityCPUPercent float64,
	capacityFilesystems string,
	gitAuthMode, gitForgeType string,
	gitSSHKeyPathEnvVar, gitSSHKnownHostsEnvVar string,
	gitlabTokenEnvVar, gitlabTokenUsername, gitlabAPIBase, gitlabProjectPath string,
	gitHubTokenEnvVar, gitHubAPIBase, gitHubProjectPath string,
	giteaTokenEnvVar, giteaAPIBase, giteaProjectPath string,
	forgejoTokenEnvVar, forgejoAPIBase, forgejoProjectPath string,
	gitCommitEmail, gitCommitName string,
	jobsFile string, jobLimits jobs.Limits,
	execWaitSeconds int,
	provisionWaitSeconds int, provisionTimeout time.Duration,
) error {
	// Session ID: one random ID per daemon run, stamped on every workspace
	// this run creates. It's how a future run (or cleanup) tells "created by
	// the currently running daemon" apart from "leftover from a run that got
	// killed or aborted before it could clean up after itself".
	sessionID, err := generateSessionID()
	if err != nil {
		return fmt.Errorf("failed to generate session id: %w", err)
	}
	internal.Log.Info("session_started", "application_name", ApplicationName, "version", Version, "session_id", sessionID)

	authToken := os.Getenv("MCWSD_TOKEN")
	if authToken == "" {
		internal.Log.Warn("MCWSD_TOKEN not set, generating random token")
		authToken = fmt.Sprintf("wsd-%d", time.Now().UnixNano())
		internal.Log.UnsanitizedInfo("generated_token", "token", authToken)
	}

	// Assemble git push config. Note the secret *values* come from the
	// env vars named above (sourced from GKM), not from the flags
	// themselves, which only carry env var *names* and non-secret settings.
	gitCfg := gitpush.Config{
		AuthMode:            gitpush.AuthMode(gitAuthMode),
		ForgeType:           gitForgeType,
		SSHKeyPath:          os.Getenv(gitSSHKeyPathEnvVar),
		SSHKnownHostsPath:   os.Getenv(gitSSHKnownHostsEnvVar),
		GitLabToken:         os.Getenv(gitlabTokenEnvVar),
		GitLabTokenUsername: gitlabTokenUsername,
		GitLabAPIBase:       gitlabAPIBase,
		GitLabProjectPath:   gitlabProjectPath,
		GitHubToken:         os.Getenv(gitHubTokenEnvVar),
		GitHubAPIBase:       gitHubAPIBase,
		GitHubProjectPath:   gitHubProjectPath,
		GiteaToken:          os.Getenv(giteaTokenEnvVar),
		GiteaAPIBase:        giteaAPIBase,
		GiteaProjectPath:    giteaProjectPath,
		ForgeJOToken:        os.Getenv(forgejoTokenEnvVar),
		ForgejoAPIBase:      forgejoAPIBase,
		ForgejoProjectPath:  forgejoProjectPath,
	}

	if gitAuthMode == "" {
		if gitForgeType != "" || gitlabAPIBase != "" || gitlabProjectPath != "" ||
			gitHubAPIBase != "" || gitHubProjectPath != "" || giteaAPIBase != "" ||
			giteaProjectPath != "" || forgejoAPIBase != "" || forgejoProjectPath != "" ||
			os.Getenv(gitSSHKeyPathEnvVar) != "" || os.Getenv(gitSSHKnownHostsEnvVar) != "" ||
			os.Getenv(gitlabTokenEnvVar) != "" || os.Getenv(gitHubTokenEnvVar) != "" ||
			os.Getenv(giteaTokenEnvVar) != "" || os.Getenv(forgejoTokenEnvVar) != "" {
			return fmt.Errorf("forge options were supplied without --git-auth-mode; configure git auth or remove all forge options")
		}
		internal.Log.Warn("git_auth_not_configured", "detail",
			`running in forgeless mode; blank workspaces are supported, repository preparation and commit_and_pr require forge authentication`)
	} else if err := gitCfg.Validate(); err != nil {
		// The operator explicitly tried to configure git auth and got it
		// wrong — fail fast and loud rather than starting up in a broken
		// half-configured state that only surfaces three steps into a commit.
		return fmt.Errorf("invalid git auth config: %w", err)
	} else {
		internal.Log.Info("git_auth_configured", "mode", gitAuthMode, "forge_type", gitForgeType)
	}

	// Assemble capacity gating config. Parsing/validation happens here so a
	// bad rule is a startup error, not a surprise on the first provision.
	capacityRules, err := capacity.ParseFilesystemRules(capacityFilesystems)
	if err != nil {
		return fmt.Errorf("invalid --capacity-filesystems: %w", err)
	}
	capacityCfg := capacity.Config{
		MemoryPercent: capacityMemoryPercent,
		SwapPercent:   capacitySwapPercent,
		CPUPercent:    capacityCPUPercent,
		Filesystems:   capacityRules,
	}
	if err := capacityCfg.Validate(); err != nil {
		return fmt.Errorf("invalid capacity config: %w", err)
	}
	if capacityCfg.Enabled() {
		internal.Log.Info("capacity_gating_enabled",
			"memory_percent", capacityCfg.MemoryPercent,
			"swap_percent", capacityCfg.SwapPercent,
			"cpu_percent", capacityCfg.CPUPercent,
			"filesystems", len(capacityCfg.Filesystems))
	}

	// Create provisioner
	provisioner, err := buildProvisioner(
		runtimeFlag,
		zonesOpts,
	)
	if err != nil {
		return err
	}

	// Give the provisioner a chance to fail fast on a misconfigured host
	// (missing hypervisor, missing master image, unusable privilege
	// escalation, ...) before the daemon starts accepting work. This turns a
	// problem the agent would otherwise hit mid-provision into a startup
	// error the operator sees immediately.
	if v, ok := provisioner.(runtime.Validator); ok {
		if err := v.Validate(context.Background()); err != nil {
			return fmt.Errorf("provisioner validation failed (runtime=%s): %w", runtimeFlag, err)
		}
		internal.Log.Info("provisioner_validated", "runtime", runtimeFlag)
	}
	if err := provisioner.Init(context.Background()); err != nil {
		return fmt.Errorf("provisioner initialization failed (runtime=%s): %w", runtimeFlag, err)
	}
	internal.Log.Info("provisioner_initialized", "runtime", runtimeFlag)
	if runtimeFlag == "zones" || runtimeFlag == "openindiana" {
		internal.Log.Info("zones_network_mode", "mode", zonesOpts.NetworkMode, "zonepath", zonesOpts.ZonePath)
	}

	// Load administrator-authored environment hints, if any. A missing file is
	// not an error; a malformed one is, so a typo surfaces at startup.
	provisioningHints, err := loadProvisioningHints(provisioningHintsFile)
	if err != nil {
		return fmt.Errorf("invalid provisioning hints file: %w", err)
	}

	// Create backend
	backend, err := appbackend.NewBackend(provisioner, cacheDir, maxCacheGB, gitCfg, sessionID, stateFile, workspaceBasePath, gitCommitEmail, gitCommitName)
	if err != nil {
		return fmt.Errorf("failed to create backend: %w", err)
	}
	backend.WithCapacityCheck(capacityCfg).WithProvisioningHints(provisioningHints)
	backend.WithExecWait(execWaitSeconds)
	backend.WithProvisionWait(provisionWaitSeconds).WithProvisionTimeout(provisionTimeout)
	if _, err := backend.WithJobs(jobsFile, jobLimits); err != nil {
		return fmt.Errorf("failed to configure background jobs: %w", err)
	}
	if jobsFile != "" {
		internal.Log.Info("job_limits_configured",
			"max_per_workspace", jobLimits.MaxPerWorkspace,
			"max_total", jobLimits.MaxTotal,
			"max_runtime", jobLimits.MaxRuntime,
			"max_log_bytes", jobLimits.MaxLogBytes,
			"retention", jobLimits.Retention)
	}

	// Recover workspaces from previous daemon session if the provisioner
	// supports it. This allows workspaces to persist across daemon restarts.
	// Only workspaces whose containers are still running will be recovered.
	if _, ok := provisioner.(runtime.WorkspaceRecoverer); ok {
		internal.Log.Info("workspace_recovery_started")
		backend.RecoverWorkspaces(context.Background())
		internal.Log.Info("workspace_recovery_complete")

		// After recovery, check if there are any remaining leftovers that couldn't be recovered
		leftovers := backend.Leftovers()
		if len(leftovers) > 0 {
			internal.Log.Warn("unrecovered_workspaces_found", "count", len(leftovers))
			for _, e := range leftovers {
				internal.Log.Warn("unrecovered_workspace", "workspace_id", e.WorkspaceID,
					"container_name", e.ContainerName, "session_id", e.SessionID,
					"created_at", e.CreatedAt.Format(time.RFC3339), "repo_url", e.RepoURL)
			}
			internal.Log.Warn("unrecovered_workspaces_hint",
				"detail", "these workspaces may be missing required metadata for recovery; run 'mcwsd cleanup --orphans' to remove them")
		}
	} else {
		// Report anything the state file knows about that this session didn't
		// create itself — i.e. containers left behind by a previous run that
		// never made it through a clean Destroy (usually because the process
		// was killed or aborted mid-task).
		leftovers := backend.Leftovers()
		if len(leftovers) > 0 {
			internal.Log.Warn("leftover_workspaces_found", "count", len(leftovers))
			for _, e := range leftovers {
				internal.Log.Warn("leftover_workspace", "workspace_id", e.WorkspaceID,
					"container_name", e.ContainerName, "session_id", e.SessionID,
					"created_at", e.CreatedAt.Format(time.RFC3339), "repo_url", e.RepoURL)
			}
			internal.Log.Warn("leftover_workspaces_hint",
				"detail", "run 'mcwsd cleanup --orphans' to remove these, or DELETE /workspaces/{id} for a specific one")
		}
	}

	// Reconcile persisted jobs: adopt those whose workspace survived and mark
	// the rest interrupted. Runs after workspace recovery so it can probe a
	// live runtime for the truth rather than trusting the stored status.
	backend.RecoverJobs(context.Background())

	// Prune-on-startup: tidy up anything the previous run's agents forgot
	// to destroy before serving new requests. Runs once, best-effort.
	if pruneOnStartup {
		removed, failed := backend.Prune(context.Background(), workspaceTTL)
		internal.Log.Info("startup_prune_complete", "removed", removed, "failed", failed)
		if n := backend.PruneJobs(time.Now()); n > 0 {
			internal.Log.Info("startup_job_prune_complete", "removed", n)
		}
	}

	// Create API server
	server := api.NewServer(backend, authToken)

	var httpServer *http.Server

	if tlsCertFile != "" && tlsKeyFile != "" {
		internal.Log.Info("starting_tls_server", "address", bindAddress, "port", port, "tls_cert_file", tlsCertFile, "tls_key_file", tlsKeyFile)
		httpServer = &http.Server{
			Addr:    fmt.Sprintf("%s:%d", bindAddress, port),
			Handler: server,
		}
		if err := httpServer.ListenAndServeTLS(tlsCertFile, tlsKeyFile); err != nil {
			return fmt.Errorf("TLS server error: %w", err)
		}
	} else {
		httpServer = &http.Server{
			Addr:    fmt.Sprintf("%s:%d", bindAddress, port),
			Handler: server,
		}
	}

	// Graceful shutdown
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGTERM, syscall.SIGINT)

	// Pruner context: cancelled on shutdown so the periodic prune loop
	// stops with the daemon instead of spinning until its next tick.
	pruneCtx, pruneCancel := context.WithCancel(context.Background())
	defer pruneCancel()

	go func() {
		internal.Log.UnsanitizedInfo("server_starting", "address", bindAddress, "port", port, "auth_token_set", authToken != "")
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			internal.Log.Error("server_error", "error", err)
			os.Exit(1)
		}
	}()

	// Periodic pruner: reaps workspaces the agent forgot to destroy while
	// the daemon keeps running. Logs only when there's something to report
	// so a quiet daemon stays quiet.
	go func() {
		ticker := time.NewTicker(pruneInterval)
		defer ticker.Stop()
		for {
			select {
			case <-pruneCtx.Done():
				return
			case <-ticker.C:
				removed, failed := backend.Prune(pruneCtx, workspaceTTL)
				if removed > 0 || failed > 0 {
					internal.Log.Info("periodic_prune_complete", "removed", removed, "failed", failed)
				}
				if n := backend.PruneJobs(time.Now()); n > 0 {
					internal.Log.Info("periodic_job_prune_complete", "removed", n)
				}
			}
		}
	}()

	// Wait for shutdown signal
	<-shutdown
	internal.Log.Info("shutdown_signal_received")
	pruneCancel()

	// Stop accepting new prepare calls, let in-flight execs finish with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		internal.Log.Error("server_shutdown_error", "error", err)
	}

	internal.Log.Info("server_stopped")
	return nil
}

// ---------------------------------------------------------------------------
// mcwsd cleanup
// ---------------------------------------------------------------------------

func newCleanupCmd() *cobra.Command {
	var (
		stateFile    string
		jobsFile     string
		runtimeFlag  string
		pluginFlag   string
		doOrphans    bool
		doPrune      bool
		workspaceTTL time.Duration
		jobRetention time.Duration
		zonesOpts    zones.Options
	)

	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Remove orphaned or stale workspaces and exit",
		Long: `Perform one-shot cleanup operations without starting the daemon.

  --orphans   Force-remove workspaces tracked in the state file that were left
              behind by a previous daemon run that did not shut down cleanly.

  --prune     Remove workspaces that have not been accessed within --ttl.
              Equivalent to the daemon's periodic prune but runs once and exits.

Both flags may be combined. Neither requires forge configuration.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if scriptPluginErr != nil {
				return scriptPluginErr
			}
			return runCleanup(stateFile, jobsFile, runtimeFlag, workspaceTTL, jobRetention, doOrphans, doPrune, zonesOpts)
		},
	}

	f := cmd.Flags()
	f.StringVar(&stateFile, "state-file", "/var/lib/wsd/state.json",
		"Path to the workspace tracking file")
	f.StringVar(&jobsFile, "jobs-file", "/var/lib/wsd/jobs.json",
		"Path to the background-job tracking file (empty disables job cleanup)")
	f.DurationVar(&jobRetention, "job-retention", 24*time.Hour,
		"Remove finished job records older than this")
	f.StringVar(&runtimeFlag, "runtime", "docker",
		"Workspace provisioner used when the state file was written: docker, podman, kubernetes, or zones/openindiana")
	f.StringVar(&pluginFlag, "plugin", "",
		"Path to a single .js runtime plugin used when the state file was written (overrides --runtime)")
	f.StringVar(&zonesOpts.ZonePath, "zones-zonepath", "",
		"Parent ZFS dataset for workspace zones (zones runtime only; needed for orphan resource cleanup)")
	f.StringVar(&zonesOpts.NetworkMode, "zones-network-mode", "shared",
		"Zone network mode used when the state file was written: shared or exclusive (zones runtime only)")
	f.StringVar(&zonesOpts.BaseZone, "zones-base-zone", "",
		"Golden zone to preserve during orphan cleanup (zones runtime only)")
	f.StringVar(&zonesOpts.Escalation, "zones-escalation", "pfexec",
		"Privilege escalation command for zoneadm/zonecfg/dladm (zones runtime only)")
	f.BoolVar(&doOrphans, "orphans", false,
		"Force-remove workspaces left behind by a previous not-cleanly-shut-down daemon run")
	f.BoolVar(&doPrune, "prune", false,
		"Remove workspaces not accessed within --ttl")
	f.DurationVar(&workspaceTTL, "ttl", 24*time.Hour,
		"TTL used with --prune")

	if p := preScanPluginPath(os.Args); p != "" && invokedSubcommand(os.Args) == "cleanup" {
		loadScriptPlugin(p, f)
	}

	return cmd
}

func runCleanup(stateFile, jobsFile, runtimeFlag string, workspaceTTL, jobRetention time.Duration, doOrphans, doPrune bool, zonesOpts zones.Options) error {
	if !doOrphans && !doPrune {
		return fmt.Errorf("specify at least one of --orphans or --prune")
	}

	sessionID, err := generateSessionID()
	if err != nil {
		return fmt.Errorf("failed to generate session id: %w", err)
	}

	// Cleanup only needs a minimal provisioner — enough to destroy containers/
	// jails/VMs — and no forge config at all.
	provisioner, err := buildProvisioner(
		runtimeFlag,
		zonesOpts,
	)
	if err != nil {
		return err
	}

	// Use empty gitCfg — cleanup never touches forge auth.
	backend, err := appbackend.NewBackend(
		provisioner,
		"", 0,
		gitpush.Config{},
		sessionID,
		stateFile,
		"", // workspaceBasePath unused for cleanup
		"", "",
	)
	if err != nil {
		return fmt.Errorf("failed to open state: %w", err)
	}
	if _, err := backend.WithJobs(jobsFile, jobs.Limits{Retention: jobRetention}); err != nil {
		return fmt.Errorf("failed to open jobs store: %w", err)
	}

	var totalFailed int

	if doOrphans {
		leftovers := backend.Leftovers()
		if len(leftovers) == 0 {
			internal.Log.Info("cleanup_orphans_none_found")
		} else {
			internal.Log.Warn("leftover_workspaces_found", "count", len(leftovers))
			for _, e := range leftovers {
				if err := backend.DestroyLeftover(context.Background(), e.WorkspaceID); err != nil {
					internal.Log.Error("cleanup_orphan_failed", "workspace_id", e.WorkspaceID, "error", err)
					totalFailed++
					continue
				}
				internal.Log.Info("cleanup_orphan_removed", "workspace_id", e.WorkspaceID, "container_name", e.ContainerName)
			}

			// Sweep provisioner resources not tracked by any live workspace. This
			// catches artifacts a crash left behind between resource creation and
			// state tracking (e.g. a VM disk cloned but never started).
			if removed, err := backend.CleanupOrphanResources(context.Background()); err != nil {
				internal.Log.Error("cleanup_orphan_resources_failed", "error", err)
				totalFailed++
			} else if removed > 0 {
				internal.Log.Info("cleanup_orphan_resources_removed", "count", removed)
			}

			internal.Log.Info("cleanup_orphans_complete", "removed", len(leftovers)-totalFailed, "failed", totalFailed)
		}
	}

	if doPrune {
		removed, failed := backend.Prune(context.Background(), workspaceTTL)
		totalFailed += failed
		if failed > 0 {
			internal.Log.Error("prune_incomplete", "removed", removed, "failed", failed)
		} else {
			internal.Log.Info("prune_complete", "removed", removed)
		}
	}

	// Drop finished job records past their retention window.
	if n := backend.PruneJobs(time.Now()); n > 0 {
		internal.Log.Info("cleanup_job_prune_complete", "removed", n)
	}

	if totalFailed > 0 {
		return fmt.Errorf("cleanup completed with %d failure(s)", totalFailed)
	}
	return nil
}

// ---------------------------------------------------------------------------
// mcwsd status
// ---------------------------------------------------------------------------

func newStatusCmd() *cobra.Command {
	var stateFile string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show tracked workspaces from the state file",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(stateFile)
		},
	}

	cmd.Flags().StringVar(&stateFile, "state-file", "/var/lib/wsd/state.json",
		"Path to the workspace tracking file")

	return cmd
}

func runStatus(stateFile string) error {
	sessionID, err := generateSessionID()
	if err != nil {
		return fmt.Errorf("failed to generate session id: %w", err)
	}

	// Build a minimal backend just to read the state file.
	backend, err := appbackend.NewBackend(
		nil, // no provisioner needed for status
		"", 0,
		gitpush.Config{},
		sessionID,
		stateFile,
		"", "", "",
	)
	if err != nil {
		return fmt.Errorf("failed to read state file: %w", err)
	}

	leftovers := backend.Leftovers()
	if len(leftovers) == 0 {
		fmt.Println("No tracked workspaces found in state file.")
		return nil
	}

	fmt.Printf("%-36s  %-30s  %-12s  %s\n", "WORKSPACE ID", "CONTAINER NAME", "SESSION", "CREATED")
	fmt.Println(strings.Repeat("-", 100))
	for _, e := range leftovers {
		fmt.Printf("%-36s  %-30s  %-12s  %s\n",
			e.WorkspaceID, e.ContainerName, e.SessionID, e.CreatedAt.Format(time.RFC3339))
	}
	return nil
}

// ---------------------------------------------------------------------------
// mcwsd version
// ---------------------------------------------------------------------------

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version and exit",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("%s %s\n", ApplicationName, Version)
		},
	}
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// preScanPluginPath reads --plugin out of the raw argv before cobra has parsed
// it, because the plugin's own flags must be registered before parsing. It
// handles both --plugin <path> and --plugin=<path>.
func preScanPluginPath(args []string) string {
	for i, a := range args {
		if a == "--plugin" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, "--plugin=") {
			return strings.TrimPrefix(a, "--plugin=")
		}
	}
	return ""
}

// invokedSubcommand returns which mcwsd subcommand the user is running, so
// only the constructed command that will actually execute loads the plugin
// (both newDaemonCmd and newCleanupCmd are built at startup, and loading twice
// would clobber the shared package-level vars).
func invokedSubcommand(args []string) string {
	for _, a := range args[1:] {
		switch a {
		case "daemon", "cleanup", "status", "version":
			return a
		}
	}
	return ""
}

// loadScriptPlugin loads the plugin at path and registers its declared options
// onto fs. The results are stored in the package-level vars for buildProvisioner.
func loadScriptPlugin(path string, fs *pflag.FlagSet) {
	rt, err := script.Load(path)
	if err != nil {
		scriptPluginErr = fmt.Errorf("load runtime plugin %s: %w", path, err)
		return
	}
	vals, err := script.BindFlags(fs, rt.Options)
	if err != nil {
		scriptPluginErr = fmt.Errorf("register options for runtime plugin %s: %w", path, err)
		return
	}
	scriptPluginRuntime = rt
	scriptPluginOptions = vals
}

// splitCommaList parses a comma-separated flag value, trimming whitespace and
// dropping empty entries.
func splitCommaList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// buildProvisioner constructs the appropriate runtime.Provisioner from flags.
// It is called by both the daemon and cleanup subcommands.
func buildProvisioner(
	runtimeFlag string,
	zonesOpts zones.Options,
) (runtime.Provisioner, error) {
	if scriptPluginRuntime != nil {
		if scriptPluginOptions == nil {
			return nil, fmt.Errorf("runtime plugin loaded but its options were not bound")
		}
		internal.Log.Info("using_script_plugin", "plugin", scriptPluginRuntime.Name, "os", scriptPluginRuntime.OS, "isolation", scriptPluginRuntime.Isolation)
		return scriptPluginRuntime.NewProvisioner(scriptPluginOptions), nil
	}
	switch runtimeFlag {
	case "podman":
		return podman.NewPodmanProvisioner(), nil
	case "kubernetes":
		namespace := os.Getenv("MCWSD_KUBERNETES_NAMESPACE")
		if namespace == "" {
			namespace = "default"
		}
		return kubernetes.NewKubernetesProvisioner("", namespace), nil
	case "zones", "openindiana":
		// illumos / OpenIndiana zones
		return zones.NewZonesProvisioner(zonesOpts), nil
	default:
		// Docker (default)
		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return nil, fmt.Errorf("failed to create docker client: %w", err)
		}
		networkName := os.Getenv("MCWSD_NETWORK_NAME")
		if networkName == "" {
			networkName = "bridge"
		}
		return docker.NewDockerProvisioner(cli, networkName), nil
	}
}

// generateSessionID returns a short random hex ID unique to this daemon
// run. It's stamped on every state-file entry this run creates so a later
// run can tell which entries are its own versus leftovers from a run that
// never shut down cleanly.
func generateSessionID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate session id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// loadProvisioningHints reads the optional provisioning-hints file. A missing
// file (or an empty path) yields no hints and no error; a file that exists but
// is not valid JSON is an error, so a typo is caught at startup rather than
// silently shipping malformed guidance to the agent.
func loadProvisioningHints(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return "", nil
	}
	if !json.Valid(trimmed) {
		return "", fmt.Errorf("%s is not valid JSON", path)
	}
	return string(trimmed), nil
}
