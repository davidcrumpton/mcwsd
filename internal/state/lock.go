package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ErrLocked means the lock file is held by a live process. Callers such as
// the cleanup and status subcommands use this to refuse to operate on a state
// file while the daemon that owns it is still running.
var ErrLocked = errors.New("state file is in use by a running daemon")

// PIDLock is a best-effort pid-file guard. It is deliberately a file-creation
// lock (O_EXCL) rather than an advisory flock so it works on every supported
// runtime, including illumos/Solaris where syscall.Flock is unavailable.
//
// The daemon creates the lock at startup and removes it on a clean shutdown.
// A lock left behind by a crash is treated as stale when its pid is no longer
// alive, and is replaced.
type PIDLock struct {
	path string
}

// AcquirePIDLock creates path as an exclusive pid file. It returns ErrLocked
// (wrapped) when the file belongs to another live process.
func AcquirePIDLock(path string) (*PIDLock, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create lock dir: %w", err)
		}
	}

	// A few attempts cover the case where a stale file is removed between our
	// read and our create.
	for attempt := 0; attempt < 3; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, werr := fmt.Fprintf(f, "%d\n", os.Getpid())
			cerr := f.Close()
			if werr != nil {
				os.Remove(path)
				return nil, fmt.Errorf("failed to write daemon lock %s: %w", path, werr)
			}
			if cerr != nil {
				os.Remove(path)
				return nil, fmt.Errorf("failed to close daemon lock %s: %w", path, cerr)
			}
			return &PIDLock{path: path}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("failed to create daemon lock %s: %w", path, err)
		}

		data, readErr := os.ReadFile(path)
		if readErr == nil {
			pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			if pid > 0 && processAlive(pid) {
				return nil, fmt.Errorf("%w (pid %d, lock %s)", ErrLocked, pid, path)
			}
		}
		// Missing/empty content or a dead pid: the previous owner left it
		// behind. Remove it and try to take it over.
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			return nil, fmt.Errorf("failed to remove stale daemon lock %s: %w", path, rmErr)
		}
	}
	return nil, fmt.Errorf("could not acquire daemon lock %s", path)
}

// Close removes the lock file. It is safe to call on a nil receiver.
func (l *PIDLock) Close() error {
	if l == nil || l.path == "" {
		return nil
	}
	path := l.path
	l.path = ""
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// processAlive reports whether pid names a live process. EPERM means the
// process exists but is owned by another user, which still counts as alive.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
