// Package ipam provides a small IPv4 address pool shared by runtime
// provisioners (jails, zones, and any scripted runtime) that assign addresses
// from a base network. It lives here, in core, rather than inside any one
// provisioner package, because every network-addressable runtime needs it.
package ipam

import (
	"fmt"
	"net"
	"strings"
	"sync"
)

// IPAM allocates IPv4 addresses from a base network to workspace IDs. The base
// CIDR supplies both the prefix length and the first address handed out.
type IPAM struct {
	mu        sync.Mutex
	network   *net.IPNet
	baseIP    net.IP
	prefixLen int
	poolSize  int
	allocated map[string]net.IP
	next      int
}

// New creates a pool starting at baseCIDR (e.g. "192.168.1.200/24") handing
// out poolSize consecutive addresses.
func New(baseCIDR string, poolSize int) (*IPAM, error) {
	ip, network, err := net.ParseCIDR(baseCIDR)
	if err != nil {
		return nil, fmt.Errorf("invalid base CIDR %q: %w", baseCIDR, err)
	}
	if ip.To4() == nil {
		return nil, fmt.Errorf("base CIDR %q is not IPv4", baseCIDR)
	}
	if poolSize <= 0 {
		return nil, fmt.Errorf("pool size must be positive, got %d", poolSize)
	}
	prefixLen, _ := network.Mask.Size()
	return &IPAM{
		network:   network,
		baseIP:    ip.To4(),
		prefixLen: prefixLen,
		poolSize:  poolSize,
		allocated: make(map[string]net.IP),
	}, nil
}

// Allocate returns the next address in the pool for workspaceID as a CIDR
// string (address/prefix). Repeat calls for the same workspace return the same
// address.
func (i *IPAM) Allocate(workspaceID string) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if ip, ok := i.allocated[workspaceID]; ok {
		return i.render(ip), nil
	}
	// Reuse released offsets: scan for the lowest unused slot instead of
	// treating next as a permanent high-water mark. Without this, the pool
	// reports exhaustion after poolSize lifetime allocations even when every
	// address has since been released.
	used := make(map[int]bool, len(i.allocated))
	for _, ip := range i.allocated {
		used[i.offset(ip)] = true
	}
	for off := 0; off < i.poolSize; off++ {
		if used[off] {
			continue
		}
		ip := i.add(off)
		i.allocated[workspaceID] = ip
		if off >= i.next {
			i.next = off + 1
		}
		return i.render(ip), nil
	}
	return "", fmt.Errorf("IP pool exhausted: %d/%d addresses allocated", len(i.allocated), i.poolSize)
}

// Release frees the address assigned to workspaceID, if any.
func (i *IPAM) Release(workspaceID string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.allocated, workspaceID)
}

// Reserve records an already-assigned address for workspaceID (used when a
// recovered workspace re-registers after a restart) and advances the cursor
// past it so a later Allocate never hands out the same address. It is
// idempotent.
func (i *IPAM) Reserve(workspaceID, addr string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, ok := i.allocated[workspaceID]; ok {
		return nil
	}
	addr, _, _ = strings.Cut(addr, "/")
	ip := net.ParseIP(addr)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("invalid IPv4 address %q", addr)
	}
	ip = ip.To4()
	i.allocated[workspaceID] = ip
	if off := i.offset(ip); off >= i.next {
		i.next = off + 1
	}
	return nil
}

// Gateway returns the .1 address of the pool's network.
func (i *IPAM) Gateway() net.IP {
	i.mu.Lock()
	defer i.mu.Unlock()
	network := i.network.IP.To4()
	if network == nil {
		return nil
	}
	gw := make(net.IP, net.IPv4len)
	copy(gw, network)
	gw[3] = 1
	return gw
}

// Network returns the pool's network in CIDR notation.
func (i *IPAM) Network() string {
	return i.network.String()
}

func (i *IPAM) render(ip net.IP) string {
	return fmt.Sprintf("%s/%d", ip.String(), i.prefixLen)
}

func (i *IPAM) offset(ip net.IP) int {
	return int(ip4ToUint(ip) - ip4ToUint(i.baseIP))
}

func (i *IPAM) add(offset int) net.IP {
	return uintToIP4(ip4ToUint(i.baseIP) + uint32(offset))
}

func ip4ToUint(ip net.IP) uint32 {
	ip = ip.To4()
	if ip == nil {
		return 0
	}
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func uintToIP4(n uint32) net.IP {
	return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}
