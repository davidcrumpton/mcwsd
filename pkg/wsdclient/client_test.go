package wsdclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientUsesWorkspaceTokenAfterPrepare(t *testing.T) {
	var execAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /workspaces", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer admin-secret" {
			t.Errorf("prepare authorization = %q", got)
		}
		json.NewEncoder(w).Encode(PrepareResponse{
			WorkspaceID:    "ws-a",
			WorkspaceToken: "wst_workspace_a",
		})
	})
	mux.HandleFunc("POST /workspaces/ws-a/exec", func(w http.ResponseWriter, r *http.Request) {
		execAuth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(ExecResponse{})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL, "admin-secret")
	if _, err := c.Prepare(context.Background(), PrepareRequest{}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := c.WorkspaceToken("ws-a"); got != "wst_workspace_a" {
		t.Fatalf("bound token = %q", got)
	}
	if _, err := c.Exec(context.Background(), "ws-a", ExecRequest{Command: "true"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if execAuth != "Bearer wst_workspace_a" {
		t.Fatalf("exec authorization = %q, want workspace token", execAuth)
	}
}

func TestWorkspaceClientIsBoundToOneWorkspace(t *testing.T) {
	var gotPath string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /workspaces/ws-a/exec", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewEncoder(w).Encode(ExecResponse{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL, "admin-secret")
	ws := c.ForWorkspace("ws-a")
	ws.SetToken("wst_workspace_a")
	if _, err := ws.Exec(context.Background(), ExecRequest{Command: "true"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if gotPath != "/workspaces/ws-a/exec" {
		t.Fatalf("path = %q", gotPath)
	}
}
