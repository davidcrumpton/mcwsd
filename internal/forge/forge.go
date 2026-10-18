// Package forge defines the abstraction over git hosting providers
// (GitLab today; GitHub, Gitea, Forgejo in later phases). It covers only
// the hosting-provider-specific step of opening a merge/pull request — the
// generic git plumbing (clone, fetch, push) lives in internal/gitpush and
// is identical regardless of which forge is in play.
package forge

import "context"

// OpenMRRequest describes a merge/pull request to open after a branch has
// already been pushed to the remote.
type OpenMRRequest struct {
	// RepoURL is the repo's git remote URL, used as a fallback source for
	// deriving ProjectPath when it isn't set explicitly.
	RepoURL string

	// ProjectPath identifies the project/repo on the forge (e.g. GitLab's
	// URL-encoded "namespace/project" path). If empty, implementations may
	// fall back to a forge-level default or attempt to derive it from
	// RepoURL.
	ProjectPath string

	Branch      string
	BaseBranch  string
	Title       string
	Description string
}

// OpenMRResult is returned on success.
type OpenMRResult struct {
	URL string
}

// Forge opens merge/pull requests against a git hosting provider's API.
//
// Extension point: implement this interface for a new provider (GitHub,
// Gitea, Forgejo, ...) and it can be swapped in wherever a Forge is used.
type Forge interface {
	// Name identifies this forge, e.g. "gitlab".
	Name() string

	OpenMergeRequest(ctx context.Context, req OpenMRRequest) (*OpenMRResult, error)
}
