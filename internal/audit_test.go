package internal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// newAuditTestLogger returns a logger writing audit records to a temp path with
// the supplied rotation settings, and cleans up afterwards.
func newAuditTestLogger(t *testing.T, maxBytes int64, keep int) (*SimpleLogger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l := &SimpleLogger{}
	l.SetAuditConfig(true, AuditOptions{Path: path, MaxBytes: maxBytes, Keep: keep})
	if !l.AuditEnabled() {
		t.Fatal("audit logging not enabled after SetAuditConfig")
	}
	t.Cleanup(func() { l.SetAuditConfig(false, AuditOptions{}) })
	return l, path
}

// auditFiles returns the active audit file plus any rotated files, newest
// first, so tests can reason about retention.
func auditFiles(t *testing.T, path string) []string {
	t.Helper()
	active := []string{path}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat active audit file: %v", err)
	}
	var rotated []string
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s.%d", path, i)
		if _, err := os.Stat(candidate); err != nil {
			break
		}
		rotated = append(rotated, candidate)
	}
	return append(active, rotated...)
}

// readAuditLines returns the JSON records in one audit file.
func readAuditLines(t *testing.T, path string) []map[string]interface{} {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var records []map[string]interface{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var record map[string]interface{}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("%s contains a malformed record %q: %v", path, line, err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return records
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}

// TestSetAuditUsesDefaultRotationSettings pins the defaults: 10 MiB per file
// and 7 rotated files retained.
func TestSetAuditUsesDefaultRotationSettings(t *testing.T) {
	if DefaultAuditKeep != 7 {
		t.Fatalf("DefaultAuditKeep = %d, want 7", DefaultAuditKeep)
	}
	if DefaultAuditMaxBytes != 10*1024*1024 {
		t.Fatalf("DefaultAuditMaxBytes = %d, want %d", DefaultAuditMaxBytes, 10*1024*1024)
	}

	l := &SimpleLogger{}
	l.SetAuditConfig(true, AuditOptions{Path: filepath.Join(t.TempDir(), "audit.jsonl")})
	defer l.SetAuditConfig(false, AuditOptions{})
	if l.auditMaxBytes != DefaultAuditMaxBytes {
		t.Errorf("auditMaxBytes = %d, want default %d", l.auditMaxBytes, DefaultAuditMaxBytes)
	}
	if l.auditKeep != DefaultAuditKeep {
		t.Errorf("auditKeep = %d, want default %d", l.auditKeep, DefaultAuditKeep)
	}

	// SetAudit is the compatibility wrapper and must apply the same defaults.
	l2 := &SimpleLogger{}
	l2.SetAudit(true, filepath.Join(t.TempDir(), "audit.jsonl"))
	defer l2.SetAudit(false, "")
	if l2.auditMaxBytes != DefaultAuditMaxBytes || l2.auditKeep != DefaultAuditKeep {
		t.Errorf("SetAudit rotation defaults = (%d, %d), want (%d, %d)",
			l2.auditMaxBytes, l2.auditKeep, DefaultAuditMaxBytes, DefaultAuditKeep)
	}
}

// TestAuditRotatesBySize verifies files never grow past the size limit and that
// records are never split across files.
func TestAuditRotatesBySize(t *testing.T) {
	const maxBytes int64 = 400
	const keep = 3
	l, path := newAuditTestLogger(t, maxBytes, keep)

	const total = 60
	for i := 0; i < total; i++ {
		l.Audit("http_request", "workspace_id", fmt.Sprintf("ws-%03d", i))
	}

	files := auditFiles(t, path)
	if len(files) != keep+1 {
		t.Fatalf("audit files = %d (%v), want %d", len(files), files, keep+1)
	}
	for _, file := range files {
		if size := fileSize(t, file); size > maxBytes {
			t.Errorf("%s is %d bytes, want <= %d", file, size, maxBytes)
		}
		if records := readAuditLines(t, file); len(records) == 0 {
			t.Errorf("%s has no records", file)
		}
	}

	// The newest record must be in the active file and the rotated files must
	// hold strictly older records.
	newest := readAuditLines(t, path)
	last := newest[len(newest)-1]
	if got := last["workspace_id"]; got != fmt.Sprintf("ws-%03d", total-1) {
		t.Errorf("newest record workspace_id = %v, want ws-%03d", got, total-1)
	}
	rotatedNewest := readAuditLines(t, fmt.Sprintf("%s.1", path))
	if got := rotatedNewest[len(rotatedNewest)-1]["workspace_id"]; got == last["workspace_id"] {
		t.Errorf("rotated file holds the newest record %v", got)
	}
}

// TestAuditKeepsAtMostKeepRotatedFiles checks that rotation prunes the oldest
// file instead of growing without bound.
func TestAuditKeepsAtMostKeepRotatedFiles(t *testing.T) {
	const keep = 7
	l, path := newAuditTestLogger(t, 200, keep)

	// Enough records to force far more rotations than the retention limit.
	for i := 0; i < 120; i++ {
		l.Audit("http_request", "workspace_id", fmt.Sprintf("ws-%03d", i))
	}

	files := auditFiles(t, path)
	if len(files) > keep+1 {
		t.Fatalf("retained %d audit files (%v), want at most %d", len(files), files, keep+1)
	}
	if len(files) != keep+1 {
		t.Fatalf("retained %d audit files (%v), want %d", len(files), files, keep+1)
	}
	if _, err := os.Stat(fmt.Sprintf("%s.%d", path, keep+1)); err == nil {
		t.Fatalf("audit file %s.%d exists past the keep limit", path, keep+1)
	}
}

// TestAuditKeepMinimumOne documents the tightest retention setting: one
// rotated file plus the active file, with older content deleted.
func TestAuditKeepMinimumOne(t *testing.T) {
	l, path := newAuditTestLogger(t, 200, 1)

	for i := 0; i < 20; i++ {
		l.Audit("http_request", "workspace_id", fmt.Sprintf("ws-%03d", i))
	}

	files := auditFiles(t, path)
	if len(files) != 2 {
		t.Fatalf("audit files = %v, want the active file plus one rotated file", files)
	}
	if _, err := os.Stat(path + ".2"); err == nil {
		t.Fatalf("rotated file %s.2 exists with --audit-keep=1", path)
	}
	if size := fileSize(t, path); size > 200 {
		t.Fatalf("active file is %d bytes, want <= 200", size)
	}
	records := readAuditLines(t, path)
	if len(records) == 0 {
		t.Fatal("active file has no records")
	}
	if got := records[len(records)-1]["workspace_id"]; got != "ws-019" {
		t.Errorf("newest record workspace_id = %v, want ws-019", got)
	}
}

// TestAuditNonPositiveKeepFallsBackToDefault keeps a zero-valued AuditOptions
// from silently disabling retention.
func TestAuditNonPositiveKeepFallsBackToDefault(t *testing.T) {
	for _, keep := range []int{0, -5} {
		l := &SimpleLogger{}
		l.SetAuditConfig(true, AuditOptions{Path: filepath.Join(t.TempDir(), "audit.jsonl"), Keep: keep})
		if l.auditKeep != DefaultAuditKeep {
			t.Errorf("Keep=%d gave auditKeep=%d, want default %d", keep, l.auditKeep, DefaultAuditKeep)
		}
		l.SetAuditConfig(false, AuditOptions{})
	}
}

// TestAuditRotatesOnRestart verifies the size of an existing log is picked up
// at open time, so a restart cannot silently keep growing a full file.
func TestAuditRotatesOnRestart(t *testing.T) {
	const maxBytes int64 = 200
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", int(maxBytes-10))+"\n"), 0o640); err != nil {
		t.Fatalf("seed audit file: %v", err)
	}

	l := &SimpleLogger{}
	l.SetAuditConfig(true, AuditOptions{Path: path, MaxBytes: maxBytes, Keep: 3})
	defer l.SetAuditConfig(false, AuditOptions{})

	l.Audit("http_request", "workspace_id", "ws-restart")

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("existing audit log was not rotated on the first record: %v", err)
	}
	records := readAuditLines(t, path)
	if len(records) != 1 {
		t.Fatalf("active file has %d records, want 1", len(records))
	}
	if got := records[0]["workspace_id"]; got != "ws-restart" {
		t.Errorf("record workspace_id = %v, want ws-restart", got)
	}
}

// TestAuditRecordLargerThanLimit makes sure a single record bigger than the
// limit is written instead of triggering endless rotations.
func TestAuditRecordLargerThanLimit(t *testing.T) {
	l, path := newAuditTestLogger(t, 20, 3)

	l.Audit("http_request", "workspace_id", "ws-oversized-record")
	l.Audit("http_request", "workspace_id", "ws-second-record")

	records := readAuditLines(t, path)
	if len(records) != 1 {
		t.Fatalf("active file has %d records, want 1", len(records))
	}
	if got := records[0]["workspace_id"]; got != "ws-second-record" {
		t.Errorf("active record workspace_id = %v, want ws-second-record", got)
	}
	rotated := readAuditLines(t, path+".1")
	if len(rotated) != 1 {
		t.Fatalf("rotated file has %d records, want 1", len(rotated))
	}
	if got := rotated[0]["workspace_id"]; got != "ws-oversized-record" {
		t.Errorf("rotated record workspace_id = %v, want ws-oversized-record", got)
	}
}

// TestAuditSkipsRotationForNonRegularFile covers destinations such as /dev/null
// that cannot be renamed.
func TestAuditSkipsRotationForNonRegularFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("non-regular device paths differ on Windows")
	}
	l := &SimpleLogger{}
	l.SetAuditConfig(true, AuditOptions{Path: os.DevNull, MaxBytes: 1, Keep: 3})
	defer l.SetAuditConfig(false, AuditOptions{})
	if l.auditRotatable {
		t.Fatal("non-regular audit destination marked rotatable")
	}
	l.Audit("http_request", "workspace_id", "ws-devnull")
	l.Audit("http_request", "workspace_id", "ws-devnull-2")
	if _, err := os.Stat(os.DevNull + ".1"); err == nil {
		t.Fatalf("%s.1 was created by rotating a non-regular file", os.DevNull)
	}
}

// TestAuditConcurrentWritesRotate verifies rotation stays consistent when many
// goroutines audit at once: every line in every file must remain valid JSON.
func TestAuditConcurrentWritesRotate(t *testing.T) {
	l, path := newAuditTestLogger(t, 512, 4)

	const writers = 8
	const perWriter = 40
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				l.Audit("http_request", "workspace_id", fmt.Sprintf("ws-%02d-%03d", w, i))
			}
		}(w)
	}
	wg.Wait()

	files := auditFiles(t, path)
	total := 0
	var bytes int64
	for _, file := range files {
		// readAuditLines fails the test if any line was split or interleaved.
		total += len(readAuditLines(t, file))
		bytes += fileSize(t, file)
	}
	if total == 0 {
		t.Fatal("no audit records written")
	}
	// The whole point of rotation: the retained trail stays bounded no matter
	// how many records were written.
	if maxRetained := int64(len(files)) * 512; bytes > maxRetained {
		t.Fatalf("retained audit files hold %d bytes, want <= %d", bytes, maxRetained)
	}
}

// TestAuditRedactsSecrets keeps the existing redaction guarantee intact after
// the rotation rewrite.
func TestAuditRedactsSecrets(t *testing.T) {
	l, path := newAuditTestLogger(t, DefaultAuditMaxBytes, DefaultAuditKeep)
	l.Audit("http_request", "authorization", "Bearer super-secret", "workspace_id", "ws-1")

	records := readAuditLines(t, path)
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	if got := records[0]["authorization"]; got != "[REDACTED]" {
		t.Errorf("authorization = %v, want [REDACTED]", got)
	}
	if got := records[0]["workspace_id"]; got != "ws-1" {
		t.Errorf("workspace_id = %v, want ws-1", got)
	}
}
