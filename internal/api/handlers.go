package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
	"github.com/davidcrumpton/mcwsd/internal/state"
)

// Server holds the HTTP server state.
type Server struct {
	backend   internal.WorkspaceBackend
	authToken string
	mux       *http.ServeMux
}

// NewServer creates a new API server.
func NewServer(backend internal.WorkspaceBackend, authToken string) *Server {
	s := &Server{
		backend:   backend,
		authToken: authToken,
		mux:       http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("POST /workspaces", s.handlePrepare)
	s.mux.HandleFunc("POST /workspaces/{id}/exec", s.handleExec)
	s.mux.HandleFunc("GET /workspaces/{id}/artifacts", s.handleArtifacts)
	s.mux.HandleFunc("POST /workspaces/{id}/commit-and-pr", s.handleCommitAndPR)
	s.mux.HandleFunc("POST /workspaces/{id}/safe-edit", s.handleSafeEdit)
	s.mux.HandleFunc("DELETE /workspaces/{id}", s.handleDestroy)
	s.mux.HandleFunc("GET /workspaces/{id}/status", s.handleStatus)
	s.mux.HandleFunc("GET /admin/leftovers", s.handleLeftovers)

	// File system tools
	s.mux.HandleFunc("GET /workspaces/{id}/files", s.handleListFiles)
	s.mux.HandleFunc("GET /workspaces/{id}/file", s.handleReadFile)
	s.mux.HandleFunc("PUT /workspaces/{id}/file", s.handleWriteFile)

	// Background jobs. /jobs is the canonical, AI-facing surface (the ID is
	// returned as job_id); /processes is the original alias and keeps working
	// unchanged. Both route sets share the same handlers.
	s.mux.HandleFunc("POST /workspaces/{id}/jobs", s.handleStartProcess)
	s.mux.HandleFunc("GET /workspaces/{id}/jobs", s.handleListProcesses)
	s.mux.HandleFunc("GET /workspaces/{id}/jobs/{jid}", s.handleGetProcessStatus)
	s.mux.HandleFunc("POST /workspaces/{id}/jobs/{jid}/input", s.handleSendProcessInput)
	s.mux.HandleFunc("DELETE /workspaces/{id}/jobs/{jid}", s.handleKillProcess)

	s.mux.HandleFunc("POST /workspaces/{id}/processes", s.handleStartProcess)
	s.mux.HandleFunc("GET /workspaces/{id}/processes", s.handleListProcesses)
	s.mux.HandleFunc("GET /workspaces/{id}/processes/{pid}", s.handleGetProcessStatus)
	s.mux.HandleFunc("POST /workspaces/{id}/processes/{pid}/input", s.handleSendProcessInput)
	s.mux.HandleFunc("DELETE /workspaces/{id}/processes/{pid}", s.handleKillProcess)
}

// processIDFromPath returns the job/process identifier from either the
// canonical /jobs/{jid} route or the legacy /processes/{pid} alias.
func processIDFromPath(r *http.Request) string {
	if v := r.PathValue("jid"); v != "" {
		return v
	}
	return r.PathValue("pid")
}

// leftoverLister is implemented by backends that track container creation
// in a state file (currently only the docker backend). It's kept as a
// narrow, optional interface rather than growing WorkspaceBackend, since a
// future backend without a state file shouldn't need a no-op implementation
// just to satisfy this.
type leftoverLister interface {
	Leftovers() []state.Entry
}

type correlationIDContextKey string

const requestCorrelationIDKey correlationIDContextKey = "request_correlation_id"

func requestCorrelationID(r *http.Request) string {
	if v := r.Header.Get("X-Correlation-ID"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Request-ID"); v != "" {
		return v
	}
	if v, ok := r.Context().Value(requestCorrelationIDKey).(string); ok && v != "" {
		return v
	}
	return ""
}

func requestCaller(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-User"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Remote-User"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Authenticated-User"); v != "" {
		return v
	}
	if r.Header.Get("Authorization") != "" {
		return "shared_bearer_token"
	}
	return "anonymous"
}

func requestWorkspaceID(r *http.Request) string {
	if v := r.PathValue("id"); v != "" {
		return v
	}
	if v := r.PathValue("workspace_id"); v != "" {
		return v
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.Split(path, "/")
	if len(parts) >= 2 && parts[0] == "workspaces" && parts[1] != "" {
		return parts[1]
	}
	return ""
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	correlationID := requestCorrelationID(r)
	if correlationID == "" {
		correlationID = fmt.Sprintf("wsd-%d", time.Now().UnixNano())
	}
	r = r.WithContext(context.WithValue(r.Context(), requestCorrelationIDKey, correlationID))
	recorder := &statusRecorder{ResponseWriter: w}
	recorder.Header().Set("X-Correlation-ID", correlationID)
	defer func() {
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		internal.Log.Debug("http_request",
			"method", r.Method,
			"path", strconv.Quote(r.URL.Path),
			"remote_addr", r.RemoteAddr,
			"status", status,
			"correlation_id", correlationID,
			"duration_ms", time.Since(started).Milliseconds())
		internal.Log.Audit("http_request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"workspace_id", requestWorkspaceID(r),
			"authenticated_caller", requestCaller(r),
			"correlation_id", correlationID,
			"remote_addr", r.RemoteAddr,
			"duration_ms", time.Since(started).Milliseconds())
	}()

	// Auth middleware
	if r.Header.Get("Authorization") != "Bearer "+s.authToken {
		http.Error(recorder, "unauthorized", http.StatusUnauthorized)
		internal.Log.Error("unauthorized_access",
			"remote_addr", r.RemoteAddr,
			"workspace_id", requestWorkspaceID(r),
			"authenticated_caller", requestCaller(r),
			"correlation_id", correlationID)
		return
	}
	s.mux.ServeHTTP(recorder, r)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusRecorder) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// maxRequestBodyBytes caps every JSON request body. It is large enough for a
// source file written via PUT /workspaces/{id}/file but small enough that a
// client cannot exhaust daemon memory by streaming an unbounded body.
const maxRequestBodyBytes = 10 << 20

// decodeJSONBody decodes exactly one JSON value from a size-limited body.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst interface{}) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	return json.NewDecoder(r.Body).Decode(dst)
}

// PrepareRequest is the body for POST /workspaces.
type PrepareRequest struct {
	RepoURL      string            `json:"repo_url"`
	Ref          string            `json:"ref,omitempty"`
	EnvOverrides map[string]string `json:"env_overrides,omitempty"`

	// Interface and IP4Address are required only by runtimes that need
	// explicit network configuration (FreeBSD jails). They are optional for
	// container runtimes such as Docker and Podman.
	Interface  string `json:"interface,omitempty"`
	IP4Address string `json:"ip4_address,omitempty"`

	// WaitSeconds promotes a still-running provision to a background job
	// after this many seconds (0 = daemon default). Async returns a job_id
	// at once; TimeoutSeconds bounds the whole pipeline (0 = daemon default).
	WaitSeconds    int  `json:"wait_seconds,omitempty"`
	Async          bool `json:"async,omitempty"`
	TimeoutSeconds int  `json:"timeout_seconds,omitempty"`
}

// PrepareResponse is the response for POST /workspaces. Either the finished
// workspace details are returned inline, or — when provisioning was
// backgrounded — a workspace_id/job_id/status the caller polls via the job API.
type PrepareResponse struct {
	WorkspaceID string                       `json:"workspace_id"`
	OS          string                       `json:"os,omitempty"`
	WorkDir     string                       `json:"work_dir,omitempty"`
	Profile     *internal.EnvironmentProfile `json:"profile,omitempty"`
	CacheHit    bool                         `json:"cache_hit"`
	JobID       string                       `json:"job_id,omitempty"`
	Status      string                       `json:"status,omitempty"`
}

func (s *Server) handlePrepare(w http.ResponseWriter, r *http.Request) {
	var req PrepareRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	spec := internal.WorkspaceSpec{
		RepoURL:      req.RepoURL,
		Ref:          req.Ref,
		EnvOverrides: req.EnvOverrides,
		Interface:    req.Interface,
		IP4Address:   req.IP4Address,
	}

	result, err := s.backend.Provision(r.Context(), internal.ProvisionRequest{
		WorkspaceSpec:  spec,
		WaitSeconds:    req.WaitSeconds,
		Async:          req.Async,
		TimeoutSeconds: req.TimeoutSeconds,
	})
	if err != nil {
		internal.Log.Error("provision_failed", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Backgrounded: report the job the caller must poll. The workspace does
	// not exist yet; its ID is reserved and returned so the job is addressable.
	if result.JobID != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(PrepareResponse{
			WorkspaceID: result.WorkspaceID,
			JobID:       result.JobID,
			Status:      string(result.Status),
		})
		return
	}

	ws := result.Workspace
	resp := PrepareResponse{
		WorkspaceID: ws.ID,
		OS:          ws.OS,
		WorkDir:     ws.WorkDir,
		Profile:     ws.Profile,
		CacheHit:    ws.CacheHit,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ExecRequest is the body for POST /workspaces/{id}/exec.
type ExecRequest struct {
	Command        string   `json:"command"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	Secrets        []string `json:"secrets,omitempty"`
	AllowNetwork   bool     `json:"allow_network,omitempty"`
	// WaitSeconds promotes a still-running command to a background job after
	// this many seconds (0 = daemon default). Async returns a job_id at once.
	WaitSeconds int  `json:"wait_seconds,omitempty"`
	Async       bool `json:"async,omitempty"`
}

// ExecResponse is the response for POST /workspaces/{id}/exec. Either inline
// output (stdout/stderr/exit_code) is returned, or — when the command was
// promoted to a background job — job_id/status with empty output.
type ExecResponse struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
	JobID      string `json:"job_id,omitempty"`
	Status     string `json:"status,omitempty"`
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req ExecRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	execReq := internal.ExecRequest{
		Command:        req.Command,
		TimeoutSeconds: req.TimeoutSeconds,
		Secrets:        req.Secrets,
		AllowNetwork:   req.AllowNetwork,
		WaitSeconds:    req.WaitSeconds,
		Async:          req.Async,
	}

	ws := &internal.Workspace{ID: id}
	result, err := s.backend.Exec(r.Context(), ws, execReq)
	if err != nil {
		internal.Log.Error("exec_failed", "workspace_id", id, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := ExecResponse{
		Stdout:     result.Stdout,
		Stderr:     result.Stderr,
		ExitCode:   result.ExitCode,
		DurationMs: result.DurationMs,
		JobID:      result.JobID,
		Status:     result.Status,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ArtifactsResponse is the response for GET /workspaces/{id}/artifacts.
type ArtifactsResponse struct {
	Artifacts []ArtifactInfo `json:"artifacts"`
}

type ArtifactInfo struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
}

func (s *Server) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	paths := r.URL.Query()["paths"]

	ws := &internal.Workspace{ID: id}
	artifacts, err := s.backend.Artifacts(r.Context(), ws, paths)
	if err != nil {
		internal.Log.Error("artifacts_failed", "workspace_id", id, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := ArtifactsResponse{
		Artifacts: make([]ArtifactInfo, len(artifacts)),
	}
	for i, a := range artifacts {
		resp.Artifacts[i] = ArtifactInfo{
			Name:    a.Name,
			Type:    a.Type,
			Content: string(a.Content),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

type SafeEditRequest struct {
	FilePath    string `json:"file_path"`
	Pattern     string `json:"pattern"`
	Replacement string `json:"replacement"`
}

type SafeEditResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

func (s *Server) handleSafeEdit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req SafeEditRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	ws := &internal.Workspace{ID: id}
	err := s.backend.SafeEdit(r.Context(), ws, req.FilePath, req.Pattern, req.Replacement)
	if err != nil {
		internal.Log.Error("safe_edit_failed", "workspace_id", id, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := SafeEditResponse{
		Success: true,
		Message: "Edit successful",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// CommitAndPRRequest is the body for POST /workspaces/{id}/commit-and-pr.
// Note there is no branch field — the branch name is chosen server-side
// (see backend.Backend.CommitAndPR) so a workspace can never target an
// existing branch or another workspace's branch. There is likewise no
// forge field: the forge is a daemon-level setting. repo_url may echo the
// workspace's repository (a mismatch is rejected) but cannot redirect the push;
// project_path/gitlab_project_path are accepted but ignored for compatibility.
type CommitAndPRRequest struct {
	CommitMessage     string `json:"commit_message"`
	PRTitle           string `json:"pr_title"`
	PRDescription     string `json:"pr_description,omitempty"`
	ProjectPath       string `json:"project_path,omitempty"`
	GitLabProjectPath string `json:"gitlab_project_path,omitempty"`
}

// CommitAndPRResponse is the response for POST /workspaces/{id}/commit-and-pr.
// Error carries a non-fatal reason the PR was not opened (e.g. "no changes to
// commit"); it is returned with HTTP 200 so the caller can surface it rather
// than seeing an empty branch/mr_url with no explanation. NoChangesDetail
// accompanies the no-changes case with a factual snapshot of the working tree.
type CommitAndPRResponse struct {
	Branch          string `json:"branch"`
	MRURL           string `json:"mr_url"`
	Error           string `json:"error,omitempty"`
	NoChangesDetail string `json:"no_changes_detail,omitempty"`
}

func (s *Server) handleCommitAndPR(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req CommitAndPRRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	ws := &internal.Workspace{ID: id}
	result, err := s.backend.CommitAndPR(r.Context(), ws, internal.CommitAndPRRequest{
		CommitMessage:     req.CommitMessage,
		PRTitle:           req.PRTitle,
		PRDescription:     req.PRDescription,
		ProjectPath:       req.ProjectPath,
		GitLabProjectPath: req.GitLabProjectPath,
	})
	if err != nil {
		internal.Log.Error("commit_and_pr_failed", "workspace_id", id, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := CommitAndPRResponse{
		Branch:          result.Branch,
		MRURL:           result.MRURL,
		Error:           result.Error,
		NoChangesDetail: result.NoChangesDetail,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleDestroy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	ws := &internal.Workspace{ID: id}
	if err := s.backend.Destroy(r.Context(), ws); err != nil {
		internal.Log.Error("destroy_failed", "workspace_id", id, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Simple liveness check - in production would check actual container state
	resp := map[string]interface{}{
		"workspace_id": id,
		"status":       "alive",
		"timestamp":    time.Now().Unix(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// LeftoverInfo describes one tracked container that a previous daemon run
// created but never destroyed.
type LeftoverInfo struct {
	WorkspaceID   string `json:"workspace_id"`
	ContainerName string `json:"container_name"`
	SessionID     string `json:"session_id"`
	RepoURL       string `json:"repo_url,omitempty"`
	CreatedAt     string `json:"created_at"`
}

// LeftoversResponse is the response for GET /admin/leftovers.
type LeftoversResponse struct {
	Leftovers []LeftoverInfo `json:"leftovers"`
}

// handleLeftovers reports containers tracked in the state file whose
// session ID doesn't match the currently running daemon — i.e. containers
// a previous run created but never got to Destroy before it was killed or
// aborted. Each one can be removed with the normal DELETE /workspaces/{id}
// call; Destroy falls back to the state file for workspaces that aren't in
// this process's in-memory map.
func (s *Server) handleLeftovers(w http.ResponseWriter, r *http.Request) {
	lister, ok := s.backend.(leftoverLister)
	if !ok {
		http.Error(w, "backend does not support leftover tracking", http.StatusNotImplemented)
		return
	}

	entries := lister.Leftovers()
	resp := LeftoversResponse{Leftovers: make([]LeftoverInfo, len(entries))}
	for i, e := range entries {
		resp.Leftovers[i] = LeftoverInfo{
			WorkspaceID:   e.WorkspaceID,
			ContainerName: e.ContainerName,
			SessionID:     e.SessionID,
			RepoURL:       e.RepoURL,
			CreatedAt:     e.CreatedAt.Format(time.RFC3339),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// --- File System Tool Handlers ---

// ListFilesResponse is the response for GET /workspaces/{id}/files.
type ListFilesResponse struct {
	Entries []internal.FileEntry `json:"entries"`
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path query parameter is required", http.StatusBadRequest)
		return
	}

	ws := &internal.Workspace{ID: id}
	result, err := s.backend.ListFiles(r.Context(), ws, path)
	if err != nil {
		internal.Log.Error("list_files_failed", "workspace_id", id, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := ListFilesResponse{Entries: result.Entries}
	if resp.Entries == nil {
		resp.Entries = []internal.FileEntry{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ReadFileResponse is the response for GET /workspaces/{id}/file.
type ReadFileResponse struct {
	Content string `json:"content"`
}

func (s *Server) handleReadFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path query parameter is required", http.StatusBadRequest)
		return
	}

	ws := &internal.Workspace{ID: id}
	result, err := s.backend.ReadFile(r.Context(), ws, path)
	if err != nil {
		internal.Log.Error("read_file_failed", "workspace_id", id, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ReadFileResponse{Content: result.Content})
}

// WriteFileRequest is the body for PUT /workspaces/{id}/file.
type WriteFileRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (s *Server) handleWriteFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req WriteFileRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	ws := &internal.Workspace{ID: id}
	if err := s.backend.WriteFile(r.Context(), ws, req.Path, req.Content); err != nil {
		internal.Log.Error("write_file_failed", "workspace_id", id, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- Background Process Management Handlers ---

// StartProcessRequest is the body for POST /workspaces/{id}/jobs (and its
// /processes alias).
type StartProcessRequest struct {
	Command        string `json:"command"`
	AllowNetwork   bool   `json:"allow_network,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

// StartProcessResponse is the response for POST /workspaces/{id}/jobs. JobID
// is the canonical field; ID is retained for older clients.
type StartProcessResponse struct {
	JobID   string `json:"job_id"`
	ID      string `json:"id"`
	Command string `json:"command"`
	Status  string `json:"status"`
}

func (s *Server) handleStartProcess(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req StartProcessRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Command == "" {
		http.Error(w, "command is required", http.StatusBadRequest)
		return
	}

	ws := &internal.Workspace{ID: id}
	info, err := s.backend.StartProcess(r.Context(), ws, internal.StartProcessRequest{
		Command:        req.Command,
		AllowNetwork:   req.AllowNetwork,
		TimeoutSeconds: req.TimeoutSeconds,
	})
	if err != nil {
		internal.Log.Error("start_process_failed", "workspace_id", id, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(StartProcessResponse{
		JobID:   info.ID,
		ID:      info.ID,
		Command: info.Command,
		Status:  string(info.Status),
	})
}

// ListProcessesResponse is the response for GET /workspaces/{id}/jobs. Jobs
// is the canonical field; Processes is retained for older clients and carries
// the same entries.
type ListProcessesResponse struct {
	Jobs      []ProcessInfo `json:"jobs"`
	Processes []ProcessInfo `json:"processes"`
}

// ProcessInfo is the API representation of a background job.
type ProcessInfo struct {
	JobID    string `json:"job_id"`
	ID       string `json:"id"`
	Command  string `json:"command"`
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code,omitempty"`
}

func (s *Server) handleListProcesses(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	ws := &internal.Workspace{ID: id}
	procs, err := s.backend.ListProcesses(r.Context(), ws)
	if err != nil {
		internal.Log.Error("list_processes_failed", "workspace_id", id, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	infos := make([]ProcessInfo, len(procs))
	for i, p := range procs {
		infos[i] = ProcessInfo{
			JobID:    p.ID,
			ID:       p.ID,
			Command:  p.Command,
			Status:   string(p.Status),
			ExitCode: p.ExitCode,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ListProcessesResponse{Jobs: infos, Processes: infos})
}

// ProcessStatusResponse is the response for GET /workspaces/{id}/processes/{pid}.
type ProcessStatusResponse struct {
	ProcessInfo
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	// Output is the combined, timestamped JSON-lines log produced since the
	// requested cursor. Cursor is the value to pass back on the next poll.
	Output string `json:"output,omitempty"`
	Cursor string `json:"cursor,omitempty"`
	// Workspace is set once a background provisioning job succeeds; it
	// carries the finished workspace's details.
	Workspace *PrepareResponse `json:"workspace,omitempty"`
}

func (s *Server) handleGetProcessStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	pid := processIDFromPath(r)

	// "since" is accepted as an alias for "cursor" for convenience.
	cursor := r.URL.Query().Get("cursor")
	if cursor == "" {
		cursor = r.URL.Query().Get("since")
	}

	ws := &internal.Workspace{ID: id}
	result, err := s.backend.GetProcessStatus(r.Context(), ws, pid, cursor)
	if err != nil {
		internal.Log.Error("get_process_status_failed", "workspace_id", id, "job_id", pid, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ProcessStatusResponse{
		ProcessInfo: ProcessInfo{
			JobID:    result.ID,
			ID:       result.ID,
			Command:  result.Command,
			Status:   string(result.Status),
			ExitCode: result.ExitCode,
		},
		Stdout:    result.Stdout,
		Stderr:    result.Stderr,
		Output:    result.Output,
		Cursor:    result.Cursor,
		Workspace: prepareResponseFromWorkspace(result.Workspace),
	})
}

// prepareResponseFromWorkspace renders a finished workspace in the same shape
// POST /workspaces returns, so a client polling a provisioning job receives
// identical details to an inline provision.
func prepareResponseFromWorkspace(ws *internal.Workspace) *PrepareResponse {
	if ws == nil {
		return nil
	}
	return &PrepareResponse{
		WorkspaceID: ws.ID,
		OS:          ws.OS,
		WorkDir:     ws.WorkDir,
		Profile:     ws.Profile,
		CacheHit:    ws.CacheHit,
	}
}

// handleSendProcessInput reports that interactive stdin is not supported for
// background jobs. It is kept as an explicit 501 rather than a 404 so clients
// that still call it get a clear reason instead of "no such route".
func (s *Server) handleSendProcessInput(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "interactive stdin is not supported for background jobs", http.StatusNotImplemented)
}

func (s *Server) handleKillProcess(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	pid := processIDFromPath(r)

	ws := &internal.Workspace{ID: id}
	if err := s.backend.KillProcess(r.Context(), ws, pid); err != nil {
		internal.Log.Error("kill_process_failed", "workspace_id", id, "job_id", pid, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
