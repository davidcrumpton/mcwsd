package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// makeEntry is a test helper that returns a populated Entry.
func makeEntry(workspaceID, containerID, sessionID string) Entry {
	return Entry{
		WorkspaceID:   workspaceID,
		ContainerID:   containerID,
		ContainerName: "ctr-" + workspaceID,
		SessionID:     sessionID,
		RepoURL:       "https://example.com/repo.git",
		CreatedAt:     time.Now().Truncate(time.Second),
	}
}

func TestNewStore_NonExistentPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "state.json")

	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("NewStore returned nil store")
	}
	if len(s.List()) != 0 {
		t.Errorf("expected empty store, got %d entries", len(s.List()))
	}
}

func TestNewStore_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore on empty file: %v", err)
	}
	if len(s.List()) != 0 {
		t.Errorf("expected empty store, got %d entries", len(s.List()))
	}
}

func TestNewStore_CorruptJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("not valid json{{{{"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Corrupt JSON must NOT be fatal — the store starts empty.
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore with corrupt JSON: want nil error, got %v", err)
	}
	if len(s.List()) != 0 {
		t.Errorf("expected empty store after corrupt load, got %d entries", len(s.List()))
	}
}

func TestAddGetRemove_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	e := makeEntry("ws-001", "ctr-abc123", "sess-1")
	if err := s.Add(e); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, ok := s.Get("ws-001")
	if !ok {
		t.Fatal("Get: entry not found after Add")
	}
	if got.ContainerID != "ctr-abc123" {
		t.Errorf("Get ContainerID: want %q, got %q", "ctr-abc123", got.ContainerID)
	}

	if err := s.Remove("ws-001"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := s.Get("ws-001"); ok {
		t.Error("Get: entry still present after Remove")
	}
}

func TestRemove_NonExistent(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Removing a nonexistent key should be a no-op, not an error.
	if err := s.Remove("does-not-exist"); err != nil {
		t.Errorf("Remove nonexistent: want nil error, got %v", err)
	}
}

func TestList_Snapshot(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"ws-1", "ws-2", "ws-3"} {
		if err := s.Add(makeEntry(id, "ctr-"+id, "sess")); err != nil {
			t.Fatal(err)
		}
	}

	list := s.List()
	if len(list) != 3 {
		t.Errorf("List: want 3 entries, got %d", len(list))
	}
}

func TestOrphans(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	currentSession := "sess-current"

	// Two entries from a previous session.
	_ = s.Add(makeEntry("ws-old-1", "ctr-old-1", "sess-old"))
	_ = s.Add(makeEntry("ws-old-2", "ctr-old-2", "sess-old"))
	// One entry from the current session.
	_ = s.Add(makeEntry("ws-cur", "ctr-cur", currentSession))

	orphans := s.Orphans(currentSession)
	if len(orphans) != 2 {
		t.Errorf("Orphans: want 2, got %d", len(orphans))
	}
	for _, o := range orphans {
		if o.SessionID == currentSession {
			t.Errorf("Orphans returned current-session entry %q", o.WorkspaceID)
		}
	}
}

func TestOrphans_AllCurrent(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Add(makeEntry("ws-1", "ctr-1", "sess-a"))
	if orphans := s.Orphans("sess-a"); len(orphans) != 0 {
		t.Errorf("Orphans: want 0, got %d", len(orphans))
	}
}

func TestTouch_UpdatesLastAccessedAt(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	e := makeEntry("ws-touch", "ctr-touch", "sess")
	old := time.Now().Add(-time.Hour)
	e.LastAccessedAt = old
	if err := s.Add(e); err != nil {
		t.Fatal(err)
	}

	if err := s.Touch("ws-touch"); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	got, _ := s.Get("ws-touch")
	if !got.LastAccessedAt.After(old) {
		t.Errorf("Touch: want LastAccessedAt after %v, got %v", old, got.LastAccessedAt)
	}
}

func TestTouch_NonExistentIsNoop(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Touch("does-not-exist"); err != nil {
		t.Errorf("Touch nonexistent: want nil error, got %v", err)
	}
}

func TestTouch_PersistsToDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s1, _ := NewStore(path)
	_ = s1.Add(makeEntry("ws-p", "ctr-p", "s"))
	_ = s1.Touch("ws-p")

	s2, _ := NewStore(path)
	got, ok := s2.Get("ws-p")
	if !ok {
		t.Fatal("entry missing after reload")
	}
	if got.LastAccessedAt.IsZero() {
		t.Error("LastAccessedAt lost after reload")
	}
}

func TestStale_FallsBackToCreatedAt(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Entries with no LastAccessedAt (as an older daemon would have
	// written) are judged by CreatedAt.
	fresh := makeEntry("ws-fresh", "ctr-fresh", "s")
	fresh.CreatedAt = time.Now().Add(-time.Minute)
	_ = s.Add(fresh)

	stale := makeEntry("ws-stale", "ctr-stale", "s")
	stale.CreatedAt = time.Now().Add(-48 * time.Hour)
	_ = s.Add(stale)

	cutoff := time.Now().Add(-24 * time.Hour)
	staleIDs := map[string]bool{}
	for _, e := range s.Stale(cutoff) {
		staleIDs[e.WorkspaceID] = true
	}

	if !staleIDs["ws-stale"] {
		t.Error("Stale: ws-stale (48h old) should be stale")
	}
	if staleIDs["ws-fresh"] {
		t.Error("Stale: ws-fresh (1m old) should not be stale")
	}
}

func TestStale_UsesLastAccessedAt(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Old creation but recently accessed: not stale.
	old := makeEntry("ws-old-but-live", "ctr-live", "s")
	old.CreatedAt = time.Now().Add(-48 * time.Hour)
	old.LastAccessedAt = time.Now().Add(-time.Minute)
	_ = s.Add(old)

	// Recently created but never re-accessed since creation: stale when
	// LastAccessedAt is older than the cutoff.
	recent := makeEntry("ws-new-but-idle", "ctr-idle", "s")
	recent.CreatedAt = time.Now().Add(-time.Hour)
	recent.LastAccessedAt = time.Now().Add(-48 * time.Hour)
	_ = s.Add(recent)

	cutoff := time.Now().Add(-24 * time.Hour)
	staleIDs := map[string]bool{}
	for _, e := range s.Stale(cutoff) {
		staleIDs[e.WorkspaceID] = true
	}

	if staleIDs["ws-old-but-live"] {
		t.Error("Stale: recently-accessed workspace should not be stale")
	}
	if !staleIDs["ws-new-but-idle"] {
		t.Error("Stale: workspace idle since before cutoff should be stale")
	}
}

func TestPersistence_ReloadFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// Write an entry with the first store instance.
	s1, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	e := makeEntry("ws-persist", "ctr-persist", "sess-x")
	if err := s1.Add(e); err != nil {
		t.Fatal(err)
	}

	// Open the same file with a second store instance — entry must survive.
	s2, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore (second open): %v", err)
	}
	got, ok := s2.Get("ws-persist")
	if !ok {
		t.Fatal("entry not found after reload from disk")
	}
	if got.ContainerID != "ctr-persist" {
		t.Errorf("ContainerID after reload: want %q, got %q", "ctr-persist", got.ContainerID)
	}
}

func TestPersistence_RemovePersistedOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s1, _ := NewStore(path)
	_ = s1.Add(makeEntry("ws-a", "ctr-a", "s"))
	_ = s1.Add(makeEntry("ws-b", "ctr-b", "s"))
	_ = s1.Remove("ws-a")

	s2, _ := NewStore(path)
	if _, ok := s2.Get("ws-a"); ok {
		t.Error("removed entry ws-a still appears after reload")
	}
	if _, ok := s2.Get("ws-b"); !ok {
		t.Error("remaining entry ws-b missing after reload")
	}
}

func TestPIDLockExcludesSecondOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json.lock")

	lock, err := AcquirePIDLock(path)
	if err != nil {
		t.Fatalf("AcquirePIDLock: %v", err)
	}

	// A second acquisition (as cleanup/status would do) must refuse while the
	// first is held by this live process.
	if _, err := AcquirePIDLock(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second AcquirePIDLock error = %v, want ErrLocked", err)
	}

	if err := lock.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// After release the path can be acquired again.
	again, err := AcquirePIDLock(path)
	if err != nil {
		t.Fatalf("AcquirePIDLock after close: %v", err)
	}
	again.Close()
}

func TestPIDLockReclaimsStaleFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json.lock")
	// A pid that cannot be alive (above the usual max).
	if err := os.WriteFile(path, []byte("999999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquirePIDLock(path)
	if err != nil {
		t.Fatalf("AcquirePIDLock should reclaim a stale lock: %v", err)
	}
	lock.Close()
}
