package stats

import (
	"strings"
	"testing"

	"ngxsetup/internal/facts"
	"ngxsetup/internal/tmpl"
	"ngxsetup/internal/tuning"
)

func TestParseLogLine(t *testing.T) {
	cases := []struct {
		line string
		want string
		ok   bool
	}{
		{
			`203.0.113.4 - - [17/Aug/2026:17:04:51 +0000] "GET / HTTP/2.0" 200 5423 "-" "curl/8.5.0" rt=0.002 uct=- urt=0.002 cache=HIT host=shop.example.com`,
			"HIT", true,
		},
		{
			`203.0.113.4 - - [17/Aug/2026:17:04:51 +0000] "GET /wp-admin/ HTTP/2.0" 302 0 "-" "-" rt=0.041 uct=0.040 urt=0.041 cache=BYPASS host=shop.example.com`,
			"BYPASS", true,
		},
		{"garbage line with no cache field", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := ParseLogLine(c.line)
		if ok != c.ok {
			t.Errorf("ParseLogLine(%q) ok = %v, want %v", c.line, ok, c.ok)
			continue
		}
		if ok && got.CacheStatus != c.want {
			t.Errorf("ParseLogLine(%q) = %q, want %q", c.line, got.CacheStatus, c.want)
		}
	}
}

func TestLogEntryCached(t *testing.T) {
	cached := []string{"HIT", "STALE", "UPDATING"}
	for _, s := range cached {
		if !(LogEntry{CacheStatus: s}).Cached() {
			t.Errorf("%s should count as cached (visitor got an instant response)", s)
		}
	}
	uncached := []string{"MISS", "BYPASS", "EXPIRED", "-", ""}
	for _, s := range uncached {
		if (LogEntry{CacheStatus: s}).Cached() {
			t.Errorf("%s should not count as cached", s)
		}
	}
}

func TestAggregateLinesAndHitPercent(t *testing.T) {
	lines := []string{
		`a - - [x] "GET / HTTP/2" 200 1 "-" "-" rt=0 uct=- urt=- cache=HIT host=x`,
		`a - - [x] "GET / HTTP/2" 200 1 "-" "-" rt=0 uct=- urt=- cache=HIT host=x`,
		`a - - [x] "GET / HTTP/2" 200 1 "-" "-" rt=0 uct=- urt=- cache=MISS host=x`,
		`a - - [x] "GET /wp-admin/ HTTP/2" 302 0 "-" "-" rt=0 uct=- urt=- cache=BYPASS host=x`,
		"a truncated line from a mid-write read",
	}
	w := AggregateLines(lines)
	if w.Requests != 4 {
		t.Errorf("Requests = %d, want 4 (the truncated line must be skipped, not counted)", w.Requests)
	}
	if w.Hits != 2 {
		t.Errorf("Hits = %d, want 2", w.Hits)
	}
	if got := w.HitPercent(); got < 49.9 || got > 50.1 {
		t.Errorf("HitPercent = %.2f, want 50", got)
	}
}

// "No traffic" and "traffic, zero hits" must render differently on a live
// dashboard — a quiet site should not look identical to a site whose cache
// just stopped working.
func TestHitPercentEmptyWindowIsDistinctFromZero(t *testing.T) {
	empty := Window{}
	if got := empty.HitPercent(); got != -1 {
		t.Errorf("empty window HitPercent = %.2f, want -1 (sentinel for \"no data\")", got)
	}
	allMiss := Window{Requests: 5, Hits: 0}
	if got := allMiss.HitPercent(); got != 0 {
		t.Errorf("all-miss window HitPercent = %.2f, want 0", got)
	}
}

// This is the guard the comment on LogEntry promises: if nginx.conf.tmpl's
// log_format ever changes shape, this must fail loudly rather than let
// ParseLogLine silently stop matching real log lines in production.
func TestLogFormatContractMatchesTemplate(t *testing.T) {
	plan := tuning.Compute(facts.Facts{CPUCores: 2, MemTotalMB: 2048}, tuning.Options{})
	out, err := tmpl.Render("nginx/nginx.conf.tmpl", tmpl.Global{
		Plan: plan, ACMERoot: "/var/www/_acme", Resolvers: "127.0.0.53",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)
	if !strings.Contains(body, "cache=$upstream_cache_status") {
		t.Fatal("nginx.conf.tmpl's log_format no longer emits cache=$upstream_cache_status; " +
			"update ParseLogLine (and this test) to match the new shape")
	}
}
