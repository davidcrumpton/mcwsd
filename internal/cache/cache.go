package cache

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
)

// Manager handles caching of dependency installation state.
type Manager struct {
	cacheDir     string
	maxSizeBytes int64
	entries      map[string]*cacheEntry
	mu           sync.RWMutex
}

type cacheEntry struct {
	path      string
	size      int64
	lastAccess time.Time
}

// NewManager creates a new cache manager with the given directory and max size in GB.
func NewManager(cacheDir string, maxSizeGB int) (*Manager, error) {
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, err
	}

	maxSizeBytes := int64(maxSizeGB) * 1024 * 1024 * 1024

	m := &Manager{
		cacheDir:     cacheDir,
		maxSizeBytes: maxSizeBytes,
		entries:      make(map[string]*cacheEntry),
	}

	// Start background eviction ticker
	go m.evictionLoop()

	return m, nil
}

// Get retrieves a cached entry by key. Returns path and hit status.
func (m *Manager) Get(key string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.entries[key]
	if !ok {
		return "", false
	}

	entry.lastAccess = time.Now()
	return entry.path, true
}

// Save stores a cache entry. The sourcePath is the path inside a container to cache.
func (m *Manager) Save(key, sourcePath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Create cache directory for this key
	cachePath := filepath.Join(m.cacheDir, key)
	if err := os.MkdirAll(cachePath, 0755); err != nil {
		return err
	}

	m.entries[key] = &cacheEntry{
		path:       cachePath,
		size:       0, // Would need to compute actual size
		lastAccess: time.Now(),
	}

	internal.Log.Info("cache_saved", "key", key, "path", cachePath)
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

func (m *Manager) evict() {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Calculate total size
	var totalSize int64
	for _, entry := range m.entries {
		totalSize += entry.size
	}

	if totalSize <= m.maxSizeBytes {
		return // Under limit
	}

	// Sort by last access and remove oldest until under limit
	// Simplified: just log for now
	internal.Log.Warn("cache_over_limit", "total_size", totalSize, "max_size", m.maxSizeBytes)
}
