# API Reference

This document describes the API provided by the Workspace Daemon.  The API is designed to be simple and easy to use for AI clients.  The APIExisting field names and routes are treated as the contract for the initial public API; future additions will be additive and backwards-compatible.

The daemon exposes a JSON-over-HTTP API for creating and managing temporary workspaces. All endpoints require a bearer token in the `Authorization` header:

```http
Authorization: Bearer <token>
```

On every response, the daemon also includes `X-Correlation-ID` for request tracing.

## Base URL

The daemon listens on the configured bind address and port. A typical local deployment is:

```text
http://127.0.0.1:9080
```

## Request and response conventions

- Content type for JSON requests/responses is `application/json`.
- Successful JSON responses return structured objects.
- Empty-success endpoints return HTTP 204 No Content.
- Errors are plain-text HTTP error bodies with an HTTP status code. Unauthorized requests return 401.
- For provisioning and commands that can run longer than the request timeout, the daemon may return a background `job_id` and the caller polls the job status endpoint.

## Workspace lifecycle

### POST /workspaces

Create or prepare a workspace from a repository.

Request body:

```json
{
  "repo_url": "https://github.com/example/project",
  "ref": "main",
  "env_overrides": {
    "NODE_ENV": "test"
  },
  "interface": "vnet0",
  "ip4_address": "10.0.0.42",
  "wait_seconds": 30,
  "async": false,
  "timeout_seconds": 600
}
```

Fields:

- `repo_url` (required): repository to clone into the workspace
- `ref` (optional): branch, tag, or commit to check out
- `env_overrides` (optional): environment variables applied inside the workspace
- `interface` / `ip4_address` (optional): runtime-specific network settings; primarily used for FreeBSD jails
- `wait_seconds` (optional): block up to this many seconds before promoting the provision to a background job
- `async` (optional): immediately return a `job_id` and continue provisioning in the background
- `timeout_seconds` (optional): cap the whole provision pipeline

Success response (inline completion):

```json
{
  "workspace_id": "ws-123",
  "os": "linux",
  "work_dir": "/workspace/repo",
  "profile": {
    "language": "go",
    "version": "1.23.3",
    "package_manager": "go",
    "install_command": "go install ./...",
    "base_image": "ubuntu:24.04",
    "system_package_manager": "apt",
    "system_install_command": "apt-get install -y",
    "notes": "Detected Go project"
  },
  "cache_hit": false
}
```

Success response (backgrounded):

```json
{
  "workspace_id": "ws-123",
  "job_id": "job-456",
  "status": "queued"
}
```

### DELETE /workspaces/{id}

Destroy a workspace and clean up its runtime resources.

Response:

- HTTP 204 No Content on success.

### GET /workspaces/{id}/status

Liveness check for a workspace.

Success response:

```json
{
  "workspace_id": "ws-123",
  "status": "alive",
  "timestamp": 1730000000
}
```

## Command execution

### POST /workspaces/{id}/exec

Run a command inside a workspace.

Request body:

```json
{
  "command": "go test ./...",
  "timeout_seconds": 180,
  "secrets": ["api-token"],
  "allow_network": true,
  "wait_seconds": 10,
  "async": false
}
```

Fields:

- `command` (required): shell command to execute inside the workspace
- `timeout_seconds` (optional): command deadline in seconds
- `secrets` (optional): short-lived secret references; values are not sent in the request body
- `allow_network` (optional): allow network access for this execution
- `wait_seconds` (optional): block before promoting to a background job
- `async` (optional): start a background job immediately

Success response (inline execution):

```json
{
  "stdout": "ok\n",
  "stderr": "",
  "exit_code": 0,
  "duration_ms": 1842
}
```

Success response (backgrounded):

```json
{
  "stdout": "",
  "stderr": "",
  "exit_code": 0,
  "duration_ms": 0,
  "job_id": "job-789",
  "status": "queued"
}
```

## Artifacts and edits

### GET /workspaces/{id}/artifacts

Return one or more artifact files or patches from the workspace.

Query parameters:

- `paths`: repeatable query parameter for artifact paths to fetch

Example:

```bash
curl "http://127.0.0.1:9080/workspaces/ws-123/artifacts?paths=dist/app.tar.gz&paths=diff.patch"
```

Response:

```json
{
  "artifacts": [
    {
      "name": "dist/app.tar.gz",
      "type": "file",
      "content": "base64-encoded-content"
    },
    {
      "name": "diff.patch",
      "type": "patch",
      "content": "base64-encoded-content"
    }
  ]
}
```

### POST /workspaces/{id}/safe-edit

Apply a safe text replacement to a file inside the workspace.

Request body:

```json
{
  "file_path": "/workspace/repo/app/main.go",
  "pattern": "old text",
  "replacement": "new text"
}
```

Success response:

```json
{
  "success": true,
  "message": "Edit successful"
}
```

### POST /workspaces/{id}/commit-and-pr

Commit the workspace's current working tree, push it to a server-selected branch, and open a merge request.

Request body:

```json
{
  "commit_message": "Add feature",
  "pr_title": "Add feature",
  "pr_description": "Describe the change",
  "project_path": "example/project",
  "gitlab_project_path": "example/project"
}
```

Fields:

- `commit_message` (required): commit summary for the workspace change
- `pr_title` (required): merge request title
- `pr_description` (optional): merge request description
- `project_path` / `gitlab_project_path` (optional): override project path when required by the configured forge

Success response:

```json
{
  "branch": "wsd-branch-name",
  "mr_url": "https://example.com/merge/123",
  "error": ""
}
```

If the backend cannot open a PR (for example, no changes to commit), it still responds with HTTP 200 and includes a non-empty `error` string.

## File-system access

### GET /workspaces/{id}/files

List files at a directory inside the workspace.

Query parameter:

- `path` (required): directory path to list

Response:

```json
{
  "entries": [
    {
      "name": "src",
      "path": "/workspace/repo/src",
      "is_dir": true,
      "size": 4096
    }
  ]
}
```

### GET /workspaces/{id}/file

Read a file.

Query parameter:

- `path` (required): file path to read

Response:

```json
{
  "content": "file contents here"
}
```

### PUT /workspaces/{id}/file

Write text content to a file.

Request body:

```json
{
  "path": "/workspace/repo/config.yaml",
  "content": "key: value\n"
}
```

Response:

- HTTP 204 No Content on success.

## Background jobs

The daemon supports long-running commands and provisioning jobs through the `jobs` API. The historical `/processes` routes remain available as aliases.

### POST /workspaces/{id}/jobs

Start a tracked background job.

Request body:

```json
{
  "command": "go test ./...",
  "allow_network": true,
  "timeout_seconds": 600
}
```

Success response:

```json
{
  "job_id": "job-abc",
  "id": "job-abc",
  "command": "go test ./...",
  "status": "queued"
}
```

### GET /workspaces/{id}/jobs

List tracked jobs for a workspace.

Response:

```json
{
  "jobs": [
    {
      "job_id": "job-abc",
      "id": "job-abc",
      "command": "go test ./...",
      "status": "running",
      "exit_code": 0
    }
  ],
  "processes": [
    {
      "job_id": "job-abc",
      "id": "job-abc",
      "command": "go test ./...",
      "status": "running",
      "exit_code": 0
    }
  ]
}
```

### GET /workspaces/{id}/jobs/{jid}

Read a job's state, output, and optional finished workspace detail.

Query parameters:

- `cursor` or `since`: return only output emitted after the previous cursor

Response:

```json
{
  "job_id": "job-abc",
  "id": "job-abc",
  "command": "go test ./...",
  "status": "succeeded",
  "exit_code": 0,
  "stdout": "ok\n",
  "stderr": "",
  "output": "{\"ts\":\"...\",\"stream\":\"stdout\",\"line\":\"ok\"}\n",
  "cursor": "done",
  "workspace": {
    "workspace_id": "ws-123",
    "os": "linux",
    "work_dir": "/workspace/repo",
    "profile": {
      "language": "go",
      "version": "1.23.3"
    },
    "cache_hit": false
  }
}
```

Status values are:

- `queued`
- `running`
- `succeeded`
- `failed`
- `cancelled`
- `timed_out`

### POST /workspaces/{id}/jobs/{jid}/input

Explicitly rejected. Interactive stdin is not supported for background jobs.

Response:

- HTTP 501 Not Implemented

### DELETE /workspaces/{id}/jobs/{jid}

Terminate a background job and mark it cancelled.

Response:

- HTTP 204 No Content on success.

## Admin

### GET /admin/leftovers

List containers or workspace records from a previous daemon run that were left behind and not cleaned up.

Response:

```json
{
  "leftovers": [
    {
      "workspace_id": "ws-999",
      "container_name": "mcwsd-ws-999",
      "session_id": "sess-abc",
      "repo_url": "https://github.com/example/project",
      "created_at": "2026-10-18T00:00:00Z"
    }
  ]
}
```

## Authentication and security notes

- Every request must use the daemon's shared bearer token.
- Authorization headers are checked exactly as `Bearer <token>`.
- Audit logs redact secret-bearing keys, including `Authorization`, `token`, `password`, and `api_key`.
- The API is intentionally simple and AI-client friendly; it is not designed to expose per-user identity or authorization scopes.

## Compatibility note

This is the stable surface used before the public release. The daemon may continue to accept historical `/processes` aliases, but the canonical route names are `/jobs` and the fields `job_id` and `id` are both retained for compatibility.
