package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal"
)

// TestRegisterRoutesNoConflict ensures every route pattern (jobs, the
// processes alias, and the rest) registers without a ServeMux conflict panic.
func TestRegisterRoutesNoConflict(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	s.registerRoutes()
}

// TestProcessIDFromPathAliases confirms the shared handlers read the job ID
// from either the canonical /jobs/{jid} route or the legacy /processes/{pid}
// alias.
func TestProcessIDFromPathAliases(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
	}{
		{"GET /workspaces/{id}/jobs/{jid}", "/workspaces/ws-1/jobs/proc-abc"},
		{"GET /workspaces/{id}/processes/{pid}", "/workspaces/ws-1/processes/proc-abc"},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			mux := http.NewServeMux()
			var got string
			mux.HandleFunc(tt.pattern, func(w http.ResponseWriter, r *http.Request) {
				got = processIDFromPath(r)
			})
			mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", tt.path, nil))
			if got != "proc-abc" {
				t.Fatalf("processIDFromPath = %q, want proc-abc", got)
			}
		})
	}
}

func TestServeHTTPDebugLogsSafeRequestDetails(t *testing.T) {
	previousDebug := internal.Log.DebugEnabled()
	internal.Log.SetDebug(true)
	defer internal.Log.SetDebug(previousDebug)

	originalStderr := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = writer
	defer func() {
		os.Stderr = originalStderr
		reader.Close()
	}()

	server := NewServer(nil, "test-token", WorkspaceAuthScoped)
	request := httptest.NewRequest(http.MethodGet, "/unknown?token=query-secret", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	os.Stderr = originalStderr
	if err := writer.Close(); err != nil {
		t.Fatalf("close log pipe: %v", err)
	}
	logged, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read log pipe: %v", err)
	}
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
	if !strings.Contains(string(logged), "method=GET") ||
		!strings.Contains(string(logged), `path="/unknown"`) ||
		!strings.Contains(string(logged), "status=404") {
		t.Errorf("request diagnostics missing expected fields: %q", logged)
	}
	if strings.Contains(string(logged), "query-secret") || strings.Contains(string(logged), "test-token") {
		t.Errorf("request diagnostics included sensitive query or authorization data: %q", logged)
	}
}

func TestServeHTTPAuditLogsStructuredRequestData(t *testing.T) {
	previousAudit := internal.Log.AuditEnabled()
	auditPath := t.TempDir() + "/audit.jsonl"
	internal.Log.SetAudit(true, auditPath)
	defer internal.Log.SetAudit(previousAudit, auditPath)

	server := NewServer(nil, "test-token", WorkspaceAuthScoped)
	request := httptest.NewRequest(http.MethodGet, "/workspaces/ws-123/files?path=/tmp", nil)
	request.Header.Set("Authorization", "Bearer wrong-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if got := response.Header().Get("X-Correlation-ID"); got == "" {
		t.Fatal("response missing X-Correlation-ID header")
	}
	logged, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	logText := string(logged)
	if !strings.Contains(logText, "\"event\":\"http_request\"") ||
		!strings.Contains(logText, "\"workspace_id\":\"ws-123\"") ||
		!strings.Contains(logText, "\"correlation_id\":") {
		t.Fatalf("audit log missing structured request fields: %q", logText)
	}
	if strings.Contains(logText, "query-secret") || strings.Contains(logText, "test-token") {
		t.Fatalf("audit log included secret data: %q", logText)
	}
}
