package internal

import (
	"io"
	"os"
	"strings"
	"testing"
)

// TestDebugGatedBySetDebug verifies the single on/off debug switch: Debug
// output is suppressed while disabled and emitted while enabled.
func TestDebugGatedBySetDebug(t *testing.T) {
	originalStderr := os.Stderr
	defer func() { os.Stderr = originalStderr }()

	l := &SimpleLogger{}

	// Disabled: Debug must not write anything.
	l.SetDebug(false)
	if l.DebugEnabled() {
		t.Fatal("DebugEnabled() = true before being enabled")
	}
	disabledOut := captureStderr(t, func() { l.Debug("hidden_message") })
	if strings.Contains(disabledOut, "hidden_message") {
		t.Errorf("debug output written while disabled: %q", disabledOut)
	}

	// Enabled: Debug must write the message.
	l.SetDebug(true)
	if !l.DebugEnabled() {
		t.Fatal("DebugEnabled() = false after being enabled")
	}
	enabledOut := captureStderr(t, func() { l.Debug("visible_message") })
	if !strings.Contains(enabledOut, "visible_message") {
		t.Errorf("debug output not written while enabled: %q", enabledOut)
	}
}

// captureStderr redirects os.Stderr for the duration of fn and returns what
// was written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	original := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = original }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(out)
}
