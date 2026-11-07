package script

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
)

const testPluginJS = `
module.exports = {
  name: "testrt",
  description: "test runtime",
  version: "0.0.0",
  os: "linux",
  isolation: "container",
  binaries: ["echo", "sh"],
  options: [
    { flag: "network", type: "string", default: "bridge", usage: "network name" },
    { flag: "pool", type: "int", default: "4", usage: "pool size" },
    { flag: "verbose", type: "bool", default: "false", usage: "verbose" },
  ],
  execArgs: function (handle, cmd, workDir, env, opts) {
    var args = ["echo", "exec"];
    if (opts.stdin) args.push("-i");
    args.push(handle.native_id);
    return args.concat(cmd);
  },
  provision: function (spec) {
    return { native_id: "wsd-" + spec.workspace_id, work_dir: spec.work_dir, username: "root", privilege: "root" };
  },
  destroy: function (handle) {},
  destroyByID: function (id) {},
};
`

func loadTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "testrt.js")
	if err := os.WriteFile(path, []byte(testPluginJS), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLoadMetadata(t *testing.T) {
	r := loadTestRuntime(t)
	if r.Name != "testrt" {
		t.Fatalf("name = %q", r.Name)
	}
	if r.OS != "linux" || r.Isolation != "container" {
		t.Fatalf("os/isolation = %q/%q", r.OS, r.Isolation)
	}
	if len(r.Options) != 3 {
		t.Fatalf("options = %d, want 3", len(r.Options))
	}
	if !r.binaries["echo"] || !r.binaries["sh"] {
		t.Fatalf("binaries not populated: %v", r.binaries)
	}
}

func TestBindFlagsAndValues(t *testing.T) {
	r := loadTestRuntime(t)
	fs := pflag.NewFlagSet("daemon", pflag.ContinueOnError)
	vals, err := BindFlags(fs, r.Options)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Parse([]string{"--network", "host", "--verbose"}); err != nil {
		t.Fatal(err)
	}
	obj := vals.Object()
	if obj["network"] != "host" {
		t.Fatalf("network = %v", obj["network"])
	}
	if obj["pool"] != int64(4) {
		t.Fatalf("pool = %v (%T)", obj["pool"], obj["pool"])
	}
	if obj["verbose"] != true {
		t.Fatalf("verbose = %v", obj["verbose"])
	}
}

func TestExecArgsMapping(t *testing.T) {
	r := loadTestRuntime(t)
	tr := r.newTransport("wsd-abc", map[string]interface{}{"native_id": "wsd-abc"})
	argv, err := tr.execArgs([]string{"ls", "/"}, "/w", nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"echo", "exec", "wsd-abc", "ls", "/"}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, argv[i], want[i])
		}
	}
}

func TestExecArgsStdinFlag(t *testing.T) {
	r := loadTestRuntime(t)
	tr := r.newTransport("wsd-abc", map[string]interface{}{"native_id": "wsd-abc"})
	argv, err := tr.execArgs([]string{"cat"}, "", nil, true, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"echo", "exec", "-i", "wsd-abc", "cat"}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, argv[i], want[i])
		}
	}
}

func TestBinaryGate(t *testing.T) {
	_, err := runHostCommand(t.Context(), []string{"sh", "-c", "true"}, nil, map[string]bool{"echo": true})
	if err == nil {
		t.Fatal("expected binary gate to reject sh")
	}
}

func TestOptionCollision(t *testing.T) {
	fs := pflag.NewFlagSet("daemon", pflag.ContinueOnError)
	fs.String("network", "", "")
	opts := []Option{{Flag: "network", Type: "string", Default: "", Usage: ""}}
	if _, err := BindFlags(fs, opts); err == nil {
		t.Fatal("expected collision error")
	}
}
