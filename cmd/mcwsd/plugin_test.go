package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
)

const pluginFixtureJS = `
module.exports = {
  name: "fixtrt",
  os: "linux",
  isolation: "container",
  binaries: ["echo"],
  options: [
    { flag: "fixtrt-network", type: "string", default: "bridge", usage: "network" },
  ],
  execArgs: function (h, cmd) { return ["echo"].concat(cmd); },
  provision: function (s) { return { native_id: "x", work_dir: s.work_dir }; },
  destroy: function () {},
  destroyByID: function () {},
};
`

func TestPreScanPluginPath(t *testing.T) {
	if got := preScanPluginPath([]string{"mcwsd", "daemon", "--plugin", "p.js"}); got != "p.js" {
		t.Fatalf("got %q", got)
	}
	if got := preScanPluginPath([]string{"mcwsd", "daemon", "--plugin=p.js"}); got != "p.js" {
		t.Fatalf("got %q", got)
	}
	if got := preScanPluginPath([]string{"mcwsd", "daemon"}); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestInvokedSubcommand(t *testing.T) {
	if got := invokedSubcommand([]string{"mcwsd", "daemon", "--plugin", "p.js"}); got != "daemon" {
		t.Fatalf("got %q", got)
	}
	if got := invokedSubcommand([]string{"mcwsd", "cleanup", "--orphans"}); got != "cleanup" {
		t.Fatalf("got %q", got)
	}
}

func TestLoadScriptPluginRegistersOptions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixtrt.js")
	if err := os.WriteFile(path, []byte(pluginFixtureJS), 0o644); err != nil {
		t.Fatal(err)
	}

	scriptPluginRuntime = nil
	scriptPluginOptions = nil
	scriptPluginErr = nil
	defer func() {
		scriptPluginRuntime = nil
		scriptPluginOptions = nil
		scriptPluginErr = nil
	}()

	fs := pflag.NewFlagSet("daemon", pflag.ContinueOnError)
	loadScriptPlugin(path, fs)

	if scriptPluginErr != nil {
		t.Fatalf("loadScriptPlugin: %v", scriptPluginErr)
	}
	if scriptPluginRuntime == nil || scriptPluginRuntime.Name != "fixtrt" {
		t.Fatalf("runtime not set: %+v", scriptPluginRuntime)
	}
	if fs.Lookup("fixtrt-network") == nil {
		t.Fatal("plugin option flag not registered")
	}
	if err := fs.Parse([]string{"--fixtrt-network", "host"}); err != nil {
		t.Fatal(err)
	}
	if got := scriptPluginOptions.Object()["fixtrt-network"]; got != "host" {
		t.Fatalf("parsed option = %v, want host", got)
	}
}
