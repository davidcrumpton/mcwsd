package internal

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// SimpleLogger is a basic logger for Go 1.19 compatibility.
//
// Debug output is off unless explicitly enabled (see SetDebug), so verbose
// per-provisioner diagnostics stay out of the logs during normal operation.
// Audit output is off unless enabled via SetAuditConfig (see audit.go).
type SimpleLogger struct {
	debug     bool
	audit     bool
	auditPath string
	auditFile *os.File
	// auditSize tracks how many bytes the active audit file holds so rotation
	// can happen without a stat per record.
	auditSize int64
	// auditMaxBytes is the size at which the audit file rotates.
	auditMaxBytes int64
	// auditKeep is the number of rotated audit files retained.
	auditKeep int
	// auditRotatable is false for destinations that cannot be renamed (devices,
	// FIFOs, sockets).
	auditRotatable bool
	auditMu        sync.Mutex
}

var Log = &SimpleLogger{}

// SetDebug turns debug-level logging on or off. It is intended to be called
// once at startup from the --debug CLI flag, before any goroutines that log
// are started.
func (l *SimpleLogger) SetDebug(enabled bool) {
	l.debug = enabled
}

// DebugEnabled reports whether debug-level logging is currently enabled.
func (l *SimpleLogger) DebugEnabled() bool {
	return l.debug
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
	if strings.Contains(keyStr, "token") || strings.Contains(keyStr, "secret") || strings.Contains(keyStr, "password") || strings.Contains(keyStr, "authorization") || strings.Contains(keyStr, "cookie") || strings.Contains(keyStr, "header") || strings.Contains(keyStr, "key") || strings.Contains(keyStr, "repo_url") || strings.Contains(keyStr, "repo-url") {
		return "[REDACTED]"
	}
	if s, ok := value.(string); ok && (strings.Contains(s, "Bearer ") || strings.Contains(s, "token=")) {
		return "[REDACTED]"
	}
	return value
}
