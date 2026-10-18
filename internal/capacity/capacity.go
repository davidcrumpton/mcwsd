// Package capacity provides pre-provisioning host capacity checks.
//
// Before the daemon provisions a new workspace (container, jail, VM, or
// plain directory), it can verify that host memory, swap, CPU, and
// filesystem usage are within configured thresholds and fail fast when they
// are not. The point is to stop a burst of workspace requests from driving
// the host into swap or filling a pool: refusing one request is far cheaper
// than an unresponsive host.
//
// A zero percentage disables that particular check, so the zero value of
// Config performs no checks at all and is safe to leave unset.
package capacity

import (
	"fmt"
	"strconv"
	"strings"
)

// FilesystemRule pairs a host path with the maximum percentage of that
// filesystem's space that may be in use before provisioning is refused.
// Multiple rules are allowed, so an operator can gate root, a jail pool, and
// a workspace pool independently.
type FilesystemRule struct {
	// Path is any path on the host; the rule applies to the filesystem
	// containing it (its mount), not to the path itself. It does not need
	// to exist for configuration to be valid, but it must exist when the
	// check runs.
	Path string

	// MaxPercent is the usage percentage at or above which provisioning is
	// refused. 0 disables the rule.
	MaxPercent float64
}

// Config describes the capacity thresholds that gate provisioning.
type Config struct {
	// MemoryPercent refuses provisioning when host memory usage (total
	// minus available) is at or above this percentage. 0 disables.
	MemoryPercent float64

	// SwapPercent refuses provisioning when host swap usage is at or above
	// this percentage. 0 disables. If the host has no swap configured the
	// check is skipped (there is nothing to fill).
	SwapPercent float64

	// CPUPercent refuses provisioning when host CPU usage, sampled over a
	// short window, is at or above this percentage. 0 disables.
	CPUPercent float64

	// Filesystems holds the per-filesystem rules.
	Filesystems []FilesystemRule
}

// Enabled reports whether any capacity check is configured.
func (c Config) Enabled() bool {
	return c.MemoryPercent > 0 || c.SwapPercent > 0 || c.CPUPercent > 0 || len(c.Filesystems) > 0
}

// Validate rejects nonsensical thresholds (negative or above 100 percent)
// and filesystem rules with no path.
func (c Config) Validate() error {
	for name, pct := range map[string]float64{
		"memory": c.MemoryPercent,
		"swap":   c.SwapPercent,
		"cpu":    c.CPUPercent,
	} {
		if pct < 0 || pct > 100 {
			return fmt.Errorf("capacity %s percent must be between 0 and 100, got %g", name, pct)
		}
	}
	for _, rule := range c.Filesystems {
		if strings.TrimSpace(rule.Path) == "" {
			return fmt.Errorf("capacity filesystem rule is missing a path")
		}
		if rule.MaxPercent < 0 || rule.MaxPercent > 100 {
			return fmt.Errorf("capacity filesystem %q percent must be between 0 and 100, got %g", rule.Path, rule.MaxPercent)
		}
	}
	return nil
}

// Stats is a snapshot of host resource usage. Sizes are in bytes.
type Stats struct {
	MemoryTotalBytes uint64
	MemoryUsedBytes  uint64
	SwapTotalBytes   uint64
	SwapUsedBytes    uint64

	// CPUPercent is the fraction of CPU time that was busy over the probe's
	// sampling window, 0-100.
	CPUPercent float64
}

// FSStats is a snapshot of a single filesystem's usage. Sizes are in bytes.
type FSStats struct {
	TotalBytes uint64
	UsedBytes  uint64
}

// Probe reads host resource usage. It exists as an interface so the check
// logic can be exercised deterministically in tests without depending on
// the real host.
type Probe interface {
	// Stats returns a snapshot of memory, swap, and CPU usage. It may
	// return ErrUnsupported on platforms where these cannot be read.
	Stats() (Stats, error)

	// Filesystem returns usage for the filesystem containing path.
	Filesystem(path string) (FSStats, error)
}

// ErrUnsupported is returned by a Probe for a resource it cannot read on the
// current platform.
var ErrUnsupported = fmt.Errorf("resource usage not supported on this platform")

// Checker evaluates a Config against a Probe.
type Checker struct {
	cfg   Config
	probe Probe
}

// NewChecker returns a Checker backed by the host probe for the current
// platform. A nil Checker (or one with no thresholds configured) never
// fails a check.
func NewChecker(cfg Config) *Checker {
	return &Checker{cfg: cfg, probe: defaultProbe()}
}

// NewCheckerWithProbe returns a Checker backed by the supplied Probe. It is
// intended for tests.
func NewCheckerWithProbe(cfg Config, probe Probe) *Checker {
	return &Checker{cfg: cfg, probe: probe}
}

// Check evaluates every configured threshold and returns an error describing
// all violations if any is met or exceeded. It returns nil when no
// thresholds are configured or all are within limits.
//
// A configured memory/swap/CPU threshold on a platform whose probe cannot
// read those resources is treated as an error rather than silently skipped:
// an operator who asked for enforcement should know it is not happening.
func (c *Checker) Check() error {
	if c == nil || !c.cfg.Enabled() {
		return nil
	}

	var violations []string

	if c.cfg.MemoryPercent > 0 || c.cfg.SwapPercent > 0 || c.cfg.CPUPercent > 0 {
		stats, err := c.probe.Stats()
		if err != nil {
			return fmt.Errorf("capacity check: %w", err)
		}
		if c.cfg.MemoryPercent > 0 {
			pct := percent(stats.MemoryUsedBytes, stats.MemoryTotalBytes)
			if pct >= c.cfg.MemoryPercent {
				violations = append(violations, fmt.Sprintf(
					"memory usage %.1f%% >= %.1f%% limit (%s used of %s)",
					pct, c.cfg.MemoryPercent, humanBytes(stats.MemoryUsedBytes), humanBytes(stats.MemoryTotalBytes)))
			}
		}
		if c.cfg.SwapPercent > 0 && stats.SwapTotalBytes > 0 {
			pct := percent(stats.SwapUsedBytes, stats.SwapTotalBytes)
			if pct >= c.cfg.SwapPercent {
				violations = append(violations, fmt.Sprintf(
					"swap usage %.1f%% >= %.1f%% limit (%s used of %s)",
					pct, c.cfg.SwapPercent, humanBytes(stats.SwapUsedBytes), humanBytes(stats.SwapTotalBytes)))
			}
		}
		if c.cfg.CPUPercent > 0 {
			if stats.CPUPercent >= c.cfg.CPUPercent {
				violations = append(violations, fmt.Sprintf(
					"cpu usage %.1f%% >= %.1f%% limit", stats.CPUPercent, c.cfg.CPUPercent))
			}
		}
	}

	for _, rule := range c.cfg.Filesystems {
		if rule.MaxPercent <= 0 {
			continue
		}
		fs, err := c.probe.Filesystem(rule.Path)
		if err != nil {
			return fmt.Errorf("capacity check: filesystem %s: %w", rule.Path, err)
		}
		pct := percent(fs.UsedBytes, fs.TotalBytes)
		if pct >= rule.MaxPercent {
			violations = append(violations, fmt.Sprintf(
				"filesystem %s usage %.1f%% >= %.1f%% limit (%s used of %s)",
				rule.Path, pct, rule.MaxPercent, humanBytes(fs.UsedBytes), humanBytes(fs.TotalBytes)))
		}
	}

	if len(violations) > 0 {
		return fmt.Errorf("provisioning refused: %s", strings.Join(violations, "; "))
	}
	return nil
}

// ParseFilesystemRules parses a comma-separated list of "path=percent" rules,
// e.g. "/=90,/var=85,/usr/local/jails=95". Whitespace around entries is
// ignored. An empty string yields no rules.
func ParseFilesystemRules(s string) ([]FilesystemRule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}

	var rules []FilesystemRule
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		path, pctStr, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("invalid capacity filesystem rule %q: expected path=percent", entry)
		}
		path = strings.TrimSpace(path)
		if path == "" {
			return nil, fmt.Errorf("invalid capacity filesystem rule %q: path is empty", entry)
		}
		pct, err := strconv.ParseFloat(strings.TrimSpace(pctStr), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid capacity filesystem rule %q: bad percent %q", entry, pctStr)
		}
		if pct < 0 || pct > 100 {
			return nil, fmt.Errorf("invalid capacity filesystem rule %q: percent must be between 0 and 100", entry)
		}
		rules = append(rules, FilesystemRule{Path: path, MaxPercent: pct})
	}
	return rules, nil
}

// percent returns used/total as a percentage, guarding against a zero total.
func percent(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

// humanBytes renders a byte count in the largest binary unit that keeps it
// readable, for use in error messages.
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
