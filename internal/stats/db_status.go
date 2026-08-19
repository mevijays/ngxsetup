package stats

import (
	"strconv"
	"time"
)

// DatabaseStatus is a human-relevant summary derived from SHOW GLOBAL
// STATUS, for the web UI's live database performance graphs.
type DatabaseStatus struct {
	ThreadsConnected   int
	ThreadsRunning     int
	QueriesPerSec      float64
	SlowQueries        int64
	UptimeSec          int64
	MaxUsedConnections int
	// BufferPoolHitPercent is -1 when it cannot be computed (no InnoDB
	// activity yet, or a non-InnoDB-only server) — distinct from a real 0%,
	// the same "-1 means not applicable" convention SiteStats.CacheHitPercent
	// already uses.
	BufferPoolHitPercent float64
}

// dbCounterSample is the bit of state a rate calculation needs to carry
// from one poll to the next — just enough to compute a delta, not a full
// history.
type dbCounterSample struct {
	at        time.Time
	questions uint64
}

// computeDatabaseStatus derives DatabaseStatus from a raw SHOW GLOBAL
// STATUS map and the previous poll's counter sample, returning the new
// sample for the caller to keep. Split from the SQL query itself so the
// arithmetic — especially the rate calculation, the easiest part of this to
// get subtly wrong — can be tested against fixed input without a database.
func computeDatabaseStatus(raw map[string]string, prev dbCounterSample, now time.Time) (DatabaseStatus, dbCounterSample) {
	var status DatabaseStatus
	status.ThreadsConnected = atoiOr(raw["Threads_connected"], 0)
	status.ThreadsRunning = atoiOr(raw["Threads_running"], 0)
	status.SlowQueries = atoi64Or(raw["Slow_queries"], 0)
	status.UptimeSec = atoi64Or(raw["Uptime"], 0)
	status.MaxUsedConnections = atoiOr(raw["Max_used_connections"], 0)

	questions := atou64Or(raw["Questions"], 0)
	cur := dbCounterSample{at: now, questions: questions}
	if !prev.at.IsZero() && questions >= prev.questions {
		if elapsed := now.Sub(prev.at).Seconds(); elapsed > 0 {
			status.QueriesPerSec = float64(questions-prev.questions) / elapsed
		}
	}

	reads := atou64Or(raw["Innodb_buffer_pool_read_requests"], 0)
	misses := atou64Or(raw["Innodb_buffer_pool_reads"], 0)
	if reads > 0 && misses <= reads {
		status.BufferPoolHitPercent = (1 - float64(misses)/float64(reads)) * 100
	} else {
		status.BufferPoolHitPercent = -1
	}

	return status, cur
}

func atoiOr(s string, def int) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

func atoi64Or(s string, def int64) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return def
	}
	return v
}

func atou64Or(s string, def uint64) uint64 {
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return def
	}
	return v
}
