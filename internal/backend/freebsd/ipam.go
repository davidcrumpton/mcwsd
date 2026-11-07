package freebsd

import (
	"fmt"
	"net"
	"strings"
	"sync"
)

// IPAM (IP Address Manager) manages automatic allocation of IP addresses
// from a configured network pool.
type IPAM struct {
	mu            sync.Mutex
	network       *net.IPNet
	baseIP        net.IP
	prefixLen     int // prefix length from the CIDR (e.g., 24)
	poolSize      int
	allocated     map[string]net.IP // workspace ID -> assigned IP
	nextAvailable int               // index of next available IP in pool
}

// NewIPAM creates a new IP Address Manager with the given base network and pool size.
// baseCIDR is in the form "192.168.1.200/24" - this defines both the network and the starting IP.
// poolSize is the number of IPs to allocate from the base IP onwards.
func NewIPAM(baseCIDR string, poolSize int) (*IPAM, error) {
	// Parse the base CIDR
	ip, ipnet, err := net.ParseCIDR(baseCIDR)
	if err != nil {
		return nil, fmt.Errorf("invalid base CIDR %q: %w", baseCIDR, err)
	}

	if poolSize <= 0 {
		return nil, fmt.Errorf("pool size must be positive, got %d", poolSize)
	}

	// Get prefix length from mask
	prefixLen, _ := ipnet.Mask.Size()

	return &IPAM{
		network:       ipnet,
		baseIP:        ip,
		prefixLen:     prefixLen,
		poolSize:      poolSize,
		allocated:     make(map[string]net.IP),
		nextAvailable: 0,
	}, nil
}

// Allocate assigns the next available IP address from the pool to the given workspace ID.
// Returns the allocated IP as a string in CIDR notation (e.g., "192.168.1.200/24"),
// or an error if no IPs are available.
func (ipam *IPAM) Allocate(workspaceID string) (string, error) {
	ipam.mu.Lock()
	defer ipam.mu.Unlock()

	// Check if workspace already has an IP
	if ip, exists := ipam.allocated[workspaceID]; exists {
		return fmt.Sprintf("%s/%d", ip.String(), ipam.prefixLen), nil
	}

	// Check if pool is exhausted
	if ipam.nextAvailable >= ipam.poolSize {
		return "", fmt.Errorf("IP pool exhausted: %d/%d IPs allocated", ipam.nextAvailable, ipam.poolSize)
	}

	// Calculate the next IP
	nextIP := ipam.calculateIP(ipam.nextAvailable)
	ipam.allocated[workspaceID] = nextIP
	ipam.nextAvailable++

	return fmt.Sprintf("%s/%d", nextIP.String(), ipam.prefixLen), nil
}

// Release frees the IP address assigned to the given workspace ID.
// The IP is returned to the pool and can be allocated to future workspaces.
func (ipam *IPAM) Release(workspaceID string) {
	ipam.mu.Lock()
	defer ipam.mu.Unlock()

	if _, exists := ipam.allocated[workspaceID]; exists {
		delete(ipam.allocated, workspaceID)
		// Note: We don't decrement nextAvailable because we want to avoid
		// IP address reuse conflicts. The IP will be reused only when the pool
		// is exhausted and we wrap around (not implemented here for simplicity).
		// For a more sophisticated implementation, we could track freed IPs in a list.
	}
}

// Reserve records an already-assigned IP for a workspace, for example when a
// recovered workspace is re-registered after a daemon restart. It advances the
// allocation cursor past the reserved IP so a subsequent Allocate never hands
// the same address out again. Reservations are idempotent: reserving a
// workspace that already has an IP is a no-op.
func (ipam *IPAM) Reserve(workspaceID, ipStr string) error {
	ipam.mu.Lock()
	defer ipam.mu.Unlock()

	if _, exists := ipam.allocated[workspaceID]; exists {
		return nil
	}

	ipStr, _, _ = strings.Cut(ipStr, "/")
	ip := net.ParseIP(ipStr)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("invalid IPv4 address %q", ipStr)
	}
	ip = ip.To4()

	ipam.allocated[workspaceID] = ip
	offset := int(ipToInt(ip) - ipToInt(ipam.baseIP))
	if offset >= ipam.nextAvailable {
		ipam.nextAvailable = offset + 1
	}
	return nil
}

// IsAllocated checks if the given workspace ID has an IP allocated.
func (ipam *IPAM) IsAllocated(workspaceID string) bool {
	ipam.mu.Lock()
	defer ipam.mu.Unlock()

	_, exists := ipam.allocated[workspaceID]
	return exists
}

// GetAssignedIP returns the IP assigned to a workspace, or empty string if not allocated.
func (ipam *IPAM) GetAssignedIP(workspaceID string) string {
	ipam.mu.Lock()
	defer ipam.mu.Unlock()

	ip, exists := ipam.allocated[workspaceID]
	if !exists {
		return ""
	}
	return fmt.Sprintf("%s/%d", ip.String(), ipam.prefixLen)
}

// calculateIP computes the IP at the given offset from the base IP.
// This handles incrementing the IP address correctly within the network.
func (ipam *IPAM) calculateIP(offset int) net.IP {
	// Convert base IP to 32-bit integer
	baseInt := ipToInt(ipam.baseIP)
	newInt := baseInt + uint32(offset)
	return intToIP(newInt)
}

// ipToInt converts an IPv4 address to a 32-bit integer.
func ipToInt(ip net.IP) uint32 {
	// Ensure it's IPv4
	ip = ip.To4()
	if ip == nil {
		return 0
	}
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

// intToIP converts a 32-bit integer to an IPv4 address.
func intToIP(n uint32) net.IP {
	return net.IPv4(
		byte(n>>24),
		byte(n>>16),
		byte(n>>8),
		byte(n),
	)
}

// Network returns the network in CIDR notation.
func (ipam *IPAM) Network() string {
	return ipam.network.String()
}

// BaseIP returns the base IP address.
func (ipam *IPAM) BaseIP() net.IP {
	return ipam.baseIP
}

// PoolSize returns the size of the IP pool.
func (ipam *IPAM) PoolSize() int {
	return ipam.poolSize
}

// CountAllocated returns the number of currently allocated IPs.
func (ipam *IPAM) CountAllocated() int {
	ipam.mu.Lock()
	defer ipam.mu.Unlock()
	return len(ipam.allocated)
}

// GatewayIP calculates a reasonable gateway IP for the network.
// For a /24 network like 192.168.1.0/24, this returns 192.168.1.1
// For a network like 192.168.1.200/24, this returns 192.168.1.1 (the .1 address)
func (ipam *IPAM) GatewayIP() net.IP {
	// For simplicity, we use .1 as the gateway
	// A more sophisticated implementation could take the gateway as a parameter

	// Zero out the host portion of base IP
	ip := make(net.IP, len(ipam.baseIP))
	copy(ip, ipam.baseIP)

	// Apply network mask
	mask := net.IP(ipam.network.Mask)
	for i := 0; i < len(ip) && i < len(mask); i++ {
		ip[i] = ip[i] & mask[i]
	}

	// Set last octet to 1
	if len(ip) == 4 {
		ip[3] = 1
	} else if len(ip) == net.IPv6len {
		// For IPv6, set the last byte to 1
		ip[net.IPv6len-1] = 1
	}
	return ip
}
