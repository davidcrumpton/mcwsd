//go:build !linux && !freebsd && !darwin

package capacity

type unsupportedProbe struct{}

func defaultProbe() Probe { return unsupportedProbe{} }

// Stats is not implemented on this platform.
func (unsupportedProbe) Stats() (Stats, error) {
	return Stats{}, ErrUnsupported
}

// Filesystem is not implemented on this platform.
func (unsupportedProbe) Filesystem(string) (FSStats, error) {
	return FSStats{}, ErrUnsupported
}
