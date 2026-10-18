//go:build freebsd

package capacity

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// cpuSampleInterval is how long the FreeBSD probe samples kern.cp_time for
// CPU usage. CPU usage is a rate, so two samples separated in time are
// required.
const cpuSampleInterval = 200 * time.Millisecond

type freebsdProbe struct{}

func defaultProbe() Probe { return freebsdProbe{} }

// Stats reads memory and swap via sysctl(8)/swapinfo(8) and samples CPU
// usage from kern.cp_time.
func (freebsdProbe) Stats() (Stats, error) {
	memTotal, err := sysctlUint("hw.physmem")
	if err != nil {
		return Stats{}, err
	}
	pageSize, err := sysctlUint("hw.pagesize")
	if err != nil {
		return Stats{}, err
	}

	free, err := sysctlUint("vm.stats.vm.v_free_count")
	if err != nil {
		return Stats{}, err
	}
	inactive, err := sysctlUint("vm.stats.vm.v_inactive_count")
	if err != nil {
		return Stats{}, err
	}
	cache, err := sysctlUint("vm.stats.vm.v_cache_count")
	if err != nil {
		// Not all versions expose v_cache_count; treat it as zero.
		cache = 0
	}

	available := (free + inactive + cache) * pageSize
	used := uint64(0)
	if memTotal > available {
		used = memTotal - available
	}

	swapTotal, swapUsed, err := swapUsage()
	if err != nil {
		return Stats{}, err
	}

	cpu, err := readCPUPercent()
	if err != nil {
		return Stats{}, err
	}

	return Stats{
		MemoryTotalBytes: memTotal,
		MemoryUsedBytes:  used,
		SwapTotalBytes:   swapTotal,
		SwapUsedBytes:    swapUsed,
		CPUPercent:       cpu,
	}, nil
}

// Filesystem reports usage for the filesystem containing path via statfs(2).
func (freebsdProbe) Filesystem(path string) (FSStats, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return FSStats{}, err
	}
	total := st.Blocks * st.Bsize
	bavail := uint64(0)
	if st.Bavail > 0 {
		bavail = uint64(st.Bavail)
	}
	used := total - bavail*st.Bsize
	return FSStats{TotalBytes: total, UsedBytes: used}, nil
}

// sysctlUint reads a numeric sysctl via sysctl(8). The base system guarantees
// sysctl(8) is present, and reading it this way avoids depending on a
// particular OID's in-kernel binary layout.
func sysctlUint(name string) (uint64, error) {
	out, err := exec.Command("sysctl", "-n", name).Output()
	if err != nil {
		return 0, fmt.Errorf("sysctl -n %s: %w", name, err)
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("sysctl -n %s: bad value %q: %w", name, strings.TrimSpace(string(out)), err)
	}
	return v, nil
}

// swapUsage sums the total and used columns of swapinfo -k. It returns
// (0, 0, nil) when no swap is configured, which makes the caller skip the
// swap check.
func swapUsage() (total, used uint64, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "swapinfo", "-k").Output()
	if err != nil {
		return 0, 0, fmt.Errorf("swapinfo -k: %w", err)
	}

	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// Skip the header ("Device 1K-blocks Used ...") and the Total row;
		// only real devices contribute, so the total is not double-counted.
		if fields[0] == "Device" || fields[0] == "Total" {
			continue
		}
		t, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		u, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		total += t * 1024
		used += u * 1024
	}
	return total, used, nil
}

// cpuTimes holds the aggregate CPU counters from kern.cp_time.
type cpuTimes struct {
	total uint64
	idle  uint64
}

// readCPUTimes reads kern.cp_time, whose fields are user, nice, sys, intr,
// idle. The last field is idle time.
func readCPUTimes() (cpuTimes, error) {
	out, err := exec.Command("sysctl", "-n", "kern.cp_time").Output()
	if err != nil {
		return cpuTimes{}, fmt.Errorf("sysctl -n kern.cp_time: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return cpuTimes{}, fmt.Errorf("sysctl -n kern.cp_time: empty output")
	}
	var ct cpuTimes
	for i, field := range fields {
		v, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			continue
		}
		ct.total += v
		if i == len(fields)-1 {
			ct.idle = v
		}
	}
	return ct, nil
}

// readCPUPercent samples kern.cp_time twice and returns the busy fraction
// over the interval.
func readCPUPercent() (float64, error) {
	before, err := readCPUTimes()
	if err != nil {
		return 0, err
	}
	time.Sleep(cpuSampleInterval)
	after, err := readCPUTimes()
	if err != nil {
		return 0, err
	}

	totalDelta := after.total - before.total
	if totalDelta == 0 {
		return 0, nil
	}
	idleDelta := after.idle - before.idle
	if idleDelta > totalDelta {
		idleDelta = totalDelta
	}
	return float64(totalDelta-idleDelta) / float64(totalDelta) * 100, nil
}
