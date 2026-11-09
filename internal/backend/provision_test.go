package backend

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/cache"
	"github.com/davidcrumpton/mcwsd/internal/gitpush"
	"github.com/davidcrumpton/mcwsd/internal/jobs"
	"github.com/davidcrumpton/mcwsd/internal/runtime"
	"github.com/davidcrumpton/mcwsd/internal/state"
)

// provisionStub is a runtime.Provisioner that hands back a localTransport
// rooted at a temp directory, so the provisioning pipeline can be exercised
// end to end without a container runtime.
type provisionStub struct {
	root      string
	isolation runtime.IsolationType
}

func (p *provisionStub) Name() string               { return "provision-stub" }
func (p *provisionStub) Init(context.Context) error { return nil }

func (p *provisionStub) Provision(_ context.Context, spec runtime.ProvisionSpec) (*runtime.RuntimeHandle, error) {
	workDir := filepath.Join(p.root, "repo")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	isolation := p.isolation
	if isolation == "" {
		isolation = runtime.IsolationContainer
	}
	return &runtime.RuntimeHandle{
		OS:        runtime.OSLinux,
		Isolation: isolation,
		WorkDir:   workDir,
		NativeID:  "stub-" + spec.WorkspaceID,
		Transport: &localTransport{root: p.root},
	}, nil
}

func (p *provisionStub) Destroy(context.Context, *runtime.RuntimeHandle) error { return nil }
func (p *provisionStub) DestroyByID(context.Context, string) error             { return nil }

func newProvisionTestBackend(t *testing.T) *Backend {
	t.Helper()
	root := t.TempDir()

	store, err := state.NewStore(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatalf("state store: %v", err)
	}
	cacheMgr, err := cache.NewManager(filepath.Join(root, "cache"), 1)
	if err != nil {
		t.Fatalf("cache manager: %v", err)
	}

	return &Backend{
		provisioner:   &provisionStub{root: root},
		cacheManager:  cacheMgr,
		workspaces:    make(map[string]*workspaceState),
		processes:     make(map[string]*managedProcess),
		provisionJobs: make(map[string]*provisionJob),
		stateStore:    store,
		// Forge auth set so a blank workspace is admitted regardless of URL.
		gitPushConfig: gitpush.Config{AuthMode: "token"},
		sessionID:     "test-session",
	}
}

func TestProvisionSyncReturnsWorkspace(t *testing.T) {
	b := newProvisionTestBackend(t)

	res, err := b.Provision(context.Background(), internal.ProvisionRequest{})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.JobID != "" {
		t.Fatalf("synchronous provision should not return a job id: %+v", res)
	}
	if res.Workspace == nil || res.Workspace.ID == "" {
		t.Fatalf("synchronous provision should return a workspace: %+v", res)
	}
	if _, err := b.ListFiles(context.Background(), res.Workspace, "."); err != nil {
		t.Fatalf("provisioned workspace not usable: %v", err)
	}
}

func TestProvisionAsyncPollsToWorkspace(t *testing.T) {
	b := newProvisionTestBackend(t)
	ctx := context.Background()

	res, err := b.Provision(ctx, internal.ProvisionRequest{Async: true})
	if err != nil {
		t.Fatalf("Provision(async): %v", err)
	}
	if res.JobID == "" {
		t.Fatalf("async provision should return a job id: %+v", res)
	}
	if res.WorkspaceID == "" {
		t.Fatal("async provision should reserve a workspace id")
	}

	ws := &internal.Workspace{ID: res.WorkspaceID}
	var status *internal.ProcessStatusResult
	for i := 0; i < 250; i++ {
		status, err = b.GetProcessStatus(ctx, ws, res.JobID, "")
		if err != nil {
			t.Fatalf("GetProcessStatus: %v", err)
		}
		if isTerminalProcessStatus(status.Status) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if status == nil || status.Status != internal.ProcessSucceeded {
		t.Fatalf("provision job did not succeed: %+v", status)
	}
	if status.Workspace == nil {
		t.Fatal("terminal provision status should carry the finished workspace")
	}
	if status.Workspace.ID != res.WorkspaceID {
		t.Fatalf("workspace id = %q, want %q", status.Workspace.ID, res.WorkspaceID)
	}
	if _, err := b.ListFiles(ctx, status.Workspace, "."); err != nil {
		t.Fatalf("provisioned workspace not usable: %v", err)
	}
}

// TestProvisionJobPersisted verifies a provision job is written to the
// host-side store with the provision kind so it can be told apart from shell
// jobs (and correctly interrupted on restart).
func TestProvisionJobPersisted(t *testing.T) {
	b := newProvisionTestBackend(t)
	store, err := jobs.NewStore(filepath.Join(t.TempDir(), "jobs.json"))
	if err != nil {
		t.Fatalf("jobs store: %v", err)
	}
	b.jobStore = store

	res, err := b.Provision(context.Background(), internal.ProvisionRequest{Async: true})
	if err != nil {
		t.Fatalf("Provision(async): %v", err)
	}

	// Wait for the job to finish so its goroutine stops touching the temp dir.
	ws := &internal.Workspace{ID: res.WorkspaceID}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, err := b.GetProcessStatus(context.Background(), ws, res.JobID, "")
		if err != nil {
			t.Fatalf("GetProcessStatus: %v", err)
		}
		if isTerminalProcessStatus(status.Status) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("provision job did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}

	e, ok := store.Get(res.JobID)
	if !ok {
		t.Fatalf("provision job %s not persisted", res.JobID)
	}
	if e.Kind != jobs.KindProvision {
		t.Fatalf("job kind = %q, want %q", e.Kind, jobs.KindProvision)
	}
	if e.WorkspaceID != res.WorkspaceID {
		t.Fatalf("job workspace = %q, want %q", e.WorkspaceID, res.WorkspaceID)
	}
}

func TestProvisionBlanksBaseImageForNonContainerRuntime(t *testing.T) {
	root := t.TempDir()

	store, err := state.NewStore(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatalf("state store: %v", err)
	}
	cacheMgr, err := cache.NewManager(filepath.Join(root, "cache"), 1)
	if err != nil {
		t.Fatalf("cache manager: %v", err)
	}

	b := &Backend{
		provisioner:   &provisionStub{root: root, isolation: runtime.IsolationJail},
		cacheManager:  cacheMgr,
		workspaces:    make(map[string]*workspaceState),
		processes:     make(map[string]*managedProcess),
		provisionJobs: make(map[string]*provisionJob),
		stateStore:    store,
		gitPushConfig: gitpush.Config{AuthMode: "token"},
		sessionID:     "test-session",
	}

	res, err := b.Provision(context.Background(), internal.ProvisionRequest{})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.Workspace == nil {
		t.Fatal("provision should return a workspace")
	}
	if res.Workspace.Profile == nil {
		t.Fatal("workspace should carry a profile")
	}
	if res.Workspace.Profile.BaseImage != "" {
		t.Fatalf("BaseImage = %q, want empty for a non-container runtime", res.Workspace.Profile.BaseImage)
	}
}

func TestMaxWorkspacesLimit(t *testing.T) {
	b := newProvisionTestBackend(t)
	b.WithMaxWorkspaces(1)

	first, err := b.Provision(context.Background(), internal.ProvisionRequest{})
	if err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	if first.Workspace == nil {
		t.Fatal("first provision should return a workspace")
	}

	if _, err := b.Provision(context.Background(), internal.ProvisionRequest{}); err == nil {
		t.Fatal("second Provision under limit of 1 = nil, want error")
	}

	if err := b.Destroy(context.Background(), first.Workspace); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	second, err := b.Provision(context.Background(), internal.ProvisionRequest{})
	if err != nil {
		t.Fatalf("Provision after destroy: %v", err)
	}
	if second.Workspace == nil {
		t.Fatal("provision after destroy should return a workspace")
	}
}
