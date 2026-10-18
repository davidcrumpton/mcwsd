//go:build linux

package capacity

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// cpuSampleInterval is how long the Linux probe samples /proc/stat for CPU
// usage. CPU usage is a rate, so two samples separated in time are required.
const cpuSampleInterval = 200 * time.Millisecond

type linuxProbe struct{}

func defaultProbe() Probe { return linuxProbe{} }

// Stats reads memory and swap from /proc/meminfo and samples CPU usage from
// /proc/stat.
func (linuxProbe) Stats() (Stats, error) {
	stats, err := readMeminfo()
	if err != nil {
		return Stats{}, err
	}

	cpu, err := readCPUPercent()
	if err != nil {
		return Stats{}, err
	}
	stats.CPUPercent = cpu
	return stats, nil
}

// Filesystem reports usage for the filesystem containing path via statfs(2).
func (linuxProbe) Filesystem(path string) (FSStats, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return FSStats{}, err
	}
	blockSize := uint64(st.Bsize)
	total := st.Blocks * blockSize
	// Use Bavail (blocks available to unprivileged users) rather than Bfree,
	// since the reserved root blocks are not available to workspaces.
	used := total - st.Bavail*blockSize
	return FSStats{TotalBytes: total, UsedBytes: used}, nil
}

// readMeminfo parses the handful of /proc/meminfo fields needed for the
// memory and swap checks. Values are reported in kB by the kernel.
func readMeminfo() (Stats, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return Stats{}, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	defer f.Close()

	vals := make(map[string]uint64)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		vals[key] = v * 1024 // kB -> bytes
	}
	if err := scanner.Err(); err != nil {
		return Stats{}, fmt.Errorf("read /proc/meminfo: %w", err)
	}

	total := vals["MemTotal"]
	available := vals["MemAvailable"]
	if available == 0 {
		// MemAvailable was added in Linux 3.14; fall back to MemFree on
		// older kernels (this overstates usage, which fails safe).
		available = vals["MemFree"]
	}
	used := uint64(0)
	if total > available {
		used = total - available
	}

	swapTotal := vals["SwapTotal"]
	swapFree := vals["SwapFree"]
	swapUsed := uint64(0)
	if swapTotal > swapFree {
		swapUsed = swapTotal - swapFree
	}

	return Stats{
		MemoryTotalBytes: total,
		MemoryUsedBytes:  used,
		SwapTotalBytes:   swapTotal,
		SwapUsedBytes:    swapUsed,
	}, nil
}

// cpuTimes holds the aggregate CPU counters from /proc/stat.
type cpuTimes struct {
	total uint64
	idle  uint64
}

// readCPUTimes parses the aggregate "cpu" line of /proc/stat. Idle time
// includes iowait so that waiting on disk is not counted as CPU busy.
func readCPUTimes() (cpuTimes, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuTimes{}, fmt.Errorf("read /proc/stat: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		var ct cpuTimes
		for i, field := range fields {
			v, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				continue
			}
			ct.total += v
			// Field order: user nice system idle iowait irq softirq steal ...
			if i == 3 || i == 4 { // idle + iowait
				ct.idle += v
			}
		}
		return ct, nil
	}
	if err := scanner.Err(); err != nil {
		return cpuTimes{}, fmt.Errorf("read /proc/stat: %w", err)
	}
	return cpuTimes{}, fmt.Errorf("read /proc/stat: aggregate cpu line not found")
}

// readCPUPercent samples /proc/stat twice and returns the busy fraction over
// the interval.
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
