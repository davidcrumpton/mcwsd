package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewManager_CreatesCacheDir(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache", "nested")

	m, err := NewManager(cacheDir, 1)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if m == nil {
		t.Fatal("NewManager returned nil")
	}

	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		t.Errorf("cache dir %q was not created", cacheDir)
	}
}

func TestGet_MissingKey(t *testing.T) {
	m, err := NewManager(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}

	path, hit := m.Get("nonexistent-key")
	if hit {
		t.Error("Get: expected cache miss, got hit")
	}
	if path != "" {
		t.Errorf("Get: expected empty path on miss, got %q", path)
	}
}

func TestSave_ThenGet_Hit(t *testing.T) {
	cacheDir := t.TempDir()
	m, err := NewManager(cacheDir, 1)
	if err != nil {
		t.Fatal(err)
	}

	const key = "go-mod-abc123"
	if err := m.Save(key, "/some/container/path"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	path, hit := m.Get(key)
	if !hit {
		t.Fatal("Get after Save: expected cache hit, got miss")
	}
	want := filepath.Join(cacheDir, key)
	if path != want {
		t.Errorf("Get path: want %q, got %q", want, path)
	}
}

func TestSave_CreatesCacheSubDir(t *testing.T) {
	cacheDir := t.TempDir()
	m, err := NewManager(cacheDir, 1)
	if err != nil {
		t.Fatal(err)
	}

	const key = "rust-cargo-xyz"
	if err := m.Save(key, "/container/path"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	expected := filepath.Join(cacheDir, key)
	if info, err := os.Stat(expected); err != nil {
		t.Errorf("expected subdirectory %q to exist after Save: %v", expected, err)
	} else if !info.IsDir() {
		t.Errorf("expected %q to be a directory", expected)
	}
}

func TestGet_UpdatesLastAccess(t *testing.T) {
	m, err := NewManager(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}

	const key = "node-npm-abc"
	if err := m.Save(key, "/path"); err != nil {
		t.Fatal(err)
	}

	// Capture lastAccess right after Save.
	m.mu.Lock()
	firstAccess := m.entries[key].lastAccess
	m.mu.Unlock()

	// Tiny sleep to ensure a measurable time difference.
	time.Sleep(2 * time.Millisecond)

	m.Get(key)

	m.mu.Lock()
	secondAccess := m.entries[key].lastAccess
	m.mu.Unlock()

	if !secondAccess.After(firstAccess) {
		t.Errorf("lastAccess was not updated by Get: first=%v second=%v", firstAccess, secondAccess)
	}
}

func TestSave_Idempotent(t *testing.T) {
	m, err := NewManager(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}

	const key = "idempotent-key"
	if err := m.Save(key, "/a"); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := m.Save(key, "/b"); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	_, hit := m.Get(key)
	if !hit {
		t.Error("expected hit after two Saves with same key")
	}
}

func TestEvict_UnderLimit(t *testing.T) {
	m, err := NewManager(t.TempDir(), 1) // 1 GB limit — well over any test data
	if err != nil {
		t.Fatal(err)
	}
	_ = m.Save("k1", "/p1")
	_ = m.Save("k2", "/p2")

	// evict should not remove anything when under the limit.
	m.evict()

	if _, ok := m.Get("k1"); !ok {
		t.Error("evict removed k1 even though we are under the size limit")
	}
	if _, ok := m.Get("k2"); !ok {
		t.Error("evict removed k2 even though we are under the size limit")
	}
}
