//go:build linux

package stats

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// poolTitle is the exact process title PHP-FPM gives a pool's worker
// processes — confirmed live (`ps -eo user,args | grep pool`) against a real
// php-fpm8.3, not assumed from documentation. The master process instead
// reads "php-fpm: master process (...)", which this prefix does not match.
func poolTitle(slug string) string { return "php-fpm: pool " + slug }

// PoolPIDs finds the live worker PIDs for one site's PHP-FPM pool by reading
// every process's cmdline out of /proc. This needs no privilege beyond
// ordinary read access to /proc — the same access `ps` itself uses — and no
// FastCGI status page has to be exposed through nginx for it to work.
func PoolPIDs(slug string) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("reading /proc: %w", err)
	}
	want := poolTitle(slug)

	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a PID directory (self, net, sys, ...)
		}
		// PHP-FPM rewrites argv so /proc/[pid]/cmdline *is* the pool title,
		// NUL-separated the way every /proc/[pid]/cmdline is; comparing
		// against the first NUL-delimited field is what "ps" itself does.
		data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue // process exited between the ReadDir and this read
		}
		if i := bytes.IndexByte(data, 0); i >= 0 {
			data = data[:i]
		}
		if string(data) == want {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// ReadProcSample reads one process's CPU accounting and resident memory.
func ReadProcSample(pid int) (ProcSample, error) {
	s := ProcSample{PID: pid}

	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return s, err
	}
	// Field 2 (comm) is parenthesized and may itself contain spaces or
	// parens ("php-fpm: pool foo" would not, but nothing here should assume
	// that of every process on the machine); splitting after the *last* ')'
	// is the documented-safe way to parse this line, matching what procps
	// itself does.
	line := string(stat)
	close := strings.LastIndexByte(line, ')')
	if close < 0 || close+2 >= len(line) {
		return s, fmt.Errorf("unexpected /proc/%d/stat format", pid)
	}
	fields := strings.Fields(line[close+2:])
	// After the comm field, state is fields[0]; utime is field 14 overall,
	// i.e. fields[11] in this post-comm slice (14 - 3 for pid/comm/state).
	const utimeIdx, stimeIdx = 11, 12
	if len(fields) <= stimeIdx {
		return s, fmt.Errorf("unexpected /proc/%d/stat field count", pid)
	}
	u, err := strconv.ParseUint(fields[utimeIdx], 10, 64)
	if err != nil {
		return s, fmt.Errorf("parsing utime: %w", err)
	}
	st, err := strconv.ParseUint(fields[stimeIdx], 10, 64)
	if err != nil {
		return s, fmt.Errorf("parsing stime: %w", err)
	}
	s.UTicks, s.STicks = u, st

	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return s, err // CPU fields are still valid; caller decides whether that's enough
	}
	for _, l := range strings.Split(string(status), "\n") {
		if !strings.HasPrefix(l, "VmRSS:") {
			continue
		}
		fields := strings.Fields(l)
		if len(fields) >= 2 {
			if kb, err := strconv.Atoi(fields[1]); err == nil {
				s.RSSKB = kb
			}
		}
		break
	}
	return s, nil
}

// clockTicksPerSecond is the kernel's USER_HZ — almost always 100 on Linux,
// but "almost always" is exactly the kind of assumption a tuning tool should
// not silently bake in, so it is read from the system rather than hardcoded.
// getconf is present on every Linux base install; if it is somehow missing,
// 100 is the correct fallback for the overwhelming majority of real systems.
func clockTicksPerSecond() int {
	out, err := exec.Command("getconf", "CLK_TCK").Output()
	if err != nil {
		return 100
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || n <= 0 {
		return 100
	}
	return n
}

// procSupported reports whether this platform's /proc-based sampling is
// available, so callers can degrade instead of erroring loudly on a
// development machine that will never actually run this code path in
// production.
const procSupported = true
