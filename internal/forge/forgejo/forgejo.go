package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/forge"
)

type Forge struct {
	APIBase     string
	Token       string
	ProjectPath string
}

func New(apiBase, token, projectPath string) *Forge {
	return &Forge{APIBase: apiBase, Token: token, ProjectPath: projectPath}
}

var _ forge.Forge = (*Forge)(nil)

func (f *Forge) Name() string { return "forgejo" }

func (f *Forge) OpenMergeRequest(ctx context.Context, req forge.OpenMRRequest) (*forge.OpenMRResult, error) {
	base := req.BaseBranch
	if base == "" {
		base = "main"
	}

	projectPath := req.ProjectPath
	if projectPath == "" {
		projectPath = f.ProjectPath
	}
	if projectPath == "" {
		projectPath = extractProjectPathFromURL(req.RepoURL)
	}
	if projectPath == "" {
		return nil, fmt.Errorf("forgejo project path is required (provide via project path, forge config, or repo URL)")
	}

	payload := map[string]string{
		"title": req.Title,
		"head":  req.Branch,
		"base":  base,
		"body":  req.Description,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/repos/%s/pulls", strings.TrimRight(f.APIBase, "/"), strings.TrimPrefix(projectPath, "/"))
	if strings.Contains(f.APIBase, "/api/v1") {
		endpoint = fmt.Sprintf("%s/repos/%s/pulls", strings.TrimRight(f.APIBase, "/"), strings.TrimPrefix(projectPath, "/"))
	} else {
		endpoint = fmt.Sprintf("%s/api/v1/repos/%s/pulls", strings.TrimRight(f.APIBase, "/"), strings.TrimPrefix(projectPath, "/"))
	}
	var resp *http.Response
	var bodyBytes []byte
	for i := 0; i < 5; i++ {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "token "+f.Token)
		resp, err = http.DefaultClient.Do(httpReq)
		if err != nil {
			return nil, err
		}
		bodyBytes, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted {
			break
		}
		if resp.StatusCode == http.StatusUnprocessableEntity && strings.Contains(string(bodyBytes), "head ref") {
			internal.Log.Debug("DEBUG_forgejo_pr_retry", "attempt", i+1, "status", resp.StatusCode)
			time.Sleep(2 * time.Second)
			continue
		}
		break
	}
	if resp == nil || (resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted) {
		if resp != nil {
			return nil, fmt.Errorf("forgejo API returned %d: %s", resp.StatusCode, string(bodyBytes))
		}
		return nil, fmt.Errorf("forgejo API request failed")
	}
	var result struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return nil, err
	}
	return &forge.OpenMRResult{URL: result.HTMLURL}, nil
}

func extractProjectPathFromURL(repoURL string) string {
	if repoURL == "" {
		return ""
	}
	repoURL = strings.TrimSuffix(repoURL, ".git")
	if strings.HasPrefix(repoURL, "git@") {
		path := strings.TrimPrefix(repoURL, "git@")
		if idx := strings.Index(path, ":"); idx != -1 {
			return strings.TrimPrefix(path[idx+1:], "/")
		}
		return ""
	}
	if strings.HasPrefix(repoURL, "http://") || strings.HasPrefix(repoURL, "https://") {
		u, err := url.Parse(repoURL)
		if err != nil {
			return ""
		}
		return strings.TrimPrefix(u.Path, "/")
	}
	if strings.Contains(repoURL, "/") && !strings.Contains(repoURL, "@") && !strings.HasPrefix(repoURL, "http") {
		return repoURL
	}
	return ""
}
