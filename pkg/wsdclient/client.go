package wsdclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Client is a Go client for the WSD daemon API.
type Client struct {
	baseURL   string
	authToken string

	mu              sync.RWMutex
	workspaceTokens map[string]string

	httpClient *http.Client
}

// NewClient creates a new WSD client. authToken is the admin/control-plane
// token; workspace tokens are learned from Prepare responses or bound with
// SetWorkspaceToken.
func NewClient(baseURL, authToken string) *Client {
	return &Client{
		baseURL:         baseURL,
		authToken:       authToken,
		workspaceTokens: make(map[string]string),
		httpClient:      &http.Client{Timeout: 60 * time.Second},
	}
}

// SetWorkspaceToken binds token to workspaceID. Subsequent workspace-scoped
// calls for that workspace use the workspace token instead of the admin token.
// An empty token clears the binding.
func (c *Client) SetWorkspaceToken(workspaceID, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if token == "" {
		delete(c.workspaceTokens, workspaceID)
		return
	}
	c.workspaceTokens[workspaceID] = token
}

// WorkspaceToken returns the bound token for workspaceID, if any.
func (c *Client) WorkspaceToken(workspaceID string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.workspaceTokens[workspaceID]
}

func (c *Client) tokenForWorkspace(workspaceID string) string {
	if token := c.WorkspaceToken(workspaceID); token != "" {
		return token
	}
	return c.authToken
}

// PrepareRequest is the request body for Prepare().
type PrepareRequest struct {
	RepoURL      string            `json:"repo_url"`
	Ref          string            `json:"ref,omitempty"`
	EnvOverrides map[string]string `json:"env_overrides,omitempty"`
	// WaitSeconds promotes a still-running provision to a background job after
	// this many seconds; Async returns a job id immediately.
	WaitSeconds    int  `json:"wait_seconds,omitempty"`
	Async          bool `json:"async,omitempty"`
	TimeoutSeconds int  `json:"timeout_seconds,omitempty"`
}

// PrepareResponse is the response from Prepare(). Either the workspace details
// are returned inline, or — when provisioning was backgrounded — JobID/Status
// with empty workspace fields. Poll the job with GetJob; on success its
// Workspace field carries the finished workspace.
type PrepareResponse struct {
	WorkspaceID    string              `json:"workspace_id"`
	WorkspaceToken string              `json:"workspace_token,omitempty"`
	OS             string              `json:"os,omitempty"`
	WorkDir        string              `json:"work_dir,omitempty"`
	Profile        *EnvironmentProfile `json:"profile,omitempty"`
	CacheHit       bool                `json:"cache_hit"`
	JobID          string              `json:"job_id,omitempty"`
	Status         string              `json:"status,omitempty"`
}

// EnvironmentProfile describes the detected environment.
type EnvironmentProfile struct {
	Language          string `json:"language"`
	Version           string `json:"version"`
	PackageManager    string `json:"package_manager"`
	InstallCommand    string `json:"install_command"`
	BaseImage         string `json:"base_image"`
	PostCreateCommand string `json:"post_create_command,omitempty"`
}

// Prepare provisions a new workspace.
func (c *Client) Prepare(ctx context.Context, req PrepareRequest) (*PrepareResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	resp, err := c.doRequest(ctx, "POST", "/workspaces", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result PrepareResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if result.WorkspaceToken != "" {
		c.SetWorkspaceToken(result.WorkspaceID, result.WorkspaceToken)
	}

	return &result, nil
}

// ExecRequest is the request body for Exec().
type ExecRequest struct {
	Command        string   `json:"command"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	Secrets        []string `json:"secrets,omitempty"`
	AllowNetwork   bool     `json:"allow_network,omitempty"`
	// WaitSeconds promotes a still-running command to a background job after
	// this many seconds; Async returns a job id immediately.
	WaitSeconds int  `json:"wait_seconds,omitempty"`
	Async       bool `json:"async,omitempty"`
}

// ExecResponse is the response from Exec(). Either inline output is returned
// or, when the command was promoted to a background job, JobID/Status with
// empty output.
type ExecResponse struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
	JobID      string `json:"job_id,omitempty"`
	Status     string `json:"status,omitempty"`
}

// Exec runs a command in a workspace.
func (c *Client) Exec(ctx context.Context, workspaceID string, req ExecRequest) (*ExecResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	path := fmt.Sprintf("/workspaces/%s/exec", workspaceID)
	resp, err := c.doWorkspaceRequest(ctx, "POST", workspaceID, path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result ExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return &result, nil
}

// Artifact represents a file or patch artifact.
type Artifact struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
}

// ArtifactsResponse is the response from Artifacts().
type ArtifactsResponse struct {
	Artifacts []Artifact `json:"artifacts"`
}

// Artifacts retrieves artifacts (diffs and new files) from a workspace.
func (c *Client) Artifacts(ctx context.Context, workspaceID string, paths []string) (*ArtifactsResponse, error) {
	// url := fmt.Sprintf("%s/workspaces/%s/artifacts", c.baseURL, workspaceID)
	if len(paths) > 0 {
		// Add query params if paths specified
	}

	resp, err := c.doWorkspaceRequest(ctx, "GET", workspaceID, fmt.Sprintf("/workspaces/%s/artifacts", workspaceID), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result ArtifactsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return &result, nil
}

// CommitAndPRRequest is the request body for CommitAndPR(). There is
// deliberately no branch field: the daemon chooses the branch name. There is
// also no forge field: the forge is a daemon-level setting. RepoURL may echo
// the workspace's repository URL but cannot redirect the push; a mismatch is
// rejected. ProjectPath/GitLabProjectPath are accepted but ignored.
type CommitAndPRRequest struct {
	CommitMessage     string `json:"commit_message"`
	PRTitle           string `json:"pr_title"`
	PRDescription     string `json:"pr_description,omitempty"`
	ProjectPath       string `json:"project_path,omitempty"`
	GitLabProjectPath string `json:"gitlab_project_path,omitempty"`
}

// CommitAndPRResponse is the response from CommitAndPR(). Error is set when
// the daemon declined to open the PR for a non-fatal reason (e.g. there were
// no changes to commit); Branch and MRURL are empty in that case.
// NoChangesDetail carries a factual snapshot of the working tree when the
// no-changes case applies.
type CommitAndPRResponse struct {
	Branch          string `json:"branch"`
	MRURL           string `json:"mr_url"`
	Error           string `json:"error,omitempty"`
	NoChangesDetail string `json:"no_changes_detail,omitempty"`
}

// CommitAndPR commits the workspace's current changes, pushes them to a
// daemon-chosen branch, and opens a GitLab merge request.
func (c *Client) CommitAndPR(ctx context.Context, workspaceID string, req CommitAndPRRequest) (*CommitAndPRResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	path := fmt.Sprintf("/workspaces/%s/commit-and-pr", workspaceID)
	resp, err := c.doWorkspaceRequest(ctx, "POST", workspaceID, path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result CommitAndPRResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return &result, nil
}

// Destroy tears down a workspace.
func (c *Client) Destroy(ctx context.Context, workspaceID string) error {
	resp, err := c.doWorkspaceRequest(ctx, "DELETE", workspaceID, fmt.Sprintf("/workspaces/%s", workspaceID), nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Status checks the status of a workspace.
func (c *Client) Status(ctx context.Context, workspaceID string) (map[string]interface{}, error) {
	resp, err := c.doWorkspaceRequest(ctx, "GET", workspaceID, fmt.Sprintf("/workspaces/%s/status", workspaceID), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result, nil
}

// JobRequest is the request body for StartJob.
type JobRequest struct {
	Command        string `json:"command"`
	AllowNetwork   bool   `json:"allow_network,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

// JobInfo describes a background job.
type JobInfo struct {
	JobID    string `json:"job_id"`
	Command  string `json:"command"`
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code,omitempty"`
}

// JobResponse is returned by StartJob and GetJob. Output carries the combined,
// timestamped JSON-lines log produced since the requested cursor; pass Cursor
// back on the next GetJob call to receive only new output. Workspace is set
// once a provisioning job succeeds.
type JobResponse struct {
	JobInfo
	Output    string           `json:"output,omitempty"`
	Cursor    string           `json:"cursor,omitempty"`
	Workspace *PrepareResponse `json:"workspace,omitempty"`
}

// JobListResponse is returned by ListJobs.
type JobListResponse struct {
	Jobs []JobInfo `json:"jobs"`
}

// StartJob launches a command in the workspace as a background job and returns
// immediately with its job_id. The job runs detached; poll GetJob for output
// and status, or CancelJob to stop it.
func (c *Client) StartJob(ctx context.Context, workspaceID string, req JobRequest) (*JobResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/workspaces/%s/jobs", workspaceID)
	resp, err := c.doWorkspaceRequest(ctx, "POST", workspaceID, path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result JobResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ListJobs returns the workspace's tracked background jobs.
func (c *Client) ListJobs(ctx context.Context, workspaceID string) (*JobListResponse, error) {
	path := fmt.Sprintf("/workspaces/%s/jobs", workspaceID)
	resp, err := c.doWorkspaceRequest(ctx, "GET", workspaceID, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result JobListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetJob returns a job's status and the output produced since cursor. An empty
// cursor returns all output so far; pass the returned Cursor next time to get
// only new output.
func (c *Client) GetJob(ctx context.Context, workspaceID, jobID, cursor string) (*JobResponse, error) {
	path := fmt.Sprintf("/workspaces/%s/jobs/%s", workspaceID, jobID)
	if cursor != "" {
		path += "?cursor=" + url.QueryEscape(cursor)
	}
	resp, err := c.doWorkspaceRequest(ctx, "GET", workspaceID, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result JobResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if result.Workspace != nil && result.Workspace.WorkspaceToken != "" {
		c.SetWorkspaceToken(result.Workspace.WorkspaceID, result.Workspace.WorkspaceToken)
	}
	return &result, nil
}

// CancelJob terminates a background job. It is idempotent; the terminal record
// and its output remain queryable with GetJob.
func (c *Client) CancelJob(ctx context.Context, workspaceID, jobID string) error {
	path := fmt.Sprintf("/workspaces/%s/jobs/%s", workspaceID, jobID)
	resp, err := c.doWorkspaceRequest(ctx, "DELETE", workspaceID, path, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// LeftoverInfo describes one tracked container from a previous, not-
// cleanly-shut-down daemon run.
type LeftoverInfo struct {
	WorkspaceID   string `json:"workspace_id"`
	ContainerName string `json:"container_name"`
	SessionID     string `json:"session_id"`
	RepoURL       string `json:"repo_url,omitempty"`
	CreatedAt     string `json:"created_at"`
}

// LeftoversResponse is the response from Leftovers().
type LeftoversResponse struct {
	Leftovers []LeftoverInfo `json:"leftovers"`
}

// Leftovers lists containers a previous daemon run created but never
// destroyed (usually because it was killed or aborted mid-task). Each one
// can be removed with Destroy(workspaceID) — the daemon falls back to its
// state file for workspaces it doesn't have in memory.
func (c *Client) Leftovers(ctx context.Context) (*LeftoversResponse, error) {
	resp, err := c.doRequest(ctx, "GET", "/admin/leftovers", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result LeftoversResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return &result, nil
}

// doRequest issues a control-plane request with the admin token.
func (c *Client) doRequest(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	return c.doRequestWithToken(ctx, method, path, body, c.authToken)
}

// doWorkspaceRequest issues a workspace-scoped request with the workspace's
// bound token, falling back to the admin token when no token has been bound
// (which the daemon accepts only in scoped mode).
func (c *Client) doWorkspaceRequest(ctx context.Context, method, workspaceID, path string, body io.Reader) (*http.Response, error) {
	return c.doRequestWithToken(ctx, method, path, body, c.tokenForWorkspace(workspaceID))
}

func (c *Client) doRequestWithToken(ctx context.Context, method, path string, body io.Reader, token string) (*http.Response, error) {
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return c.httpClient.Do(req)
}

// WorkspaceClient is a Client bound to one workspace. Its methods deliberately
// take no workspace ID, so an agent-facing tool cannot be talked into
// targeting a different workspace.
type WorkspaceClient struct {
	client      *Client
	workspaceID string
}

// ForWorkspace returns a client bound to workspaceID. Bind its token with
// SetToken (or use the Client that created the workspace, which learns the
// token from Prepare automatically).
func (c *Client) ForWorkspace(workspaceID string) *WorkspaceClient {
	return &WorkspaceClient{client: c, workspaceID: workspaceID}
}

// WorkspaceID returns the bound workspace ID.
func (w *WorkspaceClient) WorkspaceID() string { return w.workspaceID }

// SetToken binds this workspace's token on the underlying client.
func (w *WorkspaceClient) SetToken(token string) {
	w.client.SetWorkspaceToken(w.workspaceID, token)
}

// Exec runs a command in the bound workspace.
func (w *WorkspaceClient) Exec(ctx context.Context, req ExecRequest) (*ExecResponse, error) {
	return w.client.Exec(ctx, w.workspaceID, req)
}

// Artifacts retrieves artifacts from the bound workspace.
func (w *WorkspaceClient) Artifacts(ctx context.Context, paths []string) (*ArtifactsResponse, error) {
	return w.client.Artifacts(ctx, w.workspaceID, paths)
}

// CommitAndPR commits and opens a PR for the bound workspace.
func (w *WorkspaceClient) CommitAndPR(ctx context.Context, req CommitAndPRRequest) (*CommitAndPRResponse, error) {
	return w.client.CommitAndPR(ctx, w.workspaceID, req)
}

// Destroy tears down the bound workspace.
func (w *WorkspaceClient) Destroy(ctx context.Context) error {
	return w.client.Destroy(ctx, w.workspaceID)
}

// Status checks the bound workspace's status.
func (w *WorkspaceClient) Status(ctx context.Context) (map[string]interface{}, error) {
	return w.client.Status(ctx, w.workspaceID)
}

// StartJob launches a background job in the bound workspace.
func (w *WorkspaceClient) StartJob(ctx context.Context, req JobRequest) (*JobResponse, error) {
	return w.client.StartJob(ctx, w.workspaceID, req)
}

// ListJobs lists the bound workspace's background jobs.
func (w *WorkspaceClient) ListJobs(ctx context.Context) (*JobListResponse, error) {
	return w.client.ListJobs(ctx, w.workspaceID)
}

// GetJob returns a job's status and output.
func (w *WorkspaceClient) GetJob(ctx context.Context, jobID, cursor string) (*JobResponse, error) {
	return w.client.GetJob(ctx, w.workspaceID, jobID, cursor)
}

// CancelJob terminates a background job in the bound workspace.
func (w *WorkspaceClient) CancelJob(ctx context.Context, jobID string) error {
	return w.client.CancelJob(ctx, w.workspaceID, jobID)
}
