package main

import (
	"fmt"
	"strings"

	"github.com/davidcrumpton/mcwsd/internal/script"
	"github.com/spf13/pflag"
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
