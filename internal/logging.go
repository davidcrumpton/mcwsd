package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SimpleLogger is a basic logger for Go 1.19 compatibility.
//
// Debug output is off unless explicitly enabled (see SetDebug), so verbose
// per-provisioner diagnostics stay out of the logs during normal operation.
type SimpleLogger struct {
	debug     bool
	audit     bool
	auditPath string
	auditFile *os.File
	auditMu   sync.Mutex
}

var Log = &SimpleLogger{}

// SetDebug turns debug-level logging on or off. It is intended to be called
// once at startup from the --debug CLI flag, before any goroutines that log
// are started.
func (l *SimpleLogger) SetDebug(enabled bool) {
	l.debug = enabled
}

// SetAudit enables structured audit logging to the given path.
func (l *SimpleLogger) SetAudit(enabled bool, path string) {
	l.auditMu.Lock()
	defer l.auditMu.Unlock()

	if l.auditFile != nil {
		_ = l.auditFile.Close()
		l.auditFile = nil
	}
	l.audit = enabled
	l.auditPath = path
	if !enabled {
		return
	}
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		l.Warn("audit_directory_create_failed", "path", path, "error", err)
		l.audit = false
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		l.Warn("audit_file_open_failed", "path", path, "error", err)
		l.audit = false
		return
	}
	l.auditFile = file
}

// DebugEnabled reports whether debug-level logging is currently enabled.
func (l *SimpleLogger) DebugEnabled() bool {
	return l.debug
}

// AuditEnabled reports whether structured audit logging is currently enabled.
func (l *SimpleLogger) AuditEnabled() bool {
	return l.audit
}

func (l *SimpleLogger) Info(msg string, keysAndValues ...interface{}) {
	l.log("INFO", msg, keysAndValues...)
}

func (l *SimpleLogger) UnsanitizedInfo(msg string, keysAndValues ...interface{}) {
	l.logUnsanitized("INFO", msg, keysAndValues...)
}

func (l *SimpleLogger) Warn(msg string, keysAndValues ...interface{}) {
	l.log("WARN", msg, keysAndValues...)
}

func (l *SimpleLogger) Error(msg string, keysAndValues ...interface{}) {
	l.log("ERROR", msg, keysAndValues...)
}

// Debug logs only when debug output has been enabled.
func (l *SimpleLogger) Debug(msg string, keysAndValues ...interface{}) {
	if !l.debug {
		return
	}
	l.log("DEBUG", msg, keysAndValues...)
}

// Audit writes one structured JSON record per line to the configured audit file.
func (l *SimpleLogger) Audit(event string, keysAndValues ...interface{}) {
	if !l.audit || l.auditFile == nil {
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
	l.auditMu.Lock()
	defer l.auditMu.Unlock()
	if l.auditFile == nil {
		return
	}
	_, _ = l.auditFile.Write(append(b, '\n'))
}

func (l *SimpleLogger) log(level, msg string, keysAndValues ...interface{}) {
	ts := time.Now().Format(time.RFC3339)
	kv := ""
	for i := 0; i < len(keysAndValues); i += 2 {
		if i+1 < len(keysAndValues) {
			kv += fmt.Sprintf(" %s=%v", keysAndValues[i], sanitizeLogValue(keysAndValues[i], keysAndValues[i+1]))
		}
	}
	fmt.Fprintf(os.Stderr, "%s [%s] %s%s\n", ts, level, msg, kv)
}

func (l *SimpleLogger) logUnsanitized(level, msg string, keysAndValues ...interface{}) {
	ts := time.Now().Format(time.RFC3339)
	kv := ""
	for i := 0; i < len(keysAndValues); i += 2 {
		if i+1 < len(keysAndValues) {
			kv += fmt.Sprintf(" %s=%v", keysAndValues[i], keysAndValues[i+1])
		}
	}
	fmt.Fprintf(os.Stderr, "%s [%s] %s%s\n", ts, level, msg, kv)
}

func sanitizeLogValue(key interface{}, value interface{}) interface{} {
	if key == nil {
		return value
	}
	keyStr := strings.ToLower(fmt.Sprint(key))
	if strings.Contains(keyStr, "token") || strings.Contains(keyStr, "secret") || strings.Contains(keyStr, "password") || strings.Contains(keyStr, "authorization") || strings.Contains(keyStr, "cookie") || strings.Contains(keyStr, "header") || strings.Contains(keyStr, "key") {
		return "[REDACTED]"
	}
	if s, ok := value.(string); ok && (strings.Contains(s, "Bearer ") || strings.Contains(s, "token=")) {
		return "[REDACTED]"
	}
	return value
}

func sanitizeAuditValue(key string, value interface{}) interface{} {
	keyLower := strings.ToLower(key)
	if strings.Contains(keyLower, "token") || strings.Contains(keyLower, "secret") || strings.Contains(keyLower, "password") || strings.Contains(keyLower, "authorization") || strings.Contains(keyLower, "cookie") || strings.Contains(keyLower, "header") || strings.Contains(keyLower, "key") {
		return "[REDACTED]"
	}
	if s, ok := value.(string); ok && (strings.Contains(s, "Bearer ") || strings.Contains(s, "token=")) {
		return "[REDACTED]"
	}
	return value
}
