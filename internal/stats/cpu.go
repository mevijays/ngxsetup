package stats

import "time"

// ProcSample is one snapshot of a process's CPU accounting and memory, read
// from /proc/[pid]/stat and /proc/[pid]/status. Kept independent of how it
// was read so the arithmetic below can be tested with fabricated data.
type ProcSample struct {
	PID    int
	UTicks uint64 // utime: /proc/[pid]/stat field 14
	STicks uint64 // stime: field 15
	RSSKB  int    // VmRSS from /proc/[pid]/status, in kilobytes
}

// totalTicks is utime+stime, the CPU time this process has consumed since it
// started, in kernel clock ticks.
func (p ProcSample) totalTicks() uint64 { return p.UTicks + p.STicks }

// CPUPercent computes the aggregate CPU utilization of a process group —
// every PHP-FPM worker belonging to one site's pool — between two samples.
//
// ticksPerSec must come from the kernel's actual clock tick rate
// (sysconf(_SC_CLK_TCK), read via getconf CLK_TCK — see proc_linux.go), not
// assumed. It is almost always 100 on Linux, but "almost always" is how a
// tuning tool ships a CPU percentage that is off by a constant, silently
// wrong factor on the one platform where it differs.
//
// Processes present in cur but not prev (a worker that just forked) are
// counted only from their own start; a process present in prev but gone from
// cur (a worker that exited, e.g. after pm.max_requests) contributes its
// prior consumption to neither sample, which is correct — that CPU time was
// already attributed on the sample where it was observed.
func CPUPercent(prev, cur []ProcSample, elapsed time.Duration, ticksPerSec int) float64 {
	if elapsed <= 0 || ticksPerSec <= 0 {
		return 0
	}
	prevByPID := make(map[int]uint64, len(prev))
	for _, p := range prev {
		prevByPID[p.PID] = p.totalTicks()
	}

	var deltaTicks uint64
	for _, c := range cur {
		before, existed := prevByPID[c.PID]
		total := c.totalTicks()
		if !existed || total < before {
			// A new PID, or one that wrapped/restarted with a lower counter
			// than we last saw — either way there is no valid delta to take,
			// so this sample contributes nothing rather than a nonsensical
			// negative or inflated one.
			continue
		}
		deltaTicks += total - before
	}

	seconds := elapsed.Seconds()
	cpuSeconds := float64(deltaTicks) / float64(ticksPerSec)
	return (cpuSeconds / seconds) * 100
}

// TotalRSSMB sums resident memory across a process group, in megabytes.
func TotalRSSMB(samples []ProcSample) int {
	var kb int
	for _, s := range samples {
		kb += s.RSSKB
	}
	return kb / 1024
}
