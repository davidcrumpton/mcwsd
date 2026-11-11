package cache

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
)

// Manager handles caching of dependency installation state. A cache entry is
// a real copy of the source directory on the host; an entry is only reported
// as a hit while its directory exists and contains at least one file, so a
// half-created or empty cache can never make a workspace skip its install.
type Manager struct {
	cacheDir     string
	maxSizeBytes int64
	entries      map[string]*cacheEntry
	mu           sync.RWMutex
}

type cacheEntry struct {
	path       string
	size       int64
	lastAccess time.Time
}

// NewManager creates a new cache manager with the given directory and max size in GB.
func NewManager(cacheDir string, maxSizeGB int) (*Manager, error) {
	maxSizeBytes := int64(maxSizeGB) * 1024 * 1024 * 1024

	m := &Manager{
		cacheDir:     cacheDir,
		maxSizeBytes: maxSizeBytes,
		entries:      make(map[string]*cacheEntry),
	}

	// An empty cache directory disables caching. This is used by the cleanup
	// command, which constructs a backend for lifecycle-only work and never
	// caches; skipping MkdirAll("") and the eviction loop keeps it working
	// without a cache.
	if cacheDir == "" {
		return m, nil
	}

	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, err
	}

	// Start background eviction ticker
	go m.evictionLoop()

	return m, nil
}

// Get retrieves a cached entry by key. Returns path and hit status. A missing
// or emptied cache directory is a miss, not a hit against nothing.
func (m *Manager) Get(key string) (string, bool) {
	if m.cacheDir == "" {
		return "", false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.entries[key]
	if !ok {
		return "", false
	}
	size, err := dirSize(entry.path)
	if err != nil || size == 0 {
		delete(m.entries, key)
		return "", false
	}
	entry.size = size
	entry.lastAccess = time.Now()
	return entry.path, true
}

// Save copies sourcePath into the cache under key and records its size. An
// unreadable or empty source is an error and leaves the previous entry (if
// any) untouched, so callers never advertise a cache entry that contains no
// data.
func (m *Manager) Save(key, sourcePath string) error {
	if m.cacheDir == "" {
		return nil
	}
	if key == "" || key == "." || key == ".." || strings.ContainsAny(key, `/\`) {
		return fmt.Errorf("invalid cache key %q", key)
	}

	info, err := os.Stat(sourcePath)
	if err != nil {
		return fmt.Errorf("cache source %q: %w", sourcePath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cache source %q is not a directory", sourcePath)
	}

	cachePath := filepath.Join(m.cacheDir, key)
	tmpPath, err := os.MkdirTemp(m.cacheDir, ".tmp-"+key+"-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpPath)

	if err := copyTree(sourcePath, tmpPath); err != nil {
		return fmt.Errorf("copy %q into cache: %w", sourcePath, err)
	}
	size, err := dirSize(tmpPath)
	if err != nil {
		return err
	}
	if size == 0 {
		return fmt.Errorf("cache source %q is empty", sourcePath)
	}

	// Replace any previous entry atomically.
	if err := os.RemoveAll(cachePath); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, cachePath); err != nil {
		return err
	}

	m.mu.Lock()
	m.entries[key] = &cacheEntry{
		path:       cachePath,
		size:       size,
		lastAccess: time.Now(),
	}
	m.mu.Unlock()

	internal.Log.Info("cache_saved", "key", key, "path", cachePath, "size", size)
	return nil
}

// evictionLoop runs periodically to evict old entries when over capacity.
func (m *Manager) evictionLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		m.evict()
	}
}

// evict removes least-recently-used entries until the cache is under its size
// limit. A non-positive limit means unlimited.
func (m *Manager) evict() {
	if m.cacheDir == "" || m.maxSizeBytes <= 0 {
		return
	}

	type candidate struct {
		key        string
		size       int64
		lastAccess time.Time
	}

	m.mu.RLock()
	candidates := make([]candidate, 0, len(m.entries))
	var total int64
	for key, entry := range m.entries {
		candidates = append(candidates, candidate{key: key, size: entry.size, lastAccess: entry.lastAccess})
		total += entry.size
	}
	m.mu.RUnlock()

	if total <= m.maxSizeBytes {
		return
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].lastAccess.Before(candidates[j].lastAccess)
	})

	for _, c := range candidates {
		if total <= m.maxSizeBytes {
			break
		}
		m.mu.Lock()
		entry, ok := m.entries[c.key]
		if !ok {
			m.mu.Unlock()
			continue
		}
		delete(m.entries, c.key)
		path := entry.path
		size := entry.size
		m.mu.Unlock()

		if err := os.RemoveAll(path); err != nil {
			internal.Log.Warn("cache_evict_failed", "key", c.key, "path", path, "error", err)
			continue
		}
		total -= size
		internal.Log.Info("cache_evicted", "key", c.key, "size", size)
	}
}

// copyTree copies a directory tree, preserving file modes and symlinks and
// skipping special files (sockets, fifos, devices).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyFile(path, target, info.Mode().Perm())
		default:
			return nil
		}
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// dirSize returns the total size of regular files under root.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}
