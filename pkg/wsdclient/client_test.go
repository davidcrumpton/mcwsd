package wsdclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// Artifacts must forward the requested paths to the daemon, which reads them
// from repeated ?paths= query parameters. A previous version accepted the
// argument and silently dropped it, so callers asking for one file got the
// whole workspace.
func TestArtifactsForwardsPathsAsQueryParams(t *testing.T) {
	var gotQuery url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("GET /workspaces/ws-a/artifacts", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		json.NewEncoder(w).Encode(ArtifactsResponse{
			Artifacts: []Artifact{{Name: "main.go", Type: "diff"}},
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL, "admin-secret")
	resp, err := c.Artifacts(context.Background(), "ws-a", []string{"main.go", "go.mod"})
	if err != nil {
		t.Fatalf("Artifacts: %v", err)
	}
	if len(resp.Artifacts) != 1 || resp.Artifacts[0].Name != "main.go" {
		t.Fatalf("artifacts = %+v", resp.Artifacts)
	}
	if got := gotQuery["paths"]; len(got) != 2 || got[0] != "main.go" || got[1] != "go.mod" {
		t.Fatalf("paths query = %v, want [main.go go.mod]", got)
	}
}

// With no paths, the client must not send a paths parameter at all.
func TestArtifactsOmitsPathsWhenEmpty(t *testing.T) {
	var rawQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /workspaces/ws-a/artifacts", func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		json.NewEncoder(w).Encode(ArtifactsResponse{})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL, "admin-secret")
	if _, err := c.Artifacts(context.Background(), "ws-a", nil); err != nil {
		t.Fatalf("Artifacts: %v", err)
	}
	if rawQuery != "" {
		t.Fatalf("raw query = %q, want empty", rawQuery)
	}
}
