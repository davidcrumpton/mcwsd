package gitlab

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal/forge"
)

// ---------------------------------------------------------------------------
// extractProjectPathFromURL — pure function, no network
// ---------------------------------------------------------------------------

func TestExtractProjectPathFromURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "SSH URL with .git suffix",
			input: "git@gitlab.example.org:namespace/project.git",
			want:  "namespace/project",
		},
		{
			name:  "SSH URL without .git suffix",
			input: "git@gitlab.example.org:namespace/project",
			want:  "namespace/project",
		},
		{
			name:  "SSH URL nested namespace",
			input: "git@gitlab.example.org:group/sub/project.git",
			want:  "group/sub/project",
		},
		{
			name:  "HTTPS URL with .git suffix",
			input: "https://gitlab.example.org/namespace/project.git",
			want:  "namespace/project",
		},
		{
			name:  "HTTPS URL without .git suffix",
			input: "https://gitlab.example.org/namespace/project",
			want:  "namespace/project",
		},
		{
			name:  "HTTP URL",
			input: "http://gitlab.example.org/ns/proj",
			want:  "ns/proj",
		},
		{
			name:  "bare path — returned as-is",
			input: "namespace/project",
			want:  "namespace/project",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "URL with no path after host",
			input: "https://gitlab.example.org",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractProjectPathFromURL(tt.input)
			if got != tt.want {
				t.Errorf("extractProjectPathFromURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// OpenMergeRequest — via httptest.Server
// ---------------------------------------------------------------------------

// newTestServer returns an httptest.Server that calls handler on every request
// and closes automatically at test cleanup.
func newTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// mrURL is the expected web URL returned by the fake server.
const mrURL = "https://gitlab.example.org/ns/proj/-/merge_requests/42"

// okHandler always returns 201 Created with a minimal MR body.
func okHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t.Helper()
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("PRIVATE-TOKEN") == "" {
			t.Error("PRIVATE-TOKEN header missing")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		body, _ := json.Marshal(map[string]string{"web_url": mrURL})
		_, _ = w.Write(body)
	}
}

func TestOpenMergeRequest_Success(t *testing.T) {
	srv := newTestServer(t, okHandler(t))

	f := New(srv.URL, "test-token", "")
	result, err := f.OpenMergeRequest(t.Context(), forge.OpenMRRequest{
		ProjectPath: "ns/proj",
		Branch:      "agent/ws-1/fix",
		BaseBranch:  "main",
		Title:       "My fix",
		Description: "Fixes things",
	})
	if err != nil {
		t.Fatalf("OpenMergeRequest: %v", err)
	}
	if result.URL != mrURL {
		t.Errorf("URL: want %q, got %q", mrURL, result.URL)
	}
}

func TestOpenMergeRequest_DefaultBaseBranch(t *testing.T) {
	var receivedBase string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		_ = json.NewDecoder(r.Body).Decode(&payload)
		receivedBase = payload["target_branch"]
		w.WriteHeader(http.StatusCreated)
		body, _ := json.Marshal(map[string]string{"web_url": mrURL})
		_, _ = w.Write(body)
	})

	f := New(srv.URL, "token", "")
	_, _ = f.OpenMergeRequest(t.Context(), forge.OpenMRRequest{
		ProjectPath: "ns/proj",
		Branch:      "agent/ws-2/feat",
		// BaseBranch intentionally omitted — should default to "main"
	})

	if receivedBase != "main" {
		t.Errorf("default BaseBranch: want %q, got %q", "main", receivedBase)
	}
}

func TestOpenMergeRequest_ProjectPathFallback_FromForgeConfig(t *testing.T) {
	var receivedURI string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		receivedURI = r.RequestURI // raw, un-decoded URI
		w.WriteHeader(http.StatusCreated)
		body, _ := json.Marshal(map[string]string{"web_url": mrURL})
		_, _ = w.Write(body)
	})

	// ProjectPath set on the forge, NOT on the request.
	f := New(srv.URL, "token", "group/my-proj")
	_, _ = f.OpenMergeRequest(t.Context(), forge.OpenMRRequest{
		Branch: "agent/ws-3/test",
	})

	// url.PathEscape encodes "/" as "%2F"; check the raw URI.
	if !strings.Contains(receivedURI, "group%2Fmy-proj") {
		t.Errorf("raw URI should contain URL-escaped project path, got %q", receivedURI)
	}
}

func TestOpenMergeRequest_ProjectPathFallback_FromRepoURL(t *testing.T) {
	var receivedURI string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		receivedURI = r.RequestURI // raw, un-decoded URI
		w.WriteHeader(http.StatusCreated)
		body, _ := json.Marshal(map[string]string{"web_url": mrURL})
		_, _ = w.Write(body)
	})

	// Neither forge config nor request has project path — must derive from RepoURL.
	f := New(srv.URL, "token", "")
	_, _ = f.OpenMergeRequest(t.Context(), forge.OpenMRRequest{
		RepoURL: "https://gitlab.example.org/derived/project.git",
		Branch:  "agent/ws-4/test",
	})

	// url.PathEscape encodes "/" as "%2F"; check the raw URI.
	if !strings.Contains(receivedURI, "derived%2Fproject") {
		t.Errorf("raw URI should contain derived project path, got %q", receivedURI)
	}
}

func TestOpenMergeRequest_MissingProjectPath_Error(t *testing.T) {
	// No project path anywhere — must fail before making any HTTP call.
	f := New("http://localhost:9", "token", "")
	_, err := f.OpenMergeRequest(t.Context(), forge.OpenMRRequest{
		Branch: "agent/ws-5/test",
		// RepoURL is empty, ProjectPath is empty, forge has no project path.
	})
	if err == nil {
		t.Fatal("expected error for missing project path, got nil")
	}
}

func TestOpenMergeRequest_NonCreatedStatus_Error(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintln(w, `{"message":"403 Forbidden"}`)
	})

	f := New(srv.URL, "bad-token", "")
	_, err := f.OpenMergeRequest(t.Context(), forge.OpenMRRequest{
		ProjectPath: "ns/proj",
		Branch:      "agent/ws-6/test",
	})
	if err == nil {
		t.Fatal("expected error for 403 response, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should mention status code 403: %v", err)
	}
}

func TestOpenMergeRequest_RetryOnSourceBranchNotExist(t *testing.T) {
	attempts := 0
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			// Simulate eventual-consistency failure.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintln(w, `{"message":["source_branch does not exist"]}`)
			return
		}
		// Succeed on third attempt.
		w.WriteHeader(http.StatusCreated)
		body, _ := json.Marshal(map[string]string{"web_url": mrURL})
		_, _ = w.Write(body)
	})

	f := New(srv.URL, "token", "")
	result, err := f.OpenMergeRequest(t.Context(), forge.OpenMRRequest{
		ProjectPath: "ns/proj",
		Branch:      "agent/ws-7/retry",
	})
	if err != nil {
		t.Fatalf("OpenMergeRequest: expected success after retries, got %v", err)
	}
	if result.URL != mrURL {
		t.Errorf("URL after retry: want %q, got %q", mrURL, result.URL)
	}
	if attempts < 3 {
		t.Errorf("expected at least 3 attempts (retry logic), got %d", attempts)
	}
}

func TestOpenMergeRequest_NoRetryOnOtherBadRequest(t *testing.T) {
	attempts := 0
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintln(w, `{"message":"title is too long"}`)
	})

	f := New(srv.URL, "token", "")
	_, err := f.OpenMergeRequest(t.Context(), forge.OpenMRRequest{
		ProjectPath: "ns/proj",
		Branch:      "agent/ws-8/no-retry",
	})
	if err == nil {
		t.Fatal("expected error for 400 non-retryable, got nil")
	}
	// Must not retry for unrelated 400 errors.
	if attempts != 1 {
		t.Errorf("expected exactly 1 attempt for non-retryable 400, got %d", attempts)
	}
}

func TestOpenMergeRequest_PrivateTokenSent(t *testing.T) {
	var gotToken string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("PRIVATE-TOKEN")
		w.WriteHeader(http.StatusCreated)
		body, _ := json.Marshal(map[string]string{"web_url": mrURL})
		_, _ = w.Write(body)
	})

	f := New(srv.URL, "my-secret-token", "")
	_, _ = f.OpenMergeRequest(t.Context(), forge.OpenMRRequest{
		ProjectPath: "ns/proj",
		Branch:      "agent/ws-9/token-check",
	})

	if gotToken != "my-secret-token" {
		t.Errorf("PRIVATE-TOKEN: want %q, got %q", "my-secret-token", gotToken)
	}
}
