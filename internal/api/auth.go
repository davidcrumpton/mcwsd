package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/davidcrumpton/mcwsd/internal"
)

// WorkspaceAuthMode controls which credentials may access workspace-scoped
// routes.
type WorkspaceAuthMode string

const (
	// WorkspaceAuthScoped accepts a workspace token for its own workspace and
	// the admin token for any workspace (control-plane break-glass).
	WorkspaceAuthScoped WorkspaceAuthMode = "scoped"

	// WorkspaceAuthStrict accepts only the matching workspace token on
	// /workspaces/{id}/... routes. The admin token is limited to provisioning,
	// /admin/*, and token minting.
	WorkspaceAuthStrict WorkspaceAuthMode = "strict"
)

const workspaceTokenPrefix = "wst_"

// ParseWorkspaceAuthMode validates a --workspace-auth-mode value. An empty
// value is an error: the mode is mandatory so a daemon cannot silently run
// with the old shared-token-only behavior.
func ParseWorkspaceAuthMode(value string) (WorkspaceAuthMode, error) {
	mode := WorkspaceAuthMode(strings.TrimSpace(value))
	switch mode {
	case WorkspaceAuthScoped, WorkspaceAuthStrict:
		return mode, nil
	case "":
		return "", fmt.Errorf("--workspace-auth-mode is required: set it to %q or %q (or set MCWSD_WORKSPACE_AUTH_MODE)", WorkspaceAuthScoped, WorkspaceAuthStrict)
	default:
		return "", fmt.Errorf("invalid --workspace-auth-mode %q: must be %q or %q", value, WorkspaceAuthScoped, WorkspaceAuthStrict)
	}
}

// tokenAuthority mints and verifies workspace capability tokens. A token is an
// HMAC over the workspace ID, keyed by the daemon's admin token, so it is
// valid for exactly one workspace and requires no per-workspace state.
type tokenAuthority struct {
	admin string
	key   []byte
}

func newTokenAuthority(adminToken string) *tokenAuthority {
	mac := hmac.New(sha256.New, []byte(adminToken))
	io.WriteString(mac, "mcwsd/workspace-token/v1")
	return &tokenAuthority{admin: adminToken, key: mac.Sum(nil)}
}

// secureEqual compares two strings in constant time regardless of length.
func secureEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}

func (a *tokenAuthority) isAdmin(token string) bool {
	return secureEqual(token, a.admin)
}

func (a *tokenAuthority) workspaceToken(workspaceID string) string {
	mac := hmac.New(sha256.New, a.key)
	io.WriteString(mac, workspaceID)
	return workspaceTokenPrefix + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *tokenAuthority) authorizeWorkspace(mode WorkspaceAuthMode, workspaceID, token string) bool {
	if mode == WorkspaceAuthScoped && a.isAdmin(token) {
		return true
	}
	return secureEqual(token, a.workspaceToken(workspaceID))
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// withAdminAuth guards control-plane routes. It always requires the admin
// token, even in strict mode.
func (s *Server) withAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.tokens == nil || !s.tokens.isAdmin(bearerToken(r)) {
			s.rejectUnauthorized(w, r)
			return
		}
		next(w, r)
	}
}

// withWorkspaceAuth guards /workspaces/{id}/... routes. The token is checked
// against the same PathValue the handler will read, so authorization and the
// acted-on workspace can never diverge.
func (s *Server) withWorkspaceAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" || s.tokens == nil || !s.tokens.authorizeWorkspace(s.workspaceAuthMode, id, bearerToken(r)) {
			s.rejectUnauthorized(w, r)
			return
		}
		next(w, r)
	}
}

func (s *Server) rejectUnauthorized(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	internal.Log.Error("unauthorized_access",
		"remote_addr", r.RemoteAddr,
		"workspace_id", requestWorkspaceID(r),
		"authenticated_caller", requestCaller(r),
		"correlation_id", requestCorrelationID(r))
}
