package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/davidcrumpton/mcwsd/internal"
)

func TestParseWorkspaceAuthMode(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    WorkspaceAuthMode
		wantErr bool
	}{
		{"scoped", WorkspaceAuthScoped, false},
		{"strict", WorkspaceAuthStrict, false},
		{" strict ", WorkspaceAuthStrict, false},
		{"", "", true},
		{"shared", "", true},
		{"STRICT", "", true},
	} {
		got, err := ParseWorkspaceAuthMode(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseWorkspaceAuthMode(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseWorkspaceAuthMode(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseWorkspaceAuthMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTokenAuthorityScopesWorkspaceTokens(t *testing.T) {
	a := newTokenAuthority("admin-secret")
	aToken := a.workspaceToken("ws-a")
	bToken := a.workspaceToken("ws-b")

	if aToken == bToken {
		t.Fatal("workspace tokens for different workspaces are identical")
	}
	if !a.isAdmin("admin-secret") || a.isAdmin(aToken) {
		t.Fatal("admin token classification is wrong")
	}
	if !a.authorizeWorkspace(WorkspaceAuthScoped, "ws-a", aToken) {
		t.Fatal("workspace token rejected for its own workspace")
	}
	if a.authorizeWorkspace(WorkspaceAuthScoped, "ws-b", aToken) {
		t.Fatal("workspace token accepted for another workspace")
	}
	if !a.authorizeWorkspace(WorkspaceAuthScoped, "ws-b", "admin-secret") {
		t.Fatal("scoped mode should accept the admin token as break-glass")
	}
	if a.authorizeWorkspace(WorkspaceAuthStrict, "ws-b", "admin-secret") {
		t.Fatal("strict mode must reject the admin token on workspace routes")
	}
	if a.authorizeWorkspace("", "ws-b", "admin-secret") {
		t.Fatal("an unknown mode must fail closed")
	}
}

func TestWorkspaceAuthWrappers(t *testing.T) {
	tests := []struct {
		name  string
		mode  WorkspaceAuthMode
		id    string
		token func(*tokenAuthority) string
		want  int
	}{
		{
			name:  "scoped admin token",
			mode:  WorkspaceAuthScoped,
			id:    "ws-a",
			token: func(*tokenAuthority) string { return "admin-secret" },
			want:  http.StatusNoContent,
		},
		{
			name:  "strict admin token",
			mode:  WorkspaceAuthStrict,
			id:    "ws-a",
			token: func(*tokenAuthority) string { return "admin-secret" },
			want:  http.StatusUnauthorized,
		},
		{
			name:  "matching workspace token",
			mode:  WorkspaceAuthStrict,
			id:    "ws-a",
			token: func(a *tokenAuthority) string { return a.workspaceToken("ws-a") },
			want:  http.StatusNoContent,
		},
		{
			name:  "other workspace token",
			mode:  WorkspaceAuthStrict,
			id:    "ws-b",
			token: func(a *tokenAuthority) string { return a.workspaceToken("ws-a") },
			want:  http.StatusUnauthorized,
		},
		{
			name:  "missing token",
			mode:  WorkspaceAuthScoped,
			id:    "ws-a",
			token: func(*tokenAuthority) string { return "" },
			want:  http.StatusUnauthorized,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			authority := newTokenAuthority("admin-secret")
			s := &Server{tokens: authority, workspaceAuthMode: tc.mode}
			handler := s.withWorkspaceAuth(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodPost, "/workspaces/"+tc.id+"/exec", nil)
			req.SetPathValue("id", tc.id)
			if tok := tc.token(authority); tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
			rec := httptest.NewRecorder()
			handler(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestServeHTTPEnforcesWorkspaceTokenScope(t *testing.T) {
	s := NewServer(nil, "admin-secret", WorkspaceAuthStrict)
	token := s.workspaceToken("ws-a")

	tests := []struct {
		name   string
		method string
		path   string
		token  string
	}{
		{"workspace token on another workspace", http.MethodPost, "/workspaces/ws-b/exec", token},
		{"workspace token on control plane", http.MethodPost, "/workspaces", token},
		{"workspace token on admin route", http.MethodGet, "/admin/leftovers", token},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer "+tc.token)
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
		})
	}
}

func TestAdminAuthRejectsWorkspaceToken(t *testing.T) {
	authority := newTokenAuthority("admin-secret")
	s := &Server{tokens: authority, workspaceAuthMode: WorkspaceAuthStrict}
	handler := s.withAdminAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/workspaces", nil)
	req.Header.Set("Authorization", "Bearer "+authority.workspaceToken("ws-a"))
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	req = httptest.NewRequest(http.MethodPost, "/workspaces", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("admin status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

type provisionStubBackend struct {
	internal.WorkspaceBackend
	result *internal.ProvisionResult
}

func (s provisionStubBackend) Provision(context.Context, internal.ProvisionRequest) (*internal.ProvisionResult, error) {
	return s.result, nil
}

func TestPrepareReturnsWorkspaceToken(t *testing.T) {
	tests := []struct {
		name       string
		result     *internal.ProvisionResult
		wantStatus int
		wantID     string
	}{
		{
			name: "sync",
			result: &internal.ProvisionResult{
				WorkspaceID: "ws-sync",
				Workspace:   &internal.Workspace{ID: "ws-sync", OS: "linux"},
			},
			wantStatus: http.StatusOK,
			wantID:     "ws-sync",
		},
		{
			name: "async",
			result: &internal.ProvisionResult{
				WorkspaceID: "ws-async",
				JobID:       "prov-1",
				Status:      internal.ProcessQueued,
			},
			wantStatus: http.StatusAccepted,
			wantID:     "ws-async",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(provisionStubBackend{result: tc.result}, "admin-secret", WorkspaceAuthScoped)
			req := httptest.NewRequest(http.MethodPost, "/workspaces", strings.NewReader("{}"))
			rec := httptest.NewRecorder()
			s.handlePrepare(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			var resp PrepareResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.WorkspaceID != tc.wantID {
				t.Fatalf("workspace id = %q, want %q", resp.WorkspaceID, tc.wantID)
			}
			wantToken := s.workspaceToken(tc.wantID)
			if resp.WorkspaceToken != wantToken {
				t.Fatalf("workspace token = %q, want %q", resp.WorkspaceToken, wantToken)
			}
			if !s.tokens.authorizeWorkspace(WorkspaceAuthStrict, tc.wantID, resp.WorkspaceToken) {
				t.Fatal("returned workspace token does not authorize its workspace")
			}
		})
	}
}
