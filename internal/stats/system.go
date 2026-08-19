package stats

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"ngxsetup/internal/facts"
)

// SystemStats is one host-wide snapshot for the web UI's Live Stats page —
// the machine-level counterpart to SiteStats, which is per-site. Everything
// here is either cheap to gather (a handful of /proc reads, the same facts
// package Status already uses) or, for nginx and the database, a single
// local round trip nginx and the database are already answering thousands
// of times a second for real traffic — nothing here adds meaningful load.
type SystemStats struct {
	Timestamp time.Time

	// CPUPercent is host-wide utilization, 0-100 regardless of core count —
	// see HostCPUPercent. 0 on the very first sample, before there is a
	// previous tick to diff against.
	CPUPercent float64
	Cores      int

	MemTotalMB     int
	MemUsedMB      int
	MemAvailMB     int
	MemUsedPercent float64
	SwapMB         int

	DiskPath        string
	DiskTotalMB     int
	DiskUsedMB      int
	DiskUsedPercent float64

	Load1, Load5, Load15 float64

	// Nginx and NginxErr are mutually informative: a non-nil error (nginx
	// down, or an older config from before the stub_status location
	// existed) means Nginx is a zero value and must not be plotted as "zero
	// connections" — the frontend is expected to show "unavailable" instead.
	Nginx    NginxStatus
	NginxErr error

	// DB and DBErr follow the same "nil error means the value is real"
	// convention. A nil db client at Sampler construction time (the database
	// was unreachable when the web server started) reports here as a
	// standing DBErr, not a silent zero.
	DB    DatabaseStatus
	DBErr error
}

// SampleSystem gathers one host-wide snapshot, computing CPU% and the
// database's queries/sec as deltas against whatever SampleSystem last saw —
// the same reasoning as Sample's per-site CPU%, on the same Sampler instance
// so both share one "previous tick" clock. Each of the five data sources
// (CPU, memory+disk, load, nginx, database) is independent: a database that
// is momentarily unreachable must not blank out CPU and memory, and vice
// versa.
func (s *Sampler) SampleSystem(ctx context.Context) SystemStats {
	now := time.Now()
	out := SystemStats{Timestamp: now}

	if cur, err := ReadHostCPUSample(); err == nil {
		if s.hostCPUSampled {
			out.CPUPercent = HostCPUPercent(s.prevHostCPU, cur)
		}
		s.prevHostCPU = cur
		s.hostCPUSampled = true
	}

	f := facts.Detect(facts.OSSource{})
	out.Cores = f.CPUCores
	out.MemTotalMB = f.MemTotalMB
	out.MemAvailMB = f.MemAvailMB
	out.SwapMB = f.SwapMB
	if f.MemTotalMB > 0 {
		out.MemUsedMB = f.MemTotalMB - f.MemAvailMB
		out.MemUsedPercent = float64(out.MemUsedMB) / float64(f.MemTotalMB) * 100
	}

	// /var is the same mount Status() reports disk usage for — where site
	// roots, the nginx cache and database data all actually live.
	st := facts.DetectStorage(facts.OSSource{}, "/var")
	out.DiskPath = st.Path
	out.DiskTotalMB = st.TotalMB
	if st.TotalMB > 0 {
		out.DiskUsedMB = st.TotalMB - st.FreeMB
		out.DiskUsedPercent = float64(out.DiskUsedMB) / float64(st.TotalMB) * 100
	}

	out.Load1, out.Load5, out.Load15 = readLoadAverage()

	if nx, err := QueryNginxStatus(ctx); err == nil {
		out.Nginx = nx
	} else {
		out.NginxErr = err
	}

	if s.db == nil {
		out.DBErr = errNoDBClient
	} else if raw, err := s.db.GlobalStatus(ctx); err != nil {
		out.DBErr = err
	} else {
		status, sample := computeDatabaseStatus(raw, s.prevDB, now)
		out.DB = status
		s.prevDB = sample
	}

	return out
}

// readLoadAverage reads /proc/loadavg directly rather than importing
// internal/provision for its own copy — provision already imports this
// package, so the reverse import would cycle. Duplicating ten lines is
// cheaper than restructuring either package over it.
func readLoadAverage() (float64, float64, float64) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0
	}
	fields := strings.Fields(string(b))
	if len(fields) < 3 {
		return 0, 0, 0
	}
	one, _ := strconv.ParseFloat(fields[0], 64)
	five, _ := strconv.ParseFloat(fields[1], 64)
	fifteen, _ := strconv.ParseFloat(fields[2], 64)
	return one, five, fifteen
}

type errNoDBClientType struct{}

func (errNoDBClientType) Error() string { return "no database client configured" }

var errNoDBClient = errNoDBClientType{}
