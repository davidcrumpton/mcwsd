package docker

import (
	"errors"
	"testing"
)

// TestIsBenignRemoveError verifies that a container which is already stopped
// or already removed is treated as a successful removal. Leftover cleanup
// relies on this: a state-file entry must be cleared once the underlying
// container is gone, otherwise it is reported as a leftover on every startup
// and cleanup can never succeed.
func TestIsBenignRemoveError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, true},
		{"no such container", errors.New("Error response from daemon: No such container: wsd-ws-abc"), true},
		{"lowercase no such container", errors.New("no such container: wsd-ws-abc"), true},
		{"does not exist", errors.New("Error response from daemon: container wsd-ws-abc does not exist"), true},
		{"not running", errors.New("Error response from daemon: Container wsd-ws-abc is not running"), true},
		{"permission denied", errors.New("Error response from daemon: cannot remove container: permission denied"), false},
		{"other error", errors.New("Error response from daemon: something else went wrong"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBenignRemoveError(tt.err); got != tt.want {
				t.Errorf("isBenignRemoveError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestIsAlreadyAttachedError verifies SetNetwork(true) stays idempotent when
// Docker reports the endpoint already exists, while real errors still surface.
func TestIsAlreadyAttachedError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"already exists", errors.New("Error response from daemon: endpoint with name wsd-ws-abc already exists in network bridge"), true},
		{"permission denied", errors.New("Error response from daemon: permission denied"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAlreadyAttachedError(tt.err); got != tt.want {
				t.Errorf("isAlreadyAttachedError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestIsAlreadyDetachedError verifies SetNetwork(false) stays idempotent when
// the container is not attached, while real errors still surface.
func TestIsAlreadyDetachedError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"not connected", errors.New("Error response from daemon: container wsd-ws-abc is not connected to network bridge"), true},
		{"missing endpoint", errors.New("Error response from daemon: network bridge does not have an endpoint named wsd-ws-abc"), true},
		{"permission denied", errors.New("Error response from daemon: permission denied"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAlreadyDetachedError(tt.err); got != tt.want {
				t.Errorf("isAlreadyDetachedError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
