package stats

import (
	"context"
	"time"
)

// Site is the subset of a registered site the sampler needs. Kept narrow and
// separate from state.Site so this package does not import provision/state
// just to read three fields, and so tests can supply fixtures directly.
type Site struct {
	Slug   string
	Domain string
	// DBName is the site's actual database schema name, which is NOT the
	// slug — site.DBName() appends a random suffix (e.g. slug
	// "shop-example-com" but schema "shop_example_com_b7oa") specifically so
	// a recreated site never silently inherits a previous one's data. Empty
	// for a site with no WordPress database (a plain vhost).
	DBName     string
	AccessLog  string // full path to this site's access log
	MaxWorkers int    // pm.max_children, the same for every site on the box
	// SocketPath is the site's PHP-FPM pool socket. Empty disables the
	// FPM-status fields below rather than erroring — a site record from
	// before SocketPath existed, or a plain non-PHP vhost, simply reports
	// them as unavailable (-1).
	SocketPath string
}

// Sampler produces a []SiteStats on each call, computing CPU% and request
// rate as deltas against whatever it remembered from the previous call. It
// is not safe for concurrent use — the TUI drives one sampler from one
// goroutine on a ticker, which is the only usage this needs to support.
type Sampler struct {
	tailer      *Tailer
	ticksPerSec int
	db          dbSizeQuerier // nil disables DB sizing entirely

	lastSampleAt time.Time
	prevProc     map[string][]ProcSample // slug -> previous CPU/mem sample

	dbSizes    map[string]int64
	dbSizedAt  time.Time
	dbInterval time.Duration

	// prevHostCPU/hostCPUSampled and prevDB are SampleSystem's own delta
	// state (system.go), kept on the same Sampler as the per-site state
	// above rather than a second type, since both need the "one long-lived
	// instance across polls" lifetime a fresh Ctx per request cannot give.
	prevHostCPU    HostCPUSample
	hostCPUSampled bool
	prevDB         dbCounterSample
}

// NewSampler builds a Sampler. db may be nil — DB size simply reads as
// unavailable rather than the whole dashboard failing to start when the
// database is unreachable at the moment the TUI opens.
func NewSampler(db dbSizeQuerier) *Sampler {
	return &Sampler{
		tailer:      NewTailer(),
		ticksPerSec: clockTicksPerSecond(),
		db:          db,
		dbInterval:  10 * time.Second,
	}
}

// Sample gathers one snapshot for every given site. A failure specific to one
// site (its pool has no live workers right now, its log is unreadable) is
// recorded on that site's SiteStats.Err rather than aborting the batch — one
// broken site must not blank the dashboard for every other site on the box.
func (s *Sampler) Sample(ctx context.Context, sites []Site) []SiteStats {
	now := time.Now()
	elapsed := sampleWindow
	if !s.lastSampleAt.IsZero() {
		elapsed = now.Sub(s.lastSampleAt)
	}
	first := s.lastSampleAt.IsZero()
	s.lastSampleAt = now

	if s.prevProc == nil {
		s.prevProc = make(map[string][]ProcSample)
	}

	s.maybeRefreshDBSizes(ctx, now)

	out := make([]SiteStats, 0, len(sites))
	for _, site := range sites {
		out = append(out, s.sampleSite(ctx, site, elapsed, first))
	}
	return out
}

func (s *Sampler) sampleSite(ctx context.Context, site Site, elapsed time.Duration, first bool) SiteStats {
	stat := SiteStats{
		Slug: site.Slug, Domain: site.Domain, MaxWorkers: site.MaxWorkers,
		FPMListenQueue: -1, FPMMaxChildrenReached: -1, FPMSlowRequests: -1,
	}

	if site.SocketPath != "" {
		if fpm, err := QueryFPMStatus(ctx, site.SocketPath, "/"+site.Slug+"-fpm-status"); err == nil {
			stat.FPMListenQueue = fpm.ListenQueue
			stat.FPMMaxChildrenReached = fpm.MaxChildrenReached
			stat.FPMSlowRequests = fpm.SlowRequests
		}
	}

	// Process, access-log and database sizing are three independent data
	// sources. A failure in one — most commonly /proc simply not existing on
	// whatever platform this happens to run on — must still let the other
	// two report normally; stat.Err flags the partial result without
	// discarding what could be gathered.
	switch {
	case !procSupported:
		stat.Err = errUnsupportedPlatform
	default:
		pids, err := PoolPIDs(site.Slug)
		if err != nil {
			stat.Err = err
			break
		}
		cur := make([]ProcSample, 0, len(pids))
		for _, pid := range pids {
			sample, err := ReadProcSample(pid)
			if err != nil {
				continue // process exited between listing and reading it; skip, don't fail the site
			}
			cur = append(cur, sample)
		}
		stat.Workers = len(cur)
		stat.MemoryMB = TotalRSSMB(cur)

		prev := s.prevProc[site.Slug]
		if !first {
			stat.CPUPercent = CPUPercent(prev, cur, elapsed, s.ticksPerSec)
		}
		s.prevProc[site.Slug] = cur
	}

	if site.AccessLog != "" {
		lines, err := s.tailer.Lines(site.AccessLog)
		if err == nil {
			window := AggregateLines(lines)
			stat.TotalRequests = int64(window.Requests)
			stat.CacheHitPercent = window.HitPercent()
			if !first && elapsed > 0 {
				stat.ReqPerSec = float64(window.Requests) / elapsed.Seconds()
			}
		}
	} else {
		stat.CacheHitPercent = -1
	}

	if s.dbSizes != nil && site.DBName != "" {
		if mb, ok := s.dbSizes[site.DBName]; ok {
			stat.DBSizeMB = mb
		}
	}

	return stat
}

// maybeRefreshDBSizes re-queries schema sizes at most once per dbInterval —
// unlike CPU and request rate, this costs a real database round trip, and a
// dashboard refreshing every second has no need for a number that does not
// meaningfully change second to second.
func (s *Sampler) maybeRefreshDBSizes(ctx context.Context, now time.Time) {
	if s.db == nil {
		return
	}
	if !s.dbSizedAt.IsZero() && now.Sub(s.dbSizedAt) < s.dbInterval {
		return
	}
	sizes, err := schemaSizesMB(ctx, s.db)
	if err != nil {
		return // keep the previous values rather than blanking them on a transient DB hiccup
	}
	s.dbSizes = sizes
	s.dbSizedAt = now
}

// Forget drops remembered state for a slug, called when a site is removed so
// a later site reusing the same slug does not inherit a stale CPU baseline.
func (s *Sampler) Forget(slug, accessLog string) {
	delete(s.prevProc, slug)
	if accessLog != "" {
		s.tailer.Reset(accessLog)
	}
}

var errUnsupportedPlatform = errPlatform{}

type errPlatform struct{}

func (errPlatform) Error() string { return "process sampling is not available on this platform" }
