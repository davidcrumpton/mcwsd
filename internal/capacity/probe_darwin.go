//go:build darwin

package capacity

import (
	"syscall"
)

type darwinProbe struct{}

func defaultProbe() Probe { return darwinProbe{} }

// Stats is not implemented on Darwin: it is a development host for mcwsd, not
// a supported provisioning target, and there is no portable way to sample
// CPU usage here without pulling in Mach APIs. A configured memory, swap, or
// CPU threshold therefore fails the check loudly rather than being silently
// ignored.
func (darwinProbe) Stats() (Stats, error) {
	return Stats{}, ErrUnsupported
}

// Filesystem reports usage for the filesystem containing path via statfs(2).
func (darwinProbe) Filesystem(path string) (FSStats, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return FSStats{}, err
	}
	blockSize := uint64(st.Bsize)
	total := st.Blocks * blockSize
	used := total - st.Bavail*blockSize
	return FSStats{TotalBytes: total, UsedBytes: used}, nil
}
