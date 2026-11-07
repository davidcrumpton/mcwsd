package zones

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal"
)

type execCall struct {
	name  string
	args  []string
	stdin string
}

type fakeExec struct {
	calls  []execCall
	result *internal.ExecResult
	err    error
}

func (f *fakeExec) run(ctx context.Context, stdin io.Reader, name string, args ...string) (*internal.ExecResult, error) {
	c := execCall{name: name, args: append([]string(nil), args...)}
	if stdin != nil {
		data, _ := io.ReadAll(stdin)
		c.stdin = string(data)
	}
	f.calls = append(f.calls, c)
	if f.err != nil {
		return nil, f.err
	}
	if f.result != nil {
		return f.result, nil
	}
	return &internal.ExecResult{}, nil
}

func TestBuildZloginPayload(t *testing.T) {
	payload := buildZloginPayload(
		[]string{"git", "status"},
		"/workspace/repo",
		[]string{"FOO=bar"},
	)
	for _, want := range []string{
		"cd /workspace/repo &&",
		"env PATH=" + zonesPath,
		"FOO=bar",
		"git status",
	} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload missing %q:\n%s", want, payload)
		}
	}
}

func TestZonesExecTransportExecUsesEscalation(t *testing.T) {
	fake := &fakeExec{}
	tr := NewZonesExecTransport("wsd-ws-test", "pfexec", "shared", "")
	tr.run = fake.run

	if _, err := tr.Exec(context.Background(), []string{"git", "status"}, "/workspace/repo", []string{"FOO=bar"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(fake.calls))
	}
	call := fake.calls[0]
	if call.name != "pfexec" {
		t.Errorf("command = %q, want pfexec", call.name)
	}
	if len(call.args) < 5 || call.args[0] != "zlogin" || call.args[1] != "wsd-ws-test" || call.args[2] != "sh" || call.args[3] != "-c" {
		t.Fatalf("unexpected args: %v", call.args)
	}
	if payload := call.args[4]; !strings.Contains(payload, "git status") || !strings.Contains(payload, "FOO=bar") {
		t.Errorf("unexpected payload: %s", payload)
	}
}

func TestZonesExecTransportSetNetworkSharedIsNoop(t *testing.T) {
	fake := &fakeExec{}
	tr := NewZonesExecTransport("wsd-ws-test", "pfexec", "shared", "")
	tr.run = fake.run

	if err := tr.SetNetwork(context.Background(), true); err != nil {
		t.Fatalf("SetNetwork: %v", err)
	}
	if err := tr.SetNetwork(context.Background(), false); err != nil {
		t.Fatalf("SetNetwork: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("shared SetNetwork made %d calls, want 0", len(fake.calls))
	}
}

func TestZonesExecTransportSetNetworkExclusive(t *testing.T) {
	fake := &fakeExec{}
	tr := NewZonesExecTransport("wsd-ws-test", "", "exclusive", "vnic0")
	tr.run = fake.run

	if err := tr.SetNetwork(context.Background(), false); err != nil {
		t.Fatalf("SetNetwork(false): %v", err)
	}
	if err := tr.SetNetwork(context.Background(), true); err != nil {
		t.Fatalf("SetNetwork(true): %v", err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(fake.calls))
	}
	if got := strings.Join(fake.calls[0].args, " "); got != "wsd-ws-test ifconfig vnic0 down" {
		t.Errorf("down call = %q", got)
	}
	if got := strings.Join(fake.calls[1].args, " "); got != "wsd-ws-test ifconfig vnic0 up" {
		t.Errorf("up call = %q", got)
	}
}

func TestZonesExecTransportReadFile(t *testing.T) {
	fake := &fakeExec{result: &internal.ExecResult{Stdout: "hello", ExitCode: 0}}
	tr := NewZonesExecTransport("wsd-ws-test", "", "shared", "")
	tr.run = fake.run

	got, err := tr.ReadFile(context.Background(), "/workspace/repo/file")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("ReadFile = %q, want hello", got)
	}
	if len(fake.calls) != 1 || fake.calls[0].name != "zlogin" {
		t.Fatalf("unexpected calls: %+v", fake.calls)
	}
	if got := strings.Join(fake.calls[0].args, " "); got != "wsd-ws-test cat /workspace/repo/file" {
		t.Errorf("args = %q", got)
	}
}

func TestZonesExecTransportWriteFile(t *testing.T) {
	fake := &fakeExec{}
	tr := NewZonesExecTransport("wsd-ws-test", "", "shared", "")
	tr.run = fake.run

	if err := tr.WriteFile(context.Background(), "/workspace/repo/file", []byte("a\nb\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var sawContent bool
	for _, c := range fake.calls {
		if strings.Contains(strings.Join(c.args, " "), "cat > /workspace/repo/file") {
			if c.stdin != "a\nb\n" {
				t.Errorf("stdin = %q, want a\\nb\\n", c.stdin)
			}
			sawContent = true
		}
	}
	if !sawContent {
		t.Error("WriteFile did not stream content to cat")
	}
}

func TestZonesExecTransportBootstrap(t *testing.T) {
	fake := &fakeExec{}
	tr := NewZonesExecTransport("wsd-ws-test", "", "shared", "")
	tr.run = fake.run

	if err := tr.Bootstrap(context.Background(), "/workspace/repo"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(fake.calls))
	}
	payload := fake.calls[0].args[len(fake.calls[0].args)-1]
	for _, want := range []string{"pkg install git", "safe.directory"} {
		if !strings.Contains(payload, want) {
			t.Errorf("bootstrap payload missing %q: %s", want, payload)
		}
	}
}

func TestZonesExecTransportExitCodeIsError(t *testing.T) {
	fake := &fakeExec{result: &internal.ExecResult{Stderr: "boom", ExitCode: 1}}
	tr := NewZonesExecTransport("wsd-ws-test", "", "shared", "")
	tr.run = fake.run

	if _, err := tr.ReadFile(context.Background(), "/missing"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("ReadFile error = %v, want stderr", err)
	}
}
