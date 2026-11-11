// Package forge defines the abstraction over git hosting providers
// (GitLab today; GitHub, Gitea, Forgejo in later phases). It covers only
// the hosting-provider-specific step of opening a merge/pull request — the
// generic git plumbing (clone, fetch, push) lives in internal/gitpush and
// is identical regardless of which forge is in play.
package forge

import (
	"context"
	"io"
	"net/http"
	"time"
)

// HTTPClient is the shared client for forge REST calls. A bounded timeout
// keeps a hung forge from pinning a daemon goroutine (and the HTTP handler
// that triggered it) forever.
var HTTPClient = &http.Client{Timeout: 30 * time.Second}

// MaxResponseBytes caps how much of a forge API response is read into memory.
const MaxResponseBytes = 4 << 20

// ReadResponseBody reads at most MaxResponseBytes from r.
func ReadResponseBody(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, MaxResponseBytes))
}

// SleepContext waits for d or until ctx is cancelled, whichever comes first.
func SleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

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
