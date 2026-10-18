package secrets

import (
	"context"
	"testing"
	"time"
)

func TestGrantThenGetValue(t *testing.T) {
	b := NewBroker()
	ctx := context.Background()

	err := b.Grant(ctx, "ws-1", "MY_SECRET", time.Minute)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}

	val, ok := b.GetValue("ws-1", "MY_SECRET")
	if !ok {
		t.Fatal("GetValue: expected hit after Grant, got miss")
	}
	if val == "" {
		t.Error("GetValue: expected non-empty value")
	}
}

func TestGrantValueFormat(t *testing.T) {
	b := NewBroker()
	ctx := context.Background()

	_ = b.Grant(ctx, "ws-2", "DB_PASS", time.Minute)
	val, _ := b.GetValue("ws-2", "DB_PASS")

	// The broker prefixes "SECRET_" + the ref as a placeholder.
	want := "SECRET_DB_PASS"
	if val != want {
		t.Errorf("GetValue: want %q, got %q", want, val)
	}
}

func TestRevoke_RemovesEntry(t *testing.T) {
	b := NewBroker()
	ctx := context.Background()

	_ = b.Grant(ctx, "ws-3", "API_KEY", time.Minute)
	if err := b.Revoke(ctx, "ws-3", "API_KEY"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	_, ok := b.GetValue("ws-3", "API_KEY")
	if ok {
		t.Error("GetValue: expected miss after Revoke, got hit")
	}
}

func TestRevoke_Nonexistent_NoError(t *testing.T) {
	b := NewBroker()
	ctx := context.Background()

	// Revoking a secret that was never granted must not return an error.
	if err := b.Revoke(ctx, "ws-x", "GHOST_KEY"); err != nil {
		t.Errorf("Revoke nonexistent: want nil error, got %v", err)
	}
}

func TestGetValue_UnknownWorkspace(t *testing.T) {
	b := NewBroker()
	val, ok := b.GetValue("no-such-ws", "KEY")
	if ok {
		t.Error("GetValue: expected miss for unknown workspace")
	}
	if val != "" {
		t.Errorf("GetValue: expected empty string, got %q", val)
	}
}

func TestGetValue_UnknownSecretRef(t *testing.T) {
	b := NewBroker()
	ctx := context.Background()

	_ = b.Grant(ctx, "ws-4", "KNOWN_KEY", time.Minute)

	val, ok := b.GetValue("ws-4", "UNKNOWN_KEY")
	if ok {
		t.Error("GetValue: expected miss for unknown ref")
	}
	if val != "" {
		t.Errorf("GetValue: expected empty string, got %q", val)
	}
}

func TestTTLExpiry(t *testing.T) {
	b := NewBroker()
	ctx := context.Background()

	// Grant with an extremely short TTL.
	_ = b.Grant(ctx, "ws-5", "SHORT_LIVED", 50*time.Millisecond)

	// Should be present immediately.
	_, ok := b.GetValue("ws-5", "SHORT_LIVED")
	if !ok {
		t.Fatal("GetValue: expected hit immediately after Grant")
	}

	// Wait for the TTL to expire.
	time.Sleep(150 * time.Millisecond)

	_, ok = b.GetValue("ws-5", "SHORT_LIVED")
	if ok {
		t.Error("GetValue: expected miss after TTL expired, got hit")
	}
}

func TestMultipleWorkspaces_Isolated(t *testing.T) {
	b := NewBroker()
	ctx := context.Background()

	_ = b.Grant(ctx, "ws-a", "KEY", time.Minute)
	_ = b.Grant(ctx, "ws-b", "KEY", time.Minute)

	valA, okA := b.GetValue("ws-a", "KEY")
	valB, okB := b.GetValue("ws-b", "KEY")

	if !okA || !okB {
		t.Fatal("expected hits for both workspaces")
	}
	// Both should have the value (they share the same ref so same placeholder).
	if valA != valB {
		t.Errorf("values differ between workspaces: ws-a=%q ws-b=%q", valA, valB)
	}

	// Revoking from ws-a should not affect ws-b.
	_ = b.Revoke(ctx, "ws-a", "KEY")
	if _, ok := b.GetValue("ws-b", "KEY"); !ok {
		t.Error("Revoke in ws-a incorrectly removed KEY from ws-b")
	}
}
