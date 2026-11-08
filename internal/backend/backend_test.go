package backend

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
	"github.com/davidcrumpton/mcwsd/internal/state"
)

type workspaceIDDestroyerStub struct {
	workspaceID string
	calls       int
}

func (p *workspaceIDDestroyerStub) Name() string               { return "workspace-id-destroyer-stub" }
func (p *workspaceIDDestroyerStub) Init(context.Context) error { return nil }
func (p *workspaceIDDestroyerStub) Provision(context.Context, runtime.ProvisionSpec) (*runtime.RuntimeHandle, error) {
	return nil, nil
}
func (p *workspaceIDDestroyerStub) Destroy(context.Context, *runtime.RuntimeHandle) error { return nil }
func (p *workspaceIDDestroyerStub) DestroyByID(context.Context, string) error             { return nil }
func (p *workspaceIDDestroyerStub) DestroyWorkspaceByID(_ context.Context, workspaceID string) error {
	p.workspaceID = workspaceID
	p.calls++
	return nil
}

func TestDestroyUntrackedWorkspaceUsesWorkspaceIDDestroyer(t *testing.T) {
	root := t.TempDir()
	store, err := state.NewStore(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	provisioner := &workspaceIDDestroyerStub{}
	b := &Backend{
		provisioner: provisioner,
		workspaces:  make(map[string]*workspaceState),
		stateStore:  store,
		processes:   make(map[string]*managedProcess),
	}

	if err := b.Destroy(context.Background(), &internal.Workspace{ID: "ws-abc123"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if provisioner.calls != 1 || provisioner.workspaceID != "ws-abc123" {
		t.Fatalf("DestroyWorkspaceByID called %d times with %q, want once with ws-abc123", provisioner.calls, provisioner.workspaceID)
	}
}

func TestSystemPackageCommands(t *testing.T) {
	tests := []struct {
		name        string
		os          runtime.OS
		baseImage   string
		wantManager string
		wantInstall string
	}{
		{name: "freebsd jail", os: runtime.OSFreeBSD, wantManager: "pkg", wantInstall: "pkg install -y"},
		{name: "illumos zone", os: runtime.OSIllumos, wantManager: "pkg", wantInstall: "pkg install"},
		{name: "openindiana zone", os: runtime.OSOpenIndiana, wantManager: "pkg", wantInstall: "pkg install"},
		{name: "openbsd", os: runtime.OSOpenBSD, wantManager: "pkg_add", wantInstall: "pkg_add"},
		{name: "netbsd", os: runtime.OSNetBSD, wantManager: "pkg_add", wantInstall: "pkg_add"},
		{name: "debian container", os: runtime.OSLinux, baseImage: "docker.io/library/golang:1.21", wantManager: "apt", wantInstall: "apt-get install -y"},
		{name: "alpine container", os: runtime.OSLinux, baseImage: "docker.io/library/alpine:3.20", wantManager: "apk", wantInstall: "apk add"},
		{name: "unknown os", os: runtime.OS("plan9"), wantManager: "", wantInstall: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotManager, gotInstall := systemPackageCommands(tt.os, tt.baseImage)
			if gotManager != tt.wantManager || gotInstall != tt.wantInstall {
				t.Fatalf("systemPackageCommands(%q, %q) = (%q, %q), want (%q, %q)",
					tt.os, tt.baseImage, gotManager, gotInstall, tt.wantManager, tt.wantInstall)
			}
		})
	}
}

func TestResolveWorkspacePath(t *testing.T) {
	const workDir = "/workspace/repo"

	tests := []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		{name: "bare filename", path: "test_run.sh", want: "/workspace/repo/test_run.sh"},
		{name: "nested relative", path: "cmd/mcwsd/main.go", want: "/workspace/repo/cmd/mcwsd/main.go"},
		{name: "dot", path: ".", want: "/workspace/repo"},
		{name: "internal parent stays inside", path: "cmd/../test_run.sh", want: "/workspace/repo/test_run.sh"},
		{name: "absolute inside", path: "/workspace/repo/test_run.sh", want: "/workspace/repo/test_run.sh"},
		{name: "absolute equal to root", path: "/workspace/repo", want: "/workspace/repo"},
		{name: "escapes to runtime root", path: "../../test_run.sh", wantErr: true},
		{name: "escapes one level", path: "../test_run.sh", wantErr: true},
		{name: "absolute outside", path: "/etc/passwd", wantErr: true},
		{name: "empty", path: "", wantErr: true},
		{name: "whitespace", path: "   ", wantErr: true},

		// Models commonly treat the workspace base directory as the repo
		// root; absolute paths at or under it are re-rooted into the repo.
		{name: "base dir alias", path: "/workspace", want: "/workspace/repo"},
		{name: "base dir child", path: "/workspace/main.go", want: "/workspace/repo/main.go"},
		{name: "base dir nested", path: "/workspace/src/x", want: "/workspace/repo/src/x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveWorkspacePath(workDir, tt.path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveWorkspacePath(%q, %q) = %q, want error", workDir, tt.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveWorkspacePath(%q, %q) unexpected error: %v", workDir, tt.path, err)
			}
			if got != tt.want {
				t.Fatalf("resolveWorkspacePath(%q, %q) = %q, want %q", workDir, tt.path, got, tt.want)
			}
		})
	}
}

// TestBuildListFilesCommand guards portability: OpenBSD's find(1) has no
// -printf, so the listing command must not use it. It must use the portable
// -exec ... {} + form and emit a tab-separated type/size/name line.
func TestBuildListFilesCommand(t *testing.T) {
	cmd := buildListFilesCommand("/workspace/repo")

	if strings.Contains(cmd, "-printf") {
		t.Fatalf("listing command must not use find -printf (unsupported on OpenBSD): %s", cmd)
	}
	for _, want := range []string{"find", "-maxdepth 1", "-mindepth 1", "-exec", "{} +"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("listing command missing %q: %s", want, cmd)
		}
	}
	// The output format is a tab-separated triple; Sprintf must have collapsed
	// the escaped percent signs.
	if !strings.Contains(cmd, `printf "%s\t%s\t%s\n"`) {
		t.Errorf("listing command has wrong printf format: %s", cmd)
	}
	// The directory must be shell-quoted as a single argument.
	if !strings.Contains(cmd, `find '/workspace/repo'`) {
		t.Errorf("listing command did not quote the directory: %s", cmd)
	}
}

func TestIsPublicRepo(t *testing.T) {
	tests := []struct {
		name    string
		repoURL string
		want    bool
	}{
		{name: "GitHub public repo", repoURL: "https://github.com/user/repo.git", want: true},
		{name: "GitLab public repo", repoURL: "https://gitlab.com/user/repo.git", want: true},
		{name: "Bitbucket public repo", repoURL: "https://bitbucket.org/user/repo.git", want: true},
		{name: "HTTP public repo", repoURL: "http://example.com/user/repo.git", want: true},
		{name: "HTTPS public repo", repoURL: "https://example.com/user/repo.git", want: true},
		{name: "Local Repo HTTP", repoURL: "http://localhost/repo.git", want: true},
		{name: "SSH private repo", repoURL: "git@github.com:user/repo.git", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPublicRepo(tt.repoURL); got != tt.want {
				t.Errorf("isPublicRepo(%q) = %v, want %v", tt.repoURL, got, tt.want)
			}
		})
	}
}
