// Package stats gathers live, per-site resource consumption: PHP-FPM worker
// CPU and memory, nginx request rate and cache hit ratio, and database size.
//
// The design mirrors the rest of this codebase: the arithmetic (CPU
// percentage from a tick delta, cache hit ratio from a batch of log lines) is
// pure and tested without a running server, while the parts that actually
// touch the OS — /proc, log files, the database — are thin adapters kept
// separate so the math can be verified without root, without Linux, and
// without a live MariaDB.
package stats

import "time"

// SiteStats is one site's resource snapshot for a single sampling tick.
type SiteStats struct {
	Slug   string
	Domain string

	// CPUPercent is the sum of every PHP-FPM worker's CPU time for this site,
	// as a percentage of one core, measured over the interval since the
	// previous sample. 400% means four cores' worth of work — it is not
	// capped at 100, since a pool can and does use more than one core.
	CPUPercent float64
	// MemoryMB is the sum of every worker's resident set size. This is the
	// number that matters for "is this site about to cause swapping," not
	// virtual memory size.
	MemoryMB int
	// Workers is the number of live PHP-FPM worker processes found for this
	// site's pool right now; MaxWorkers is the pool's configured ceiling
	// (pm.max_children), the same for every site since tuning is host-wide.
	Workers    int
	MaxWorkers int

	// ReqPerSec is the request rate since the previous sample, computed from
	// new lines appended to the site's own access log.
	ReqPerSec float64
	// CacheHitPercent is the share of those new requests nginx served from
	// the FastCGI cache without reaching PHP. -1 means "no requests in this
	// window," which is distinct from 0% (requests arrived and none hit).
	CacheHitPercent float64
	TotalRequests   int64

	// DBSizeMB is the site's schema size (data + indexes). Refreshed on a
	// slower cadence than the rest — see Sampler — since it costs a real
	// query and does not change meaningfully second to second.
	DBSizeMB int64

	// FPM* fields come from PHP-FPM's own status page (see fpm_status.go),
	// independent of the process-scan-based Workers/CPUPercent/MemoryMB
	// above. -1 means unavailable — the site has no SocketPath recorded, or
	// the status query itself failed — distinct from a real 0, which a
	// healthy idle pool legitimately reports for all three.
	FPMListenQueue        int
	FPMMaxChildrenReached int
	FPMSlowRequests       int64

	// Err records a per-site failure (e.g. its pool has no live workers, or
	// its access log is not yet readable) without aborting the whole sample;
	// a problem with one site must not blank the dashboard for every other
	// site on the box.
	Err error
}

// Healthy reports whether this sample could be gathered without error.
func (s SiteStats) Healthy() bool { return s.Err == nil }

// sampleWindow is how CPUPercent and ReqPerSec express their rate: "per
// second," regardless of how often Sample is actually called.
const sampleWindow = time.Second
