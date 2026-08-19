package stats

import "strings"

// LogEntry is the subset of one access-log line stats.go cares about, parsed
// out of the "main" log_format nginx.conf.tmpl defines:
//
//	$remote_addr - $remote_user [$time_local] "$request" $status $body_bytes_sent
//	"$http_referer" "$http_user_agent" rt=$request_time uct=$upstream_connect_time
//	urt=$upstream_response_time cache=$upstream_cache_status host=$host
//
// Parsing here depends on that exact suffix shape; if the log_format ever
// changes, TestLogFormatContractMatchesTemplate in accesslog_test.go is
// designed to fail loudly rather than let this silently stop matching.
type LogEntry struct {
	CacheStatus string // HIT, MISS, BYPASS, EXPIRED, STALE, UPDATING, or "-" when no cache zone applied
}

// Cached reports whether nginx served this request from the FastCGI cache
// without reaching PHP. STALE and UPDATING count as cached — the visitor got
// a response instantly either way, which is the thing the hit ratio is
// actually meant to measure.
func (e LogEntry) Cached() bool {
	switch e.CacheStatus {
	case "HIT", "STALE", "UPDATING":
		return true
	default:
		return false
	}
}

// ParseLogLine extracts the cache field from one access-log line. Everything
// before it (address, timestamp, request line, status) is deliberately
// unparsed — nothing here needs it, and a request line containing a quoted
// space is exactly the kind of input that turns a hand-rolled full parser
// into a source of panics.
func ParseLogLine(line string) (LogEntry, bool) {
	i := strings.Index(line, "cache=")
	if i < 0 {
		return LogEntry{}, false
	}
	rest := line[i+len("cache="):]
	if sp := strings.IndexByte(rest, ' '); sp >= 0 {
		rest = rest[:sp]
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return LogEntry{}, false
	}
	return LogEntry{CacheStatus: rest}, true
}

// Window summarises a batch of new log lines observed since the previous
// sample.
type Window struct {
	Requests int
	Hits     int
}

// HitPercent returns the share of this window's requests that were cache
// hits, or -1 when the window is empty — "no traffic" and "traffic, all
// misses" must render differently, and 0 cannot mean both.
func (w Window) HitPercent() float64 {
	if w.Requests == 0 {
		return -1
	}
	return (float64(w.Hits) / float64(w.Requests)) * 100
}

// AggregateLines folds a batch of raw log lines into a Window. Lines that do
// not match the expected shape (a partial line read mid-write, most often)
// are skipped rather than aborting the whole batch.
func AggregateLines(lines []string) Window {
	var w Window
	for _, line := range lines {
		entry, ok := ParseLogLine(line)
		if !ok {
			continue
		}
		w.Requests++
		if entry.Cached() {
			w.Hits++
		}
	}
	return w
}
