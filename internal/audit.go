package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultAuditMaxBytes is the size at which the audit log rotates. A single
	// 10 MiB file is still cheap to grep while holding plenty of request
	// records.
	DefaultAuditMaxBytes int64 = 10 * 1024 * 1024

	// DefaultAuditKeep is the number of rotated audit files retained
	// (audit.jsonl.1 … audit.jsonl.7). The oldest file is deleted when a new
	// rotation would exceed the limit, so the log cannot grow without bound.
	DefaultAuditKeep = 7
)

// AuditOptions configures the structured audit log and its size-based
// rotation.
type AuditOptions struct {
	// Path is the audit log destination. An empty path disables auditing.
	Path string
	// MaxBytes is the size at which the log rotates. The daemon rotates before
	// writing a record that would push the file past this size, so records are
	// never split across files. Zero or negative falls back to
	// DefaultAuditMaxBytes.
	MaxBytes int64
	// Keep is the number of rotated files to retain, oldest deleted first.
	// Zero or negative falls back to DefaultAuditKeep so a zero-valued
	// AuditOptions never silently disables retention.
	Keep int
}

// SetAudit enables structured audit logging to the given path using the default
// rotation settings (DefaultAuditMaxBytes, DefaultAuditKeep).
func (l *SimpleLogger) SetAudit(enabled bool, path string) {
	l.SetAuditConfig(enabled, AuditOptions{
		Path:     path,
		MaxBytes: DefaultAuditMaxBytes,
		Keep:     DefaultAuditKeep,
	})
}

// SetAuditConfig enables structured audit logging to the configured path with
// size-based rotation, replacing any previously configured destination.
func (l *SimpleLogger) SetAuditConfig(enabled bool, opts AuditOptions) {
	l.auditMu.Lock()
	defer l.auditMu.Unlock()

	l.closeAuditLocked()
	l.audit = enabled
	l.auditPath = opts.Path
	l.auditMaxBytes = opts.MaxBytes
	if l.auditMaxBytes <= 0 {
		l.auditMaxBytes = DefaultAuditMaxBytes
	}
	l.auditKeep = opts.Keep
	if l.auditKeep <= 0 {
		l.auditKeep = DefaultAuditKeep
	}
	if !enabled || opts.Path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o750); err != nil {
		l.Warn("audit_directory_create_failed", "path", opts.Path, "error", err)
		l.audit = false
		return
	}
	if err := l.openAuditLocked(); err != nil {
		l.Warn("audit_file_open_failed", "path", opts.Path, "error", err)
		l.audit = false
		return
	}
}

// AuditEnabled reports whether structured audit logging is currently enabled.
func (l *SimpleLogger) AuditEnabled() bool {
	return l.audit
}

// Audit writes one structured JSON record per line to the configured audit
// file, rotating the file first when the record would exceed the configured
// size limit.
func (l *SimpleLogger) Audit(event string, keysAndValues ...interface{}) {
	if !l.audit {
		return
	}
	payload := map[string]interface{}{
		"ts":    time.Now().UTC().Format(time.RFC3339Nano),
		"event": event,
	}
	for i := 0; i+1 < len(keysAndValues); i += 2 {
		key := fmt.Sprint(keysAndValues[i])
		if key == "" {
			continue
		}
		payload[key] = sanitizeAuditValue(key, keysAndValues[i+1])
	}
	b, err := json.Marshal(payload)
	if err != nil {
		l.Warn("audit_record_encode_failed", "event", event, "error", err)
		return
	}
	record := append(b, '\n')

	l.auditMu.Lock()
	defer l.auditMu.Unlock()
	if l.auditFile == nil {
		return
	}
	// Rotate before writing so a record is never split across two files. An
	// empty file is never rotated: a single oversized record must still land
	// somewhere instead of spinning through rotations.
	if l.auditSize > 0 && l.auditSize+int64(len(record)) > l.auditMaxBytes {
		l.rotateAuditLocked()
		if l.auditFile == nil {
			return
		}
	}
	n, err := l.auditFile.Write(record)
	if err != nil {
		l.Warn("audit_write_failed", "path", l.auditPath, "error", err)
		return
	}
	l.auditSize += int64(n)
}

// openAuditLocked opens the audit log for appending and records its current
// size so rotation limits hold across daemon restarts. The caller must hold
// auditMu.
func (l *SimpleLogger) openAuditLocked() error {
	file, err := os.OpenFile(l.auditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	l.auditFile = file
	l.auditSize = info.Size()
	// Devices, FIFOs and sockets have no meaningful size and cannot be renamed,
	// so rotation is limited to regular files.
	l.auditRotatable = info.Mode().IsRegular()
	return nil
}

// closeAuditLocked closes the active audit file, if any. The caller must hold
// auditMu.
func (l *SimpleLogger) closeAuditLocked() {
	if l.auditFile != nil {
		_ = l.auditFile.Close()
		l.auditFile = nil
	}
	l.auditSize = 0
	l.auditRotatable = false
}

// rotateAuditLocked moves the active audit log to <path>.1, shifts the
// numbered backups down by one, deletes anything past the retention limit, and
// reopens a fresh active log. Rotation failures are reported but never stop
// auditing: the log keeps being appended to when a rename fails. The caller
// must hold auditMu.
func (l *SimpleLogger) rotateAuditLocked() {
	if l.auditPath == "" || !l.auditRotatable {
		return
	}
	l.closeAuditLocked()

	// Delete the file that would fall off the end of the retention window
	// before shifting the rest down, so a failed rename can never leave more
	// files behind than requested.
	oldest := l.auditBackupPath(l.auditKeep)
	if err := os.Remove(oldest); err != nil && !os.IsNotExist(err) {
		l.Warn("audit_rotation_failed", "path", oldest, "error", err)
	}
	for i := l.auditKeep - 1; i >= 1; i-- {
		from, to := l.auditBackupPath(i), l.auditBackupPath(i+1)
		if err := os.Rename(from, to); err != nil && !os.IsNotExist(err) {
			l.Warn("audit_rotation_failed", "path", from, "error", err)
		}
	}
	if err := os.Rename(l.auditPath, l.auditBackupPath(1)); err != nil && !os.IsNotExist(err) {
		l.Warn("audit_rotation_failed", "path", l.auditPath, "error", err)
	}

	if err := l.openAuditLocked(); err != nil {
		l.Warn("audit_file_open_failed", "path", l.auditPath, "error", err)
	}
}

// auditBackupPath returns the path of rotated audit file n, where n=1 is the
// most recently rotated file.
func (l *SimpleLogger) auditBackupPath(n int) string {
	return fmt.Sprintf("%s.%d", l.auditPath, n)
}

func sanitizeAuditValue(key string, value interface{}) interface{} {
	keyLower := strings.ToLower(key)
	if strings.Contains(keyLower, "token") || strings.Contains(keyLower, "secret") || strings.Contains(keyLower, "password") || strings.Contains(keyLower, "authorization") || strings.Contains(keyLower, "cookie") || strings.Contains(keyLower, "header") || strings.Contains(keyLower, "key") || strings.Contains(keyLower, "repo_url") || strings.Contains(keyLower, "repo-url") {
		return "[REDACTED]"
	}
	if s, ok := value.(string); ok && (strings.Contains(s, "Bearer ") || strings.Contains(s, "token=")) {
		return "[REDACTED]"
	}
	return value
}
