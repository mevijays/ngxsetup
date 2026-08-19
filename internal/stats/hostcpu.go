package stats

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// HostCPUSample is one aggregate snapshot of CPU time across every core,
// parsed from /proc/stat's leading "cpu" line — distinct from ProcSample,
// which is one process's own time.
type HostCPUSample struct {
	Idle  uint64 // idle + iowait
	Total uint64 // sum of every field on the line
}

// ReadHostCPUSample reads the current sample from /proc/stat.
func ReadHostCPUSample() (HostCPUSample, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return HostCPUSample{}, fmt.Errorf("reading /proc/stat: %w", err)
	}
	return parseHostCPUSample(string(data))
}

// parseHostCPUSample is kept separate from the file read so the arithmetic
// can be tested against fixed sample text, the same split reasoning as the
// rest of this package.
func parseHostCPUSample(procStat string) (HostCPUSample, error) {
	for _, line := range strings.Split(procStat, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var sample HostCPUSample
		for i, f := range fields[1:] {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return HostCPUSample{}, fmt.Errorf("parsing /proc/stat field %d (%q): %w", i+1, f, err)
			}
			sample.Total += v
			// Fields, in order: user, nice, system, idle, iowait, irq,
			// softirq, steal, guest, guest_nice. idle (index 3) and iowait
			// (index 4) both represent the CPU doing no work on behalf of
			// any process — the standard `top`-style definition of "idle."
			if i == 3 || i == 4 {
				sample.Idle += v
			}
		}
		return sample, nil
	}
	return HostCPUSample{}, fmt.Errorf("no aggregate cpu line found in /proc/stat")
}

// HostCPUPercent computes utilization between two samples as a 0-100
// percentage of total capacity — unlike per-process CPUPercent, this never
// exceeds 100 regardless of core count, because both prev and cur already
// sum ticks across every core.
func HostCPUPercent(prev, cur HostCPUSample) float64 {
	if cur.Total < prev.Total || cur.Idle < prev.Idle {
		return 0 // counters reset (e.g. a reboot between samples)
	}
	totalDelta := cur.Total - prev.Total
	if totalDelta == 0 {
		return 0
	}
	idleDelta := cur.Idle - prev.Idle
	if idleDelta > totalDelta {
		idleDelta = totalDelta
	}
	return (1 - float64(idleDelta)/float64(totalDelta)) * 100
}
