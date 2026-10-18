package github

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

// Forge opens merge requests against a GitHub API-compatible instance.
type Forge struct {
	APIBase     string
	Token       string
	ProjectPath string
}

func New(apiBase, token, projectPath string) *Forge {
	// An empty API base would otherwise produce a relative request URL and a
	// confusing "unsupported protocol scheme" error at MR-creation time.
	// Default to public GitHub; callers targeting GitHub Enterprise pass an
	// explicit base.
	if strings.TrimSpace(apiBase) == "" {
		apiBase = "https://api.github.com"
	}
	return &Forge{APIBase: apiBase, Token: token, ProjectPath: projectPath}
}

var _ forge.Forge = (*Forge)(nil)

// prCreateRetryDelay is the pause between create-pull-request attempts when
// GitHub reports a transient ref-propagation error. It is a variable so tests
// can remove the wait.
var prCreateRetryDelay = 2 * time.Second

func (f *Forge) Name() string { return "github" }

func (f *Forge) OpenMergeRequest(ctx context.Context, req forge.OpenMRRequest) (*forge.OpenMRResult, error) {
	projectPath := req.ProjectPath
	if projectPath == "" {
		projectPath = f.ProjectPath
	}
	if projectPath == "" {
		projectPath = extractProjectPathFromURL(req.RepoURL)
	}
	if projectPath == "" {
		return nil, fmt.Errorf("github project path is required (provide via project path, forge config, or repo URL)")
	}

	base := req.BaseBranch
	if base == "" {
		// A workspace provisioned without an explicit ref was cloned from the
		// repo's default branch, so that is the branch a PR must target.
		// Assuming "main" rejects PRs for repos whose default is "master"
		// (and vice versa) with a confusing "not all refs are readable".
		if db, err := f.defaultBranch(ctx, projectPath); err == nil && db != "" {
			base = db
		} else {
			base = "main"
		}
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

	endpoint := fmt.Sprintf("%s/repos/%s/pulls", strings.TrimRight(f.APIBase, "/"), url.PathEscape(projectPath))
	if strings.Contains(projectPath, "/") {
		endpoint = fmt.Sprintf("%s/repos/%s/pulls", strings.TrimRight(f.APIBase, "/"), strings.TrimPrefix(projectPath, "/"))
	}

	const maxPRCreateAttempts = 5
	var resp *http.Response
	var bodyBytes []byte
	for i := 0; i < maxPRCreateAttempts; i++ {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+f.Token)

		resp, err = http.DefaultClient.Do(httpReq)
		if err != nil {
			return nil, err
		}
		bodyBytes, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted {
			break
		}
		// Transient: GitHub can reject a create-PR immediately after a push
		// because its ref cache hasn't caught up yet ("not all refs are
		// readable", "head ref ..."). Retry rather than failing a PR that
		// will succeed a moment later.
		if resp.StatusCode == http.StatusUnprocessableEntity && isRetryablePRCreateError(bodyBytes) {
			internal.Log.Info("github_pr_create_retry", "attempt", i+1, "max", maxPRCreateAttempts,
				"body", strings.TrimSpace(string(bodyBytes)))
			time.Sleep(prCreateRetryDelay)
			continue
		}
		// Already created (a prior attempt, or the caller retrying). Resolve
		// and return it instead of reporting a failure for a PR the user can
		// already see.
		if resp.StatusCode == http.StatusUnprocessableEntity && isPRAlreadyExistsError(bodyBytes) {
			if existingURL, lookupErr := f.findExistingPullRequest(ctx, projectPath, req.Branch); lookupErr == nil && existingURL != "" {
				return &forge.OpenMRResult{URL: existingURL}, nil
			}
		}
		break
	}

	if resp == nil || (resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted) {
		if resp != nil {
			// The create call can still report a transient ref error after the
			// PR was actually created; check for an existing PR before
			// failing, so a retry is idempotent.
			if resp.StatusCode == http.StatusUnprocessableEntity && isRetryablePRCreateError(bodyBytes) {
				if existingURL, lookupErr := f.findExistingPullRequest(ctx, projectPath, req.Branch); lookupErr == nil && existingURL != "" {
					return &forge.OpenMRResult{URL: existingURL}, nil
				}
			}
			msg := fmt.Sprintf("github API returned %d for POST %s: %s", resp.StatusCode, endpoint, string(bodyBytes))
			if resp.StatusCode == http.StatusNotFound {
				// GitHub returns 404 (not 403) for private repos the token
				// cannot see, so "Not Found" almost always means the token is
				// missing, invalid, or lacks access — not that the branch or
				// repo is absent.
				msg += " (GitHub reports 404 for private repositories the token cannot access; verify the token is set, has access to this repo, and can open pull requests)"
			}
			if resp.StatusCode == http.StatusUnprocessableEntity && isRetryablePRCreateError(bodyBytes) {
				// Persisted past every retry, so it is not the post-push race.
				// Report which refs the token can actually see to make the
				// cause actionable (usually a wrong base branch, or a token
				// without Contents: read).
				msg += " (" + f.describeRefReadability(ctx, projectPath, base, req.Branch) + ")"
				msg += "; for private repositories the token needs Contents: Read (to resolve refs) and Pull requests: Write"
			}
			return nil, fmt.Errorf("%s", msg)
		}
		return nil, fmt.Errorf("github API request failed")
	}

	var result struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return nil, err
	}
	return &forge.OpenMRResult{URL: result.HTMLURL}, nil
}

// isRetryablePRCreateError reports whether a 422 from the create-pull-request
// endpoint is a transient ref-propagation race rather than a real validation
// failure. GitHub returns "not all refs are readable" (and occasionally
// "head ref ...") when the branch has been pushed but its ref is not yet
// visible to the pull-request service; retrying after a short delay succeeds.
func isRetryablePRCreateError(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "not all refs are readable") ||
		strings.Contains(s, "head ref")
}

// isPRAlreadyExistsError reports whether a 422 means a pull request for this
// head branch already exists, in which case it should be treated as success.
func isPRAlreadyExistsError(body []byte) bool {
	return strings.Contains(strings.ToLower(string(body)), "already exists")
}

// findExistingPullRequest returns the HTML URL of an existing pull request
// whose head is <owner>:<branch>, or "" if there is none. It is used to make
// create-pull-request idempotent: a transient 422 or an "already exists"
// response should resolve to the PR rather than an error.
func (f *Forge) findExistingPullRequest(ctx context.Context, projectPath, branch string) (string, error) {
	owner := projectPath
	if i := strings.Index(projectPath, "/"); i != -1 {
		owner = projectPath[:i]
	}
	endpoint := fmt.Sprintf("%s/repos/%s/pulls?state=all&head=%s",
		strings.TrimRight(f.APIBase, "/"),
		strings.TrimPrefix(projectPath, "/"),
		url.QueryEscape(owner+":"+branch))

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Authorization", "Bearer "+f.Token)
	httpReq.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github list pulls returned %d", resp.StatusCode)
	}

	var pulls []struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pulls); err != nil {
		return "", err
	}
	if len(pulls) == 0 {
		return "", nil
	}
	return pulls[0].HTMLURL, nil
}

// defaultBranch returns the repository's default branch (e.g. "main" or
// "master") as GitHub reports it, or "" if it can't be determined.
func (f *Forge) defaultBranch(ctx context.Context, projectPath string) (string, error) {
	endpoint := fmt.Sprintf("%s/repos/%s", strings.TrimRight(f.APIBase, "/"), strings.TrimPrefix(projectPath, "/"))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Authorization", "Bearer "+f.Token)
	httpReq.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github get repo returned %d", resp.StatusCode)
	}
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&repo); err != nil {
		return "", err
	}
	return repo.DefaultBranch, nil
}

// refExists reports whether a branch with the given name exists and is
// readable with the configured token.
func (f *Forge) refExists(ctx context.Context, projectPath, branch string) (bool, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/branches/%s",
		strings.TrimRight(f.APIBase, "/"),
		strings.TrimPrefix(projectPath, "/"),
		url.PathEscape(branch))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+f.Token)
	httpReq.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("GET branch %q returned %d: %s", branch, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return true, nil
}

// describeRefReadability summarizes, for an error message, whether the token
// can see the repo's default branch, the PR base, and the PR head. It is
// called only after "not all refs are readable" has persisted past every
// retry, when the cause is usually a wrong base branch or a token without
// Contents: read.
func (f *Forge) describeRefReadability(ctx context.Context, projectPath, base, head string) string {
	var b strings.Builder
	b.WriteString("could not resolve PR refs")
	if db, err := f.defaultBranch(ctx, projectPath); err == nil && db != "" {
		fmt.Fprintf(&b, "; repo default branch=%q", db)
	} else if err != nil {
		fmt.Fprintf(&b, "; repo lookup failed: %v", err)
	}
	if base != "" {
		if ok, err := f.refExists(ctx, projectPath, base); err != nil {
			fmt.Fprintf(&b, "; base %q check failed: %v", base, err)
		} else {
			fmt.Fprintf(&b, "; base %q readable=%t", base, ok)
		}
	}
	if head != "" {
		if ok, err := f.refExists(ctx, projectPath, head); err != nil {
			fmt.Fprintf(&b, "; head %q check failed: %v", head, err)
		} else {
			fmt.Fprintf(&b, "; head %q readable=%t", head, ok)
		}
	}
	return b.String()
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
