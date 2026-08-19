package stats

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeDBQuerier struct {
	out string
	err error
	// calls counts invocations, so tests can assert the refresh interval is
	// actually respected rather than querying on every single sample.
	calls int
}

func (f *fakeDBQuerier) Query(ctx context.Context, sql string) (string, error) {
	f.calls++
	return f.out, f.err
}

// GlobalStatus is a bare stub to satisfy the widened dbSizeQuerier interface
// — none of the tests in this file exercise SampleSystem, which is what
// actually calls it (see system_test.go for that arithmetic in isolation).
func (f *fakeDBQuerier) GlobalStatus(ctx context.Context) (map[string]string, error) {
	return nil, nil
}

// A site with no PHP-FPM workers found (impossible on this dev platform,
// since procSupported is false here) must not stop the DB size and access
// log parts of its sample from working — those are independent data sources
// and one failing must not blank the others.
func TestSampleDBAndLogWorkWithoutProcSupport(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	os.WriteFile(logPath, nil, 0o644)

	db := &fakeDBQuerier{out: "example_db_a1b2\t2097152\n"}
	s := NewSampler(db)
	s.dbInterval = 0 // refresh every call, for a deterministic test

	sites := []Site{{Slug: "example-com", Domain: "example.com", DBName: "example_db_a1b2", AccessLog: logPath, MaxWorkers: 10}}

	first := s.Sample(context.Background(), sites)
	if len(first) != 1 {
		t.Fatalf("got %d results, want 1", len(first))
	}
	if first[0].DBSizeMB != 2 {
		t.Errorf("DBSizeMB = %d, want 2", first[0].DBSizeMB)
	}
	if !procSupported && first[0].Err == nil {
		t.Error("expected an Err on a platform without /proc support")
	}
}

// The DB size query must not fire on every single tick — it is a real query
// against the live database, and a dashboard refreshing every second has no
// business hitting it that often.
func TestSampleRespectsDBRefreshInterval(t *testing.T) {
	db := &fakeDBQuerier{out: "x\t1048576\n"}
	s := NewSampler(db)
	s.dbInterval = time.Hour // effectively "never refresh again after the first call"

	sites := []Site{{Slug: "x", DBName: "x"}}
	s.Sample(context.Background(), sites)
	s.Sample(context.Background(), sites)
	s.Sample(context.Background(), sites)

	if db.calls != 1 {
		t.Errorf("database was queried %d times across 3 samples, want 1 (respecting dbInterval)", db.calls)
	}
}

// A nil db querier (database unreachable when the dashboard opened) must
// degrade DBSizeMB to zero rather than panicking or blocking startup.
func TestSampleWithNilDB(t *testing.T) {
	s := NewSampler(nil)
	sites := []Site{{Slug: "x", DBName: "x"}}
	got := s.Sample(context.Background(), sites)
	if len(got) != 1 || got[0].DBSizeMB != 0 {
		t.Errorf("got %+v, want a single result with DBSizeMB 0", got)
	}
}

// A site with no database (a plain, non-WordPress vhost) must not pick up
// another site's size through an accidental empty-string map key collision.
func TestSampleSiteWithoutDBName(t *testing.T) {
	db := &fakeDBQuerier{out: "\t999999999\n"} // pathological: empty schema name with a size
	s := NewSampler(db)
	s.dbInterval = 0
	sites := []Site{{Slug: "plain-site", DBName: ""}}
	got := s.Sample(context.Background(), sites)
	if got[0].DBSizeMB != 0 {
		t.Errorf("a site with no database must never report a size, got %d MB", got[0].DBSizeMB)
	}
}

// The request-rate window must reflect only lines appended between samples,
// and must not divide by zero or report a rate on the very first sample
// (there is no "since last time" yet).
func TestSampleAccessLogRate(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	os.WriteFile(logPath, nil, 0o644)

	s := NewSampler(nil)
	sites := []Site{{Slug: "x", AccessLog: logPath}}

	first := s.Sample(context.Background(), sites)
	if first[0].ReqPerSec != 0 {
		t.Errorf("first sample ReqPerSec = %.2f, want 0 (no prior baseline)", first[0].ReqPerSec)
	}

	f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	for i := 0; i < 5; i++ {
		f.WriteString(`a - - [x] "GET / HTTP/2" 200 1 "-" "-" rt=0 uct=- urt=- cache=HIT host=x` + "\n")
	}
	f.Close()

	second := s.Sample(context.Background(), sites)
	if second[0].TotalRequests != 5 {
		t.Errorf("TotalRequests = %d, want 5", second[0].TotalRequests)
	}
	if second[0].CacheHitPercent != 100 {
		t.Errorf("CacheHitPercent = %.2f, want 100", second[0].CacheHitPercent)
	}
}

// Forget must isolate a reused slug from a previous occupant's CPU baseline
// and log offset — otherwise a removed-then-recreated site under the same
// slug could show a CPU spike or request-rate artifact from data that
// belongs to whatever used to run there.
func TestForgetIsolatesReusedSlug(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	os.WriteFile(logPath, []byte("old site's traffic\n"), 0o644)

	s := NewSampler(nil)
	s.Sample(context.Background(), []Site{{Slug: "reused", AccessLog: logPath}})

	s.Forget("reused", logPath)
	if _, stillTracked := s.prevProc["reused"]; stillTracked {
		t.Error("Forget did not clear the CPU baseline")
	}

	os.WriteFile(logPath, []byte("new site's first request\n"), 0o644)
	got := s.Sample(context.Background(), []Site{{Slug: "reused", AccessLog: logPath}})
	// Post-Forget, the tailer treats the path as newly observed again and
	// baselines at end-of-file, so this line — already on disk at Sample
	// time — is correctly not counted as "new" activity.
	if got[0].TotalRequests != 0 {
		t.Errorf("TotalRequests = %d, want 0 (Forget re-baselines rather than replaying)", got[0].TotalRequests)
	}
}
