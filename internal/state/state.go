// Package state persists a record of every container mcwsd creates, keyed by
// workspace ID, so containers left behind by a killed or aborted daemon
// process can be found and cleaned up later.
//
// The in-memory workspace map in the docker backend doesn't survive a
// process restart — this file is what does. An entry is written as soon as
// the container exists and before any of the slower provisioning steps
// (clone, install, post-create) that are most likely to get interrupted,
// and it's removed only once Destroy has actually removed the container.
// Anything left in the file whose session_id doesn't match the current
// daemon's session is, by definition, a leftover from a run that never
// went through a clean Destroy.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
)

// Entry is one tracked container.
type Entry struct {
	WorkspaceID   string    `json:"workspace_id"`
	ContainerID   string    `json:"container_id"`
	ContainerName string    `json:"container_name"`
	SessionID     string    `json:"session_id"`
	RepoURL       string    `json:"repo_url,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	// LastAccessedAt is updated on every use of the workspace (exec, file
	// ops, process ops, ...). It's what the pruner bases its retention on:
	// a workspace that hasn't been touched within the configured TTL is
	// considered abandoned and gets destroyed. Zero (e.g. entries written
	// by an older daemon version) falls back to CreatedAt.
	LastAccessedAt time.Time `json:"last_accessed_at,omitempty"`
	// WorkDir is the working directory inside the runtime where the workspace's
	// repo lives. Stored so workspaces can be recovered after a daemon restart.
	WorkDir string `json:"work_dir,omitempty"`
	// OS is the operating system of the provisioned runtime (e.g. "linux",
	// "freebsd", "openbsd"). Stored so workspaces can be recovered after a daemon restart.
	OS string `json:"os,omitempty"`
	// Ref is the git ref the workspace was cloned from. Persisted so a
	// recovered workspace still targets the right branch when opening a PR.
	Ref string `json:"ref,omitempty"`
	// EnvOverrides, Interface and IP4Address are the remainder of the
	// request-time workspace spec that must survive a restart for a recovered
	// workspace to behave exactly like the original.
	EnvOverrides map[string]string `json:"env_overrides,omitempty"`
	Interface    string            `json:"interface,omitempty"`
	IP4Address   string            `json:"ip4_address,omitempty"`
	// Profile and CacheHit are persisted so a recovered workspace reports the
	// same detected environment and cache status as the original.
	Profile  *internal.EnvironmentProfile `json:"profile,omitempty"`
	CacheHit bool                         `json:"cache_hit,omitempty"`

	// lastPersisted is when LastAccessedAt was last written to disk. It is
	// unexported so it is never serialized; it exists only to debounce
	// Touch's full-file rewrite on the hot polling path.
	lastPersisted time.Time
}

// Store is a JSON-file-backed, mutex-guarded map of workspace ID -> Entry.
// Every mutation is persisted immediately via an atomic write (temp file +
// rename) so a killed process never leaves the file mid-write or out of
// sync with what's actually on disk.
type Store struct {
	path    string
	mu      sync.Mutex
	entries map[string]Entry
}

// NewStore loads path if it exists, or starts empty if it doesn't exist
// yet. A corrupt or unreadable file is treated as empty rather than fatal
// — losing the leftover-tracking file shouldn't stop the daemon from
// starting, since the file's only job is cleanup convenience, not
// correctness of the daemon itself.
func NewStore(path string) (*Store, error) {
	s := &Store{
		path:    path,
		entries: make(map[string]Entry),
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create state dir: %w", err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("failed to read state file: %w", err)
	}
	if len(data) == 0 {
		return s, nil
	}

	var entries map[string]Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		// Corrupt tracking file. Start fresh rather than refusing to boot —
		// whatever was in there is still sitting on disk as orphaned
		// containers findable via `docker ps -a`, we just lose the
		// convenient index of them. Log it so the operator knows automatic
		// cleanup has been disabled.
		internal.Log.Warn("state_file_corrupt", "path", path, "error", err)
		return s, nil
	}
	s.entries = entries
	return s, nil
}

// Add records a newly created container. Call this as soon as the
// container exists, before any slower provisioning steps run, so a
// process killed mid-provision still leaves a record behind.
func (s *Store) Add(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[e.WorkspaceID] = e
	return s.saveLocked()
}

// Remove deletes the entry for workspaceID. Call this only after the
// container has actually been removed — if the docker remove call fails,
// leave the entry in place so it still shows up as a trackable leftover.
func (s *Store) Remove(workspaceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[workspaceID]; !ok {
		return nil
	}
	delete(s.entries, workspaceID)
	return s.saveLocked()
}

// Get returns the entry for workspaceID, if tracked.
func (s *Store) Get(workspaceID string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[workspaceID]
	return e, ok
}

// touchPersistInterval bounds how often Touch rewrites the whole state file.
// Clients poll job status frequently, and serializing the entire map on every
// poll is significant I/O and lock contention. The in-memory LastAccessedAt is
// always updated so pruning is accurate; only the durable copy is debounced.
const touchPersistInterval = 30 * time.Second

// Touch updates LastAccessedAt for workspaceID to now. Call this on every
// use of a workspace so the pruner can tell "actively in use" from
// "abandoned". Missing entries are a no-op (a leftover being cleaned up
// concurrently shouldn't error).
func (s *Store) Touch(workspaceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[workspaceID]
	if !ok {
		return nil
	}
	now := time.Now()
	e.LastAccessedAt = now
	persist := e.lastPersisted.IsZero() || now.Sub(e.lastPersisted) >= touchPersistInterval
	if persist {
		e.lastPersisted = now
	}
	s.entries[workspaceID] = e
	if !persist {
		return nil
	}
	return s.saveLocked()
}

// List returns a snapshot of every tracked entry.
func (s *Store) List() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e)
	}
	return out
}

// Orphans returns entries whose session_id isn't currentSessionID — i.e.
// containers created by a previous daemon run that were never destroyed,
// whether because the process was killed or because it exited without a
// clean shutdown of that particular workspace.
func (s *Store) Orphans(currentSessionID string) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, e := range s.entries {
		if e.SessionID != currentSessionID {
			out = append(out, e)
		}
	}
	return out
}

// Stale returns entries that haven't been accessed since cutoff — i.e.
// candidates for pruning. An entry's access time is LastAccessedAt, falling
// back to CreatedAt when the former is unset (entries written before the
// field existed).
func (s *Store) Stale(cutoff time.Time) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, e := range s.entries {
		last := e.LastAccessedAt
		if last.IsZero() {
			last = e.CreatedAt
		}
		if last.Before(cutoff) {
			out = append(out, e)
		}
	}
	return out
}

// saveLocked writes the current entries map to disk atomically. Caller
// must hold s.mu.
func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("failed to write temp state file: %w", err)
	}
	// WriteFile keeps the mode of a pre-existing file, so force the
	// restrictive mode in case a stale temp file was left behind.
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("failed to secure temp state file: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("failed to rename temp state file: %w", err)
	}
	return nil
}
