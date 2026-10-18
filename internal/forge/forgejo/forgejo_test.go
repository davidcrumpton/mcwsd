package forgejo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal/forge"
)

func TestExtractProjectPathFromURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "HTTPS URL", input: "https://git.example.com/team/project.git", want: "team/project"},
		{name: "SSH URL", input: "git@git.example.com:team/project.git", want: "team/project"},
		{name: "Bare path", input: "team/project", want: "team/project"},
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
		if r.URL.Path == "/api/v1/repos/team/project/pulls" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"html_url": "https://git.example.com/team/project/pulls/7"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"html_url": "https://git.example.com/team/project/pulls/7"})
	}))
	defer srv.Close()

	f := New(srv.URL, "token", "")
	result, err := f.OpenMergeRequest(t.Context(), forge.OpenMRRequest{
		ProjectPath: "team/project",
		Branch:      "agent/ws-2/fix",
		BaseBranch:  "main",
		Title:       "test",
		Description: "desc",
	})
	if err != nil {
		t.Fatalf("OpenMergeRequest: %v", err)
	}
	if result.URL != "https://git.example.com/team/project/pulls/7" {
		t.Fatalf("URL = %q", result.URL)
	}
	if !strings.Contains(gotPath, "/api/v1/repos/team/project/pulls") {
		t.Fatalf("path = %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "token ") {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if gotBase != "main" {
		t.Fatalf("base = %q", gotBase)
	}
}
