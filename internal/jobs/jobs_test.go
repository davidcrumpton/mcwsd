package jobs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
)

func entry(id, ws, session string, status internal.ProcessStatus) Entry {
	return Entry{
		ID:          id,
		WorkspaceID: ws,
		Command:     "echo hi",
		SessionID:   session,
		Status:      status,
		StartedAt:   time.Now().Truncate(time.Second),
	}
}

func TestStoreRoundTripPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "jobs.json")

	s1, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	e := entry("proc-1", "ws-1", "sess", internal.ProcessRunning)
	if err := s1.Put(e); err != nil {
		t.Fatalf("Put: %v", err)
	}

	s2, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore reload: %v", err)
	}
	got, ok := s2.Get("proc-1")
	if !ok {
		t.Fatal("entry missing after reload")
	}
	if got.WorkspaceID != "ws-1" || got.Status != internal.ProcessRunning {
		t.Fatalf("entry mismatch after reload: %+v", got)
	}
}

func TestStoreCorruptIsNotFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	if err := os.WriteFile(path, []byte("not json{{{"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("corrupt file should not be fatal: %v", err)
	}
	if len(s.All()) != 0 {
		t.Fatalf("expected empty store, got %d", len(s.All()))
	}
}

func TestListForWorkspace(t *testing.T) {
	s, _ := NewStore(filepath.Join(t.TempDir(), "jobs.json"))
	_ = s.Put(entry("proc-1", "ws-1", "s", internal.ProcessRunning))
	_ = s.Put(entry("proc-2", "ws-1", "s", internal.ProcessSucceeded))
	_ = s.Put(entry("proc-3", "ws-2", "s", internal.ProcessRunning))

	if got := len(s.ListForWorkspace("ws-1")); got != 2 {
		t.Fatalf("ListForWorkspace(ws-1) = %d, want 2", got)
	}
	if got := len(s.ListForWorkspace("ws-none")); got != 0 {
		t.Fatalf("ListForWorkspace(missing) = %d, want 0", got)
	}
}

func TestFinishedBeforeOnlyTerminalAndOld(t *testing.T) {
	s, _ := NewStore(filepath.Join(t.TempDir(), "jobs.json"))

	old := time.Now().Add(-48 * time.Hour)

	done := entry("proc-done", "ws", "s", internal.ProcessSucceeded)
	done.EndedAt = old
	_ = s.Put(done)

	running := entry("proc-running", "ws", "s", internal.ProcessRunning)
	running.StartedAt = old
	_ = s.Put(running)

	recent := entry("proc-recent", "ws", "s", internal.ProcessSucceeded)
	recent.EndedAt = time.Now().Add(-time.Minute)
	_ = s.Put(recent)

	got := s.FinishedBefore(time.Now().Add(-24 * time.Hour))
	if len(got) != 1 || got[0].ID != "proc-done" {
		t.Fatalf("FinishedBefore = %+v, want only proc-done", got)
	}
}

func TestRemoveMissingIsNoop(t *testing.T) {
	s, _ := NewStore(filepath.Join(t.TempDir(), "jobs.json"))
	if err := s.Remove("nope"); err != nil {
		t.Fatalf("Remove missing: %v", err)
	}
}

func TestTerminal(t *testing.T) {
	for _, s := range []internal.ProcessStatus{
		internal.ProcessSucceeded, internal.ProcessFailed,
		internal.ProcessCancelled, internal.ProcessTimedOut,
	} {
		if !Terminal(s) {
			t.Errorf("Terminal(%q) = false, want true", s)
		}
	}
	for _, s := range []internal.ProcessStatus{internal.ProcessQueued, internal.ProcessRunning} {
		if Terminal(s) {
			t.Errorf("Terminal(%q) = true, want false", s)
		}
	}
}
