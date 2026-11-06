package zones

import (
	"fmt"
	"net"
	"sync"
)

// ipam allocates IPv4 addresses from a configured pool for workspaces whose
// address is not pinned. It mirrors the FreeBSD jail pool semantics: the base
// CIDR supplies both the prefix length and the first address handed out.
type ipam struct {
	mu        sync.Mutex
	network   *net.IPNet
	baseIP    net.IP
	prefixLen int
	poolSize  int
	allocated map[string]net.IP
	next      int
}

func newIPAM(baseCIDR string, poolSize int) (*ipam, error) {
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
	return &ipam{
		network:   network,
		baseIP:    ip.To4(),
		prefixLen: prefixLen,
		poolSize:  poolSize,
		allocated: make(map[string]net.IP),
	}, nil
}

func (i *ipam) allocate(workspaceID string) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if ip, ok := i.allocated[workspaceID]; ok {
		return fmt.Sprintf("%s/%d", ip.String(), i.prefixLen), nil
	}
	if i.next >= i.poolSize {
		return "", fmt.Errorf("IP pool exhausted: %d/%d addresses allocated", i.next, i.poolSize)
	}
	ip := addIPv4(i.baseIP, i.next)
	i.allocated[workspaceID] = ip
	i.next++
	return fmt.Sprintf("%s/%d", ip.String(), i.prefixLen), nil
}

func (i *ipam) release(workspaceID string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.allocated, workspaceID)
}

// gateway returns the .1 address of the pool's network.
func (i *ipam) gateway() net.IP {
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

func addIPv4(ip net.IP, offset int) net.IP {
	n := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
	n += uint32(offset)
	return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}
