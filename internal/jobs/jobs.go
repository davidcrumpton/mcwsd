// Package jobs persists a record of every background job the daemon starts,
// independent of the in-memory process table, so jobs left behind by a killed
// or restarted daemon can be reconciled later.
//
// It is deliberately modeled on internal/state: a JSON-file-backed,
// mutex-guarded map with atomic (temp file + rename) writes, keyed by job ID
// and stamped with a per-daemon-run session ID. The state store answers
// "which containers exist"; this store answers "which jobs exist, how did they
// end, and what was the tail of their output".
package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
)

// Entry is one tracked background job. It is both the on-disk record and the
// shape used for recovery.
type Entry struct {
	ID           string                 `json:"id"`
	WorkspaceID  string                 `json:"workspace_id"`
	Command      string                 `json:"command"`
	SessionID    string                 `json:"session_id"`
	Status       internal.ProcessStatus `json:"status"`
	ExitCode     int                    `json:"exit_code,omitempty"`
	AllowNetwork bool                   `json:"allow_network,omitempty"`
	// Kind distinguishes job types. Empty (or "exec") is a shell command run
	// inside a workspace runtime; KindProvision is a daemon-side provisioning
	// pipeline that is not backed by an in-runtime process and therefore
	// cannot be probed or recovered after a restart.
	Kind string `json:"kind,omitempty"`
	// Foreground marks a job promoted from a synchronous Exec; such jobs do
	// not count against the background job limits across a restart.
	Foreground     bool      `json:"foreground,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	EndedAt        time.Time `json:"ended_at,omitempty"`
	TimeoutSeconds int       `json:"timeout_seconds,omitempty"`
	// LogTail is a capped copy of the job's combined log, captured host-side
	// when the job reaches a terminal state (or when its workspace is torn
	// down) so the result survives the runtime being destroyed.
	LogTail string `json:"log_tail,omitempty"`
}

// KindProvision marks a job that runs the daemon-side provisioning pipeline
// rather than a shell command inside a workspace runtime.
const KindProvision = "provision"

// Limits bounds background jobs daemon-wide. A zero value disables the
// corresponding bound except MaxPerWorkspace, whose default is applied by the
// caller (main.go) rather than here.
type Limits struct {
	// MaxPerWorkspace is the maximum number of concurrently active
	// (queued or running) jobs per workspace. 0 means unlimited.
	MaxPerWorkspace int
	// MaxTotal is the maximum number of concurrently active jobs across the
	// whole daemon. 0 means unlimited.
	MaxTotal int
	// MaxRuntime caps how long any job may run; 0 means no daemon-level cap
	// (a per-request timeout still applies).
	MaxRuntime time.Duration
	// MaxLogBytes caps each job's stdout and stderr streams inside the
	// runtime. 0 means unlimited.
	MaxLogBytes int64
	// Retention is how long a finished job's record and log tail are kept
	// after it ends. 0 means keep until the store is otherwise cleaned.
	Retention time.Duration
}

// Terminal reports whether a job status is final.
func Terminal(s internal.ProcessStatus) bool {
	switch s {
	case internal.ProcessSucceeded, internal.ProcessFailed,
		internal.ProcessCancelled, internal.ProcessTimedOut:
		return true
	}
	return false
}

// Store is a JSON-file-backed, mutex-guarded map of job ID -> Entry. Every
// mutation is persisted immediately via an atomic write, so a killed process
// never leaves the file mid-write or out of sync with what has happened.
type Store struct {
	path    string
	mu      sync.Mutex
	entries map[string]Entry
}

// NewStore loads path if it exists, or starts empty otherwise. A corrupt or
// unreadable file is treated as empty rather than fatal — losing the job index
// must not stop the daemon from starting.
func NewStore(path string) (*Store, error) {
	s := &Store{
		path:    path,
		entries: make(map[string]Entry),
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create jobs dir: %w", err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("failed to read jobs file: %w", err)
	}
	if len(data) == 0 {
		return s, nil
	}

	var entries map[string]Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return s, nil
	}
	s.entries = entries
	return s, nil
}

// Put records or replaces a job entry.
func (s *Store) Put(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[e.ID] = e
	return s.saveLocked()
}

// Remove deletes a job entry. Removing a missing ID is a no-op.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[id]; !ok {
		return nil
	}
	delete(s.entries, id)
	return s.saveLocked()
}

// Get returns the entry for id, if tracked.
func (s *Store) Get(id string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	return e, ok
}

// ListForWorkspace returns every tracked job for a workspace.
func (s *Store) ListForWorkspace(workspaceID string) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Entry{}
	for _, e := range s.entries {
		if e.WorkspaceID == workspaceID {
			out = append(out, e)
		}
	}
	return out
}

// All returns a snapshot of every tracked entry.
func (s *Store) All() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e)
	}
	return out
}

// FinishedBefore returns terminal entries whose end time (falling back to
// start time) is before cutoff — candidates for retention-based pruning.
func (s *Store) FinishedBefore(cutoff time.Time) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, e := range s.entries {
		if !Terminal(e.Status) {
			continue
		}
		end := e.EndedAt
		if end.IsZero() {
			end = e.StartedAt
		}
		if end.Before(cutoff) {
			out = append(out, e)
		}
	}
	return out
}

// saveLocked writes the current entries map to disk atomically. Caller must
// hold s.mu.
func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal jobs: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("failed to write temp jobs file: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("failed to rename temp jobs file: %w", err)
	}
	return nil
}
