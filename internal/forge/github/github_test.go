package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/davidcrumpton/mcwsd/internal/forge"
)

func TestExtractProjectPathFromURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "HTTPS URL", input: "https://github.com/octo/project.git", want: "octo/project"},
		{name: "SSH URL", input: "git@github.com:octo/project.git", want: "octo/project"},
		{name: "Bare path", input: "octo/project", want: "octo/project"},
		{name: "Empty", input: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractProjectPathFromURL(tt.input)
			if got != tt.want {
				t.Fatalf("extractProjectPathFromURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNew_DefaultsEmptyAPIBase(t *testing.T) {
	f := New("", "token", "")
	if f.APIBase != "https://api.github.com" {
		t.Errorf("APIBase = %q, want %q", f.APIBase, "https://api.github.com")
	}

	// An explicit base (e.g. GitHub Enterprise) must be preserved.
	f = New("https://github.example.com/api/v3", "token", "")
	if f.APIBase != "https://github.example.com/api/v3" {
		t.Errorf("APIBase = %q, want explicit base preserved", f.APIBase)
	}
}

func TestOpenMergeRequest_NotFoundMentionsEndpointAndToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	f := New(srv.URL, "token", "")
	_, err := f.OpenMergeRequest(t.Context(), forge.OpenMRRequest{
		ProjectPath: "octo/private",
		Branch:      "agent/ws-1/fix",
		BaseBranch:  "main",
		Title:       "test",
	})
	if err == nil {
		t.Fatal("expected an error for a 404 response")
	}
	if !strings.Contains(err.Error(), "/repos/octo/private/pulls") {
		t.Errorf("error does not name the endpoint: %v", err)
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error does not hint at token access: %v", err)
	}
}

// TestOpenMergeRequest_RetriesTransientRefError verifies that GitHub's
// transient "not all refs are readable" 422 (returned when a just-pushed
// branch is not yet visible to the PR service) is retried rather than
// surfaced as a failure.
func TestOpenMergeRequest_RetriesTransientRefError(t *testing.T) {
	prCreateRetryDelay = 0
	defer func() { prCreateRetryDelay = 2 * time.Second }()

	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"resource":"PullRequest","code":"custom","message":"not all refs are readable"}]}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"html_url": "https://github.com/octo/project/pull/7"})
	}))
	defer srv.Close()

	f := New(srv.URL, "token", "")
	result, err := f.OpenMergeRequest(t.Context(), forge.OpenMRRequest{
		ProjectPath: "octo/project",
		Branch:      "agent/ws-1/fix",
		BaseBranch:  "main",
		Title:       "test",
	})
	if err != nil {
		t.Fatalf("OpenMergeRequest: %v", err)
	}
	if attempts < 2 {
		t.Fatalf("expected a retry, got %d attempt(s)", attempts)
	}
	if result.URL != "https://github.com/octo/project/pull/7" {
		t.Fatalf("URL = %q", result.URL)
	}
}

// TestOpenMergeRequest_AlreadyExistsReturnsExisting verifies that when the PR
// already exists (e.g. the caller retried), the existing PR is returned as
// success rather than reported as a 422 failure.
func TestOpenMergeRequest_AlreadyExistsReturnsExisting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"resource":"PullRequest","code":"custom","message":"A pull request already exists for octo:agent/ws-1/fix."}]}`))
		case http.MethodGet:
			if !strings.Contains(r.URL.RawQuery, "head=octo%3Aagent%2Fws-1%2Ffix") {
				t.Errorf("existing-PR lookup used wrong head query: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode([]map[string]string{{"html_url": "https://github.com/octo/project/pull/9"}})
		}
	}))
	defer srv.Close()

	f := New(srv.URL, "token", "")
	result, err := f.OpenMergeRequest(t.Context(), forge.OpenMRRequest{
		ProjectPath: "octo/project",
		Branch:      "agent/ws-1/fix",
		BaseBranch:  "main",
		Title:       "test",
	})
	if err != nil {
		t.Fatalf("OpenMergeRequest: %v", err)
	}
	if result.URL != "https://github.com/octo/project/pull/9" {
		t.Fatalf("URL = %q", result.URL)
	}
}

// TestOpenMergeRequest_UsesRepoDefaultBranch verifies that when no base branch
// is supplied, the PR targets the repository's actual default branch rather
// than assuming "main".
func TestOpenMergeRequest_UsesRepoDefaultBranch(t *testing.T) {
	var gotBase string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/project":
			_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": "master"})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/octo/project/pulls":
			var payload map[string]string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			gotBase = payload["base"]
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"html_url": "https://github.com/octo/project/pull/3"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	f := New(srv.URL, "token", "")
	result, err := f.OpenMergeRequest(t.Context(), forge.OpenMRRequest{
		ProjectPath: "octo/project",
		Branch:      "agent/ws-1/fix",
		Title:       "test",
	})
	if err != nil {
		t.Fatalf("OpenMergeRequest: %v", err)
	}
	if gotBase != "master" {
		t.Fatalf("base = %q, want %q", gotBase, "master")
	}
	if result.URL != "https://github.com/octo/project/pull/3" {
		t.Fatalf("URL = %q", result.URL)
	}
}

// TestOpenMergeRequest_PersistentRefErrorIncludesDiagnostics verifies that a
// "not all refs are readable" 422 which survives every retry reports which
// refs the token can see, instead of a bare validation error.
func TestOpenMergeRequest_PersistentRefErrorIncludesDiagnostics(t *testing.T) {
	prCreateRetryDelay = 0
	defer func() { prCreateRetryDelay = 2 * time.Second }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/project":
			_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": "master"})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/octo/project/pulls":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"resource":"PullRequest","code":"custom","message":"not all refs are readable"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	f := New(srv.URL, "token", "")
	_, err := f.OpenMergeRequest(t.Context(), forge.OpenMRRequest{
		ProjectPath: "octo/project",
		Branch:      "agent/ws-1/fix",
		Title:       "test",
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{
		"could not resolve PR refs",
		`default branch="master"`,
		`base "master" readable=false`,
		`head "agent/ws-1/fix" readable=false`,
		"Contents: Read",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// TestOpenMergeRequest_RefLookupFailureIsReported verifies that when the
// branch endpoint itself fails (e.g. 403 because the token lacks Contents:
// read), the diagnostic surfaces the failure and status rather than silently
// omitting it.
func TestOpenMergeRequest_RefLookupFailureIsReported(t *testing.T) {
	prCreateRetryDelay = 0
	defer func() { prCreateRetryDelay = 2 * time.Second }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/project":
			_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/branches/"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Resource not accessible by personal access token"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/octo/project/pulls":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"resource":"PullRequest","code":"custom","message":"not all refs are readable"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	f := New(srv.URL, "token", "")
	_, err := f.OpenMergeRequest(t.Context(), forge.OpenMRRequest{
		ProjectPath: "octo/project",
		Branch:      "agent/ws-1/fix",
		Title:       "test",
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{`base "main" check failed`, "403", "Resource not accessible"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestOpenMergeRequest_Success(t *testing.T) {
	var gotAuth, gotPath, gotBase string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		gotBase = payload["base"]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"html_url": "https://github.com/octo/project/pull/12"})
	}))
	defer srv.Close()

	f := New(srv.URL, "token", "")
	result, err := f.OpenMergeRequest(t.Context(), forge.OpenMRRequest{
		ProjectPath: "octo/project",
		Branch:      "agent/ws-1/fix",
		BaseBranch:  "main",
		Title:       "test",
		Description: "desc",
	})
	if err != nil {
		t.Fatalf("OpenMergeRequest: %v", err)
	}
	if result.URL != "https://github.com/octo/project/pull/12" {
		t.Fatalf("URL = %q", result.URL)
	}
	if !strings.Contains(gotPath, "/repos/octo/project/pulls") {
		t.Fatalf("path = %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if gotBase != "main" {
		t.Fatalf("base = %q", gotBase)
	}
}
