// Package gitlab implements the forge.Forge interface for GitLab, via its
// REST API. This is the only forge implementation for now — see
// internal/forge for the interface future providers (GitHub, Gitea,
// Forgejo) will implement.
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/forge"
)

// Forge opens merge requests against a GitLab instance's REST API.
type Forge struct {
	APIBase     string // e.g. https://gitlab.crumpton.org/api/v4
	Token       string
	ProjectPath string // fallback URL-encoded "namespace/project" path, or numeric project ID
}

// New creates a GitLab Forge.
func New(apiBase, token, projectPath string) *Forge {
	return &Forge{APIBase: apiBase, Token: token, ProjectPath: projectPath}
}

var _ forge.Forge = (*Forge)(nil)

// Name identifies this forge.
func (f *Forge) Name() string { return "gitlab" }

// OpenMergeRequest opens a GitLab merge request via the REST API, retrying
// briefly if the source branch isn't visible yet due to eventual
// consistency right after a push.
func (f *Forge) OpenMergeRequest(ctx context.Context, req forge.OpenMRRequest) (*forge.OpenMRResult, error) {
	base := req.BaseBranch
	if base == "" {
		base = "main"
	}

	payload := map[string]string{
		"source_branch": req.Branch,
		"target_branch": base,
		"title":         req.Title,
		"description":   req.Description,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	// Determine project path: per-request > forge config > auto-extract from RepoURL
	projectPath := req.ProjectPath
	if projectPath == "" {
		projectPath = f.ProjectPath
	}
	if projectPath == "" {
		projectPath = extractProjectPathFromURL(req.RepoURL)
	}
	if projectPath == "" {
		return nil, fmt.Errorf("gitlab project path is required (provide via -gitlab-project-path flag, gitlab_project_path request field, or use a recognizable repo URL)")
	}

	endpoint := fmt.Sprintf("%s/projects/%s/merge_requests",
		strings.TrimRight(f.APIBase, "/"),
		url.PathEscape(projectPath))

	// --- DEBUG BLOCK START ---
	internal.Log.Debug("DEBUG_gitlab_mr_request", "endpoint", endpoint, "payload", string(body))
	// --- DEBUG BLOCK END ---

	var resp *http.Response
	var bodyBytes []byte
	var doErr error

	for i := 0; i < 5; i++ {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("PRIVATE-TOKEN", f.Token)

		resp, doErr = forge.HTTPClient.Do(httpReq)
		if doErr != nil {
			return nil, doErr
		}

		bodyBytes, _ = forge.ReadResponseBody(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusCreated {
			break // Success
		}

		// Eventual consistency check: if the branch was literally just pushed,
		// the MR API might briefly return 400 with "source_branch does not exist".
		if resp.StatusCode == http.StatusBadRequest && strings.Contains(string(bodyBytes), "source_branch") && strings.Contains(string(bodyBytes), "does not exist") {
			internal.Log.Debug("DEBUG_gitlab_mr_retry", "attempt", i+1, "status", resp.StatusCode)
			if err := forge.SleepContext(ctx, 3*time.Second); err != nil {
				return nil, err
			}
			continue
		}

		break // Any other error, do not retry
	}

	if resp.StatusCode != http.StatusCreated {
		// --- DEBUG BLOCK START ---
		internal.Log.Debug("DEBUG_gitlab_mr_error_response", "status", resp.StatusCode, "body", string(bodyBytes))
		// --- DEBUG BLOCK END ---
		return nil, fmt.Errorf("gitlab API returned %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		WebURL string `json:"web_url"`
	}
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return nil, err
	}
	return &forge.OpenMRResult{URL: result.WebURL}, nil
}

// extractProjectPathFromURL attempts to extract a GitLab project path from
// a git repository URL. Supports both SSH and HTTPS formats:
//   - git@gitlab.example.org:namespace/project.git
//   - git@gitlab.example.org:namespace/project
//   - https://gitlab.example.org/namespace/project.git
//   - https://gitlab.example.org/namespace/project
//
// Returns the URL-encoded "namespace/project" path, or empty string if extraction fails.
func extractProjectPathFromURL(repoURL string) string {
	if repoURL == "" {
		return ""
	}

	// Remove .git suffix if present
	repoURL = strings.TrimSuffix(repoURL, ".git")

	// Handle SSH URLs: git@gitlab.example.org:namespace/project
	if strings.HasPrefix(repoURL, "git@") {
		// Remove git@ prefix
		path := strings.TrimPrefix(repoURL, "git@")
		// Find the first colon to split host and path
		if idx := strings.Index(path, ":"); idx != -1 {
			path = path[idx+1:]
			// Remove leading slash if present
			path = strings.TrimPrefix(path, "/")
			return path
		}
		return ""
	}

	// Handle HTTPS URLs: https://gitlab.example.org/namespace/project
	if strings.HasPrefix(repoURL, "http://") || strings.HasPrefix(repoURL, "https://") {
		u, err := url.Parse(repoURL)
		if err != nil {
			return ""
		}
		path := strings.TrimPrefix(u.Path, "/")
		return path
	}

	// If it looks like just a path (namespace/project), use it directly
	if strings.Contains(repoURL, "/") && !strings.Contains(repoURL, "@") && !strings.HasPrefix(repoURL, "http") {
		return repoURL
	}

	return ""
}
