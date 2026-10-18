package secrets

import (
	"context"
	"sync"
	"time"

	"github.com/davidcrumpton/mcwsd/internal"
)

// Broker manages short-lived secrets for exec calls.
type Broker struct {
	secrets map[string]map[string]*secretEntry // workspaceID -> secretRef -> entry
	mu      sync.RWMutex
}

type secretEntry struct {
	value     string
	expiresAt time.Time
	timer     *time.Timer
}

// NewBroker creates a new secret broker.
func NewBroker() *Broker {
	return &Broker{
		secrets: make(map[string]map[string]*secretEntry),
	}
}

// Grant stores a secret temporarily scoped to a workspace and exec call.
// The actual secret value would come from a secure store in production.
func (b *Broker) Grant(ctx context.Context, workspaceID string, secretRef string, ttl time.Duration) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.secrets[workspaceID] == nil {
		b.secrets[workspaceID] = make(map[string]*secretEntry)
	}

	// In production, fetch actual secret value from secure store
	// For now, use a placeholder - the real implementation would integrate
	// with GPG/YubiKey-backed tooling or similar
	value := "SECRET_" + secretRef

	entry := &secretEntry{
		value:     value,
		expiresAt: time.Now().Add(ttl),
	}

	// Auto-revoke after TTL
	entry.timer = time.AfterFunc(ttl, func() {
		b.Revoke(context.Background(), workspaceID, secretRef)
	})

	b.secrets[workspaceID][secretRef] = entry
	internal.Log.Debug("secret_granted", "workspace_id", workspaceID, "secret_ref", secretRef, "ttl", ttl)
	return nil
}

// Revoke removes a secret from the workspace.
func (b *Broker) Revoke(ctx context.Context, workspaceID string, secretRef string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if ws, ok := b.secrets[workspaceID]; ok {
		if entry, ok := ws[secretRef]; ok {
			if entry.timer != nil {
				entry.timer.Stop()
			}
			delete(ws, secretRef)
			internal.Log.Debug("secret_revoked", "workspace_id", workspaceID, "secret_ref", secretRef)
		}
	}
	return nil
}

// GetValue returns the secret value for injection into exec environment.
// This is called internally by the backend during exec.
func (b *Broker) GetValue(workspaceID, secretRef string) (string, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if ws, ok := b.secrets[workspaceID]; ok {
		if entry, ok := ws[secretRef]; ok {
			if time.Now().Before(entry.expiresAt) {
				return entry.value, true
			}
		}
	}
	return "", false
}
