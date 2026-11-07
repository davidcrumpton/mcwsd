package freebsd

import (
	"fmt"
	"net"
	"testing"
)

func TestNewIPAM(t *testing.T) {
	tests := []struct {
		name      string
		baseCIDR  string
		poolSize  int
		wantError bool
	}{
		{"valid /24", "192.168.1.200/24", 50, false},
		{"valid /16", "10.0.0.1/16", 100, false},
		{"valid /24 with .0 base", "192.168.1.0/24", 10, false},
		{"invalid CIDR", "192.168.1.200", 10, true},
		{"invalid CIDR format", "192.168.1.200/99", 10, true},
		{"zero pool size", "192.168.1.200/24", 0, true},
		{"negative pool size", "192.168.1.200/24", -1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ipam, err := NewIPAM(tt.baseCIDR, tt.poolSize)
			if tt.wantError && err == nil {
				t.Errorf("NewIPAM() error = nil, want error")
			}
			if !tt.wantError && err != nil {
				t.Errorf("NewIPAM() error = %v, want nil", err)
			}
			if !tt.wantError && ipam == nil {
				t.Errorf("NewIPAM() returned nil, want non-nil")
			}
		})
	}
}

func TestIPAMAllocate(t *testing.T) {
	ipam, err := NewIPAM("192.168.1.200/24", 5)
	if err != nil {
		t.Fatalf("NewIPAM() error = %v", err)
	}

	// First allocation should get base IP
	ip1, err := ipam.Allocate("workspace-1")
	if err != nil {
		t.Fatalf("Allocate(workspace-1) error = %v", err)
	}
	if ip1 != "192.168.1.200/24" {
		t.Errorf("Allocate(workspace-1) = %q, want %q", ip1, "192.168.1.200/24")
	}

	// Second allocation should get next IP
	ip2, err := ipam.Allocate("workspace-2")
	if err != nil {
		t.Fatalf("Allocate(workspace-2) error = %v", err)
	}
	if ip2 != "192.168.1.201/24" {
		t.Errorf("Allocate(workspace-2) = %q, want %q", ip2, "192.168.1.201/24")
	}

	// Third allocation
	ip3, err := ipam.Allocate("workspace-3")
	if err != nil {
		t.Fatalf("Allocate(workspace-3) error = %v", err)
	}
	if ip3 != "192.168.1.202/24" {
		t.Errorf("Allocate(workspace-3) = %q, want %q", ip3, "192.168.1.202/24")
	}

	// Allocate same workspace again should return same IP
	ip1_again, err := ipam.Allocate("workspace-1")
	if err != nil {
		t.Fatalf("Allocate(workspace-1) second time error = %v", err)
	}
	if ip1_again != ip1 {
		t.Errorf("Allocate(workspace-1) second time = %q, want %q", ip1_again, ip1)
	}
}

func TestIPAMExhaustion(t *testing.T) {
	ipam, err := NewIPAM("192.168.1.200/24", 3)
	if err != nil {
		t.Fatalf("NewIPAM() error = %v", err)
	}

	// Allocate all IPs
	for i := 0; i < 3; i++ {
		wsID := fmt.Sprintf("workspace-%d", i)
		_, err := ipam.Allocate(wsID)
		if err != nil {
			t.Fatalf("Allocate(workspace-%d) error = %v", i, err)
		}
	}

	// Next allocation should fail
	_, err = ipam.Allocate("workspace-4")
	if err == nil {
		t.Error("Allocate(workspace-4) succeeded, want error (pool exhausted)")
	}
}

func TestIPAMRelease(t *testing.T) {
	ipam, err := NewIPAM("192.168.1.200/24", 5)
	if err != nil {
		t.Fatalf("NewIPAM() error = %v", err)
	}

	// Allocate
	_, err = ipam.Allocate("workspace-1")
	if err != nil {
		t.Fatalf("Allocate(workspace-1) error = %v", err)
	}

	// Check it's allocated
	if !ipam.IsAllocated("workspace-1") {
		t.Error("IsAllocated(workspace-1) = false, want true")
	}

	// Release
	ipam.Release("workspace-1")

	// Check it's no longer allocated
	if ipam.IsAllocated("workspace-1") {
		t.Error("IsAllocated(workspace-1) after release = true, want false")
	}

	// Note: We don't reuse IPs in this simple implementation
	// So the next allocation will still get the next available IP
	ip2, err := ipam.Allocate("workspace-2")
	if err != nil {
		t.Fatalf("Allocate(workspace-2) error = %v", err)
	}
	// Should get the next IP, not the released one
	if ip2 != "192.168.1.201/24" {
		t.Errorf("Allocate(workspace-2) = %q, want %q (next available)", ip2, "192.168.1.201/24")
	}
}

func TestIPAMReserve(t *testing.T) {
	ipam, err := NewIPAM("192.168.1.200/24", 5)
	if err != nil {
		t.Fatalf("NewIPAM() error = %v", err)
	}

	// Reserving an address in the middle of the pool skips it for later
	// allocations.
	if err := ipam.Reserve("recovered-1", "192.168.1.202/24"); err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if got := ipam.GetAssignedIP("recovered-1"); got != "192.168.1.202/24" {
		t.Errorf("GetAssignedIP(recovered-1) = %q, want %q", got, "192.168.1.202/24")
	}

	ip, err := ipam.Allocate("new-1")
	if err != nil {
		t.Fatalf("Allocate(new-1) error = %v", err)
	}
	if ip != "192.168.1.203/24" {
		t.Errorf("Allocate(new-1) = %q, want %q (must not collide with reserved IP)", ip, "192.168.1.203/24")
	}

	// Re-reserving the same workspace is a no-op and does not move the cursor.
	if err := ipam.Reserve("recovered-1", "192.168.1.202/24"); err != nil {
		t.Fatalf("Reserve() second time error = %v", err)
	}
	if got := ipam.GetAssignedIP("recovered-1"); got != "192.168.1.202/24" {
		t.Errorf("GetAssignedIP after re-reserve = %q, want %q", got, "192.168.1.202/24")
	}

	// Invalid addresses are rejected.
	if err := ipam.Reserve("bad", "not-an-ip"); err == nil {
		t.Error("Reserve() accepted an invalid address")
	}
}

func TestIPAMGetAssignedIP(t *testing.T) {
	ipam, err := NewIPAM("192.168.1.200/24", 5)
	if err != nil {
		t.Fatalf("NewIPAM() error = %v", err)
	}

	// Allocate
	ipam.Allocate("workspace-1")

	// Get assigned IP
	assigned := ipam.GetAssignedIP("workspace-1")
	if assigned != "192.168.1.200/24" {
		t.Errorf("GetAssignedIP(workspace-1) = %q, want %q", assigned, "192.168.1.200/24")
	}

	// Get unassigned workspace
	unassigned := ipam.GetAssignedIP("workspace-2")
	if unassigned != "" {
		t.Errorf("GetAssignedIP(workspace-2) = %q, want empty string", unassigned)
	}
}

func TestIPAMCountAllocated(t *testing.T) {
	ipam, err := NewIPAM("192.168.1.200/24", 5)
	if err != nil {
		t.Fatalf("NewIPAM() error = %v", err)
	}

	if ipam.CountAllocated() != 0 {
		t.Errorf("CountAllocated() = %d, want 0", ipam.CountAllocated())
	}

	ipam.Allocate("workspace-1")
	if ipam.CountAllocated() != 1 {
		t.Errorf("CountAllocated() after 1 allocation = %d, want 1", ipam.CountAllocated())
	}

	ipam.Allocate("workspace-2")
	if ipam.CountAllocated() != 2 {
		t.Errorf("CountAllocated() after 2 allocations = %d, want 2", ipam.CountAllocated())
	}

	ipam.Release("workspace-1")
	if ipam.CountAllocated() != 1 {
		t.Errorf("CountAllocated() after 1 release = %d, want 1", ipam.CountAllocated())
	}
}

func TestIPAMGatewayIP(t *testing.T) {
	tests := []struct {
		name     string
		baseCIDR string
		want     string
	}{
		{"192.168.1.0/24", "192.168.1.0/24", "192.168.1.1"},
		{"10.0.0.0/16", "10.0.0.0/16", "10.0.0.1"},
		{"192.168.1.200/24", "192.168.1.200/24", "192.168.1.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ipam, err := NewIPAM(tt.baseCIDR, 10)
			if err != nil {
				t.Fatalf("NewIPAM() error = %v", err)
			}

			gateway := ipam.GatewayIP()
			if gateway.String() != tt.want {
				t.Errorf("GatewayIP() = %v, want %v", gateway, tt.want)
			}
		})
	}
}

func TestIPAMNetworkAndBase(t *testing.T) {
	ipam, err := NewIPAM("192.168.1.200/24", 10)
	if err != nil {
		t.Fatalf("NewIPAM() error = %v", err)
	}

	if ipam.Network() != "192.168.1.0/24" {
		t.Errorf("Network() = %q, want %q", ipam.Network(), "192.168.1.0/24")
	}

	if ipam.BaseIP().String() != "192.168.1.200" {
		t.Errorf("BaseIP() = %q, want %q", ipam.BaseIP(), "192.168.1.200")
	}

	if ipam.PoolSize() != 10 {
		t.Errorf("PoolSize() = %d, want %d", ipam.PoolSize(), 10)
	}
}

// Test IP calculation across network boundaries
func TestIPAMCalculateIPCrossBoundary(t *testing.T) {
	ipam, err := NewIPAM("192.168.1.253/24", 5)
	if err != nil {
		t.Fatalf("NewIPAM() error = %v", err)
	}

	// 253 + 0 = 253
	ip1, _ := ipam.Allocate("ws-1")
	if ip1 != "192.168.1.253/24" {
		t.Errorf("Allocate(ws-1) = %q, want %q", ip1, "192.168.1.253/24")
	}

	// 253 + 1 = 254
	ip2, _ := ipam.Allocate("ws-2")
	if ip2 != "192.168.1.254/24" {
		t.Errorf("Allocate(ws-2) = %q, want %q", ip2, "192.168.1.254/24")
	}

	// 253 + 2 = 255
	ip3, _ := ipam.Allocate("ws-3")
	if ip3 != "192.168.1.255/24" {
		t.Errorf("Allocate(ws-3) = %q, want %q", ip3, "192.168.1.255/24")
	}
}

func TestIPToIntAndBack(t *testing.T) {
	tests := []struct {
		ip      string
		wantInt uint32
	}{
		{"192.168.1.0", 3232235776},
		{"192.168.1.1", 3232235777},
		{"10.0.0.1", 167772161},
		{"0.0.0.0", 0},
		{"255.255.255.255", 4294967295},
	}

	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("ParseIP(%q) returned nil", tt.ip)
			}

			n := ipToInt(ip)
			if n != tt.wantInt {
				t.Errorf("ipToInt(%q) = %d, want %d", tt.ip, n, tt.wantInt)
			}

			// Convert back
			result := intToIP(n)
			if result.String() != tt.ip {
				t.Errorf("intToIP(%d) = %q, want %q", n, result.String(), tt.ip)
			}
		})
	}
}
