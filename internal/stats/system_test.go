package stats

import (
	"testing"
	"time"
)

// ---- host CPU ------------------------------------------------------------

const sampleProcStat = `cpu  200 10 100 3600 50 5 5 0 0 0
cpu0 100 5 50 1800 25 2 2 0 0 0
cpu1 100 5 50 1800 25 3 3 0 0 0
intr 12345 0 0 0
ctxt 98765
btime 1700000000
processes 4321
`

func TestParseHostCPUSample(t *testing.T) {
	s, err := parseHostCPUSample(sampleProcStat)
	if err != nil {
		t.Fatalf("parseHostCPUSample: %v", err)
	}
	wantIdle := uint64(3600 + 50) // idle + iowait
	if s.Idle != wantIdle {
		t.Errorf("Idle = %d, want %d", s.Idle, wantIdle)
	}
	wantTotal := uint64(200 + 10 + 100 + 3600 + 50 + 5 + 5 + 0 + 0 + 0)
	if s.Total != wantTotal {
		t.Errorf("Total = %d, want %d", s.Total, wantTotal)
	}
}

func TestParseHostCPUSampleMissingLine(t *testing.T) {
	if _, err := parseHostCPUSample("intr 1 2 3\nctxt 4\n"); err == nil {
		t.Error("parseHostCPUSample accepted input with no cpu line")
	}
}

func TestHostCPUPercent(t *testing.T) {
	prev := HostCPUSample{Idle: 3650, Total: 4000}
	// 1 second later: 100 more total ticks, 50 more idle -> 50% busy.
	cur := HostCPUSample{Idle: 3700, Total: 4100}
	got := HostCPUPercent(prev, cur)
	if got < 49.9 || got > 50.1 {
		t.Errorf("HostCPUPercent = %v, want ~50", got)
	}
}

func TestHostCPUPercentFullyIdle(t *testing.T) {
	prev := HostCPUSample{Idle: 1000, Total: 1000}
	cur := HostCPUSample{Idle: 1100, Total: 1100}
	if got := HostCPUPercent(prev, cur); got != 0 {
		t.Errorf("HostCPUPercent = %v, want 0 (100%% idle)", got)
	}
}

func TestHostCPUPercentFullyBusy(t *testing.T) {
	prev := HostCPUSample{Idle: 1000, Total: 1000}
	cur := HostCPUSample{Idle: 1000, Total: 1100}
	if got := HostCPUPercent(prev, cur); got != 100 {
		t.Errorf("HostCPUPercent = %v, want 100", got)
	}
}

func TestHostCPUPercentCounterReset(t *testing.T) {
	prev := HostCPUSample{Idle: 5000, Total: 9000}
	cur := HostCPUSample{Idle: 10, Total: 20} // rebooted between samples
	if got := HostCPUPercent(prev, cur); got != 0 {
		t.Errorf("HostCPUPercent across a counter reset = %v, want 0", got)
	}
}

func TestHostCPUPercentNoElapsedTime(t *testing.T) {
	s := HostCPUSample{Idle: 100, Total: 200}
	if got := HostCPUPercent(s, s); got != 0 {
		t.Errorf("HostCPUPercent with no delta = %v, want 0", got)
	}
}

// ---- nginx stub_status -----------------------------------------------------

const sampleStubStatus = `Active connections: 3
server accepts handled requests
 16 16 42
Reading: 0 Writing: 1 Waiting: 2
`

func TestParseNginxStatus(t *testing.T) {
	s, err := parseNginxStatus(sampleStubStatus)
	if err != nil {
		t.Fatalf("parseNginxStatus: %v", err)
	}
	want := NginxStatus{Active: 3, Accepts: 16, Handled: 16, Requests: 42, Reading: 0, Writing: 1, Waiting: 2}
	if s != want {
		t.Errorf("parseNginxStatus = %+v, want %+v", s, want)
	}
}

func TestParseNginxStatusUnrecognised(t *testing.T) {
	if _, err := parseNginxStatus("<html>not stub_status</html>"); err == nil {
		t.Error("parseNginxStatus accepted unrecognised input")
	}
}

// ---- PHP-FPM status ---------------------------------------------------------

const sampleFPMStatus = "Content-type: application/json\r\n\r\n" +
	`{"pool":"example-com","process manager":"dynamic","start time":1787043628,"start since":12842,"accepted conn":1,"listen queue":0,"max listen queue":0,"listen queue len":0,"idle processes":3,"active processes":1,"total processes":4,"max active processes":1,"max children reached":0,"slow requests":0}`

func TestParseFPMStatus(t *testing.T) {
	s, err := parseFPMStatus(sampleFPMStatus)
	if err != nil {
		t.Fatalf("parseFPMStatus: %v", err)
	}
	if s.Pool != "example-com" {
		t.Errorf("Pool = %q", s.Pool)
	}
	if s.ActiveProcesses != 1 || s.IdleProcesses != 3 || s.TotalProcesses != 4 {
		t.Errorf("process counts = active:%d idle:%d total:%d", s.ActiveProcesses, s.IdleProcesses, s.TotalProcesses)
	}
	if s.ListenQueue != 0 || s.MaxChildrenReached != 0 || s.SlowRequests != 0 {
		t.Errorf("unexpected non-zero counters: %+v", s)
	}
}

// cgi-fcgi output uses a bare \n\n separator in practice on some systems;
// confirmed by inspecting real output live, both forms are handled.
func TestParseFPMStatusLFOnly(t *testing.T) {
	raw := "Content-type: application/json\n\n" + `{"pool":"x","process manager":"static","start time":1,"start since":1,"accepted conn":1,"listen queue":0,"max listen queue":0,"listen queue len":0,"idle processes":0,"active processes":1,"total processes":1,"max active processes":1,"max children reached":0,"slow requests":0}`
	s, err := parseFPMStatus(raw)
	if err != nil {
		t.Fatalf("parseFPMStatus (LF only): %v", err)
	}
	if s.Pool != "x" {
		t.Errorf("Pool = %q", s.Pool)
	}
}

func TestParseFPMStatusEmpty(t *testing.T) {
	if _, err := parseFPMStatus(""); err == nil {
		t.Error("parseFPMStatus accepted empty input")
	}
}

func TestParseFPMStatusMalformedJSON(t *testing.T) {
	if _, err := parseFPMStatus("Content-type: application/json\r\n\r\nnot json"); err == nil {
		t.Error("parseFPMStatus accepted malformed JSON")
	}
}

// ---- database status ---------------------------------------------------------

func TestComputeDatabaseStatusFirstSample(t *testing.T) {
	raw := map[string]string{
		"Threads_connected":                "5",
		"Threads_running":                  "1",
		"Slow_queries":                     "2",
		"Uptime":                           "3600",
		"Max_used_connections":             "10",
		"Questions":                        "100000",
		"Innodb_buffer_pool_read_requests": "1000000",
		"Innodb_buffer_pool_reads":         "1000",
	}
	status, sample := computeDatabaseStatus(raw, dbCounterSample{}, time.Now())
	if status.ThreadsConnected != 5 || status.ThreadsRunning != 1 || status.SlowQueries != 2 || status.UptimeSec != 3600 || status.MaxUsedConnections != 10 {
		t.Errorf("basic counters wrong: %+v", status)
	}
	// No previous sample: rate cannot be computed yet.
	if status.QueriesPerSec != 0 {
		t.Errorf("QueriesPerSec on first sample = %v, want 0", status.QueriesPerSec)
	}
	wantHit := (1 - 1000.0/1000000.0) * 100
	if diff := status.BufferPoolHitPercent - wantHit; diff > 0.01 || diff < -0.01 {
		t.Errorf("BufferPoolHitPercent = %v, want ~%v", status.BufferPoolHitPercent, wantHit)
	}
	if sample.questions != 100000 {
		t.Errorf("returned sample.questions = %d, want 100000", sample.questions)
	}
}

func TestComputeDatabaseStatusRateBetweenSamples(t *testing.T) {
	t0 := time.Now()
	prev := dbCounterSample{at: t0, questions: 100000}
	raw := map[string]string{"Questions": "105000"}
	status, _ := computeDatabaseStatus(raw, prev, t0.Add(5*time.Second))
	// 5000 queries over 5 seconds = 1000 qps.
	if status.QueriesPerSec < 999 || status.QueriesPerSec > 1001 {
		t.Errorf("QueriesPerSec = %v, want ~1000", status.QueriesPerSec)
	}
}

func TestComputeDatabaseStatusCounterResetIgnored(t *testing.T) {
	t0 := time.Now()
	prev := dbCounterSample{at: t0, questions: 100000}
	// Questions went backwards (server restarted) — must not report a
	// negative or nonsensical rate.
	raw := map[string]string{"Questions": "50"}
	status, _ := computeDatabaseStatus(raw, prev, t0.Add(5*time.Second))
	if status.QueriesPerSec != 0 {
		t.Errorf("QueriesPerSec across a counter reset = %v, want 0", status.QueriesPerSec)
	}
}

func TestComputeDatabaseStatusNoInnoDBActivityYet(t *testing.T) {
	raw := map[string]string{"Innodb_buffer_pool_read_requests": "0", "Innodb_buffer_pool_reads": "0"}
	status, _ := computeDatabaseStatus(raw, dbCounterSample{}, time.Now())
	if status.BufferPoolHitPercent != -1 {
		t.Errorf("BufferPoolHitPercent with no InnoDB activity = %v, want -1", status.BufferPoolHitPercent)
	}
}
