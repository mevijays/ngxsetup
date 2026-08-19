package stats

import (
	"testing"
	"time"
)

// One worker consuming exactly one full core for the whole interval must
// read as 100%, the reference point every other case is checked against.
func TestCPUPercentFullCore(t *testing.T) {
	prev := []ProcSample{{PID: 1, UTicks: 0, STicks: 0}}
	cur := []ProcSample{{PID: 1, UTicks: 100, STicks: 0}} // 100 ticks = 1s at 100 ticks/sec
	got := CPUPercent(prev, cur, time.Second, 100)
	if got < 99.9 || got > 100.1 {
		t.Errorf("CPUPercent = %.2f, want ~100", got)
	}
}

// A pool is many workers; their CPU time sums, and a busy pool legitimately
// exceeds 100% — that is four cores of work, not a bug to clamp away.
func TestCPUPercentSumsAcrossWorkers(t *testing.T) {
	prev := []ProcSample{{PID: 1}, {PID: 2}, {PID: 3}, {PID: 4}}
	cur := []ProcSample{
		{PID: 1, UTicks: 100}, {PID: 2, UTicks: 100},
		{PID: 3, UTicks: 100}, {PID: 4, UTicks: 100},
	}
	got := CPUPercent(prev, cur, time.Second, 100)
	if got < 399 || got > 401 {
		t.Errorf("four full-core workers = %.2f, want ~400", got)
	}
}

func TestCPUPercentIdle(t *testing.T) {
	prev := []ProcSample{{PID: 1, UTicks: 500, STicks: 200}}
	cur := []ProcSample{{PID: 1, UTicks: 500, STicks: 200}}
	if got := CPUPercent(prev, cur, time.Second, 100); got != 0 {
		t.Errorf("no tick movement should read 0%%, got %.2f", got)
	}
}

// A worker that forked since the last sample (pool grew, or replaced a
// recycled child) has no prior baseline; it must not be treated as having
// consumed nothing, but also must not be blamed for the CPU time of whatever
// PID number it reused.
func TestCPUPercentNewWorkerNotCountedFromZero(t *testing.T) {
	prev := []ProcSample{{PID: 1, UTicks: 1000}}
	cur := []ProcSample{{PID: 1, UTicks: 1000}, {PID: 2, UTicks: 50}}
	got := CPUPercent(prev, cur, time.Second, 100)
	if got != 0 {
		t.Errorf("brand-new PID should contribute nothing to this sample, got %.2f", got)
	}
}

// pm.max_requests recycles workers; an exited PID's prior consumption must
// not be double-counted or produce a negative delta on the next tick.
func TestCPUPercentExitedWorkerDropped(t *testing.T) {
	prev := []ProcSample{{PID: 1, UTicks: 1000}, {PID: 2, UTicks: 500}}
	cur := []ProcSample{{PID: 1, UTicks: 1050}} // PID 2 exited, PID 3 not yet forked
	got := CPUPercent(prev, cur, time.Second, 100)
	if got < 49 || got > 51 {
		t.Errorf("only PID 1's delta (50 ticks = 0.5 CPU-second over 1 wall second = 50%%) should count, got %.2f", got)
	}
}

// A PID reused by an unrelated process between samples must not produce a
// deranged negative-then-huge delta from comparing unrelated tick counters.
func TestCPUPercentPIDReuseGuarded(t *testing.T) {
	prev := []ProcSample{{PID: 7, UTicks: 9000}}
	cur := []ProcSample{{PID: 7, UTicks: 10}} // counter lower than before => not the same process lineage
	got := CPUPercent(prev, cur, time.Second, 100)
	if got != 0 {
		t.Errorf("a lower tick count than the previous sample must be discarded, got %.2f", got)
	}
}

func TestCPUPercentGuardsAgainstZeroInputs(t *testing.T) {
	if got := CPUPercent(nil, nil, 0, 100); got != 0 {
		t.Errorf("zero elapsed time must not divide by zero, got %.2f", got)
	}
	if got := CPUPercent(nil, nil, time.Second, 0); got != 0 {
		t.Errorf("zero ticksPerSec must not divide by zero, got %.2f", got)
	}
}

func TestTotalRSSMB(t *testing.T) {
	samples := []ProcSample{{RSSKB: 51200}, {RSSKB: 20480}, {RSSKB: 10240}}
	if got := TotalRSSMB(samples); got != 80 {
		t.Errorf("TotalRSSMB = %d, want 80", got)
	}
}

func TestTotalRSSMBEmpty(t *testing.T) {
	if got := TotalRSSMB(nil); got != 0 {
		t.Errorf("TotalRSSMB(nil) = %d, want 0", got)
	}
}
