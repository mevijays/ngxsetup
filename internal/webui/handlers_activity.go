package webui

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"ngxsetup/internal/stats"
)

// activityWindowLines bounds how much of a site's access log the activity
// view reads to build its IP/geo breakdown — a fixed, small tail rather than
// the whole file, so a year-old, multi-gigabyte access log costs the same to
// sample as a brand-new one. This is the concrete "not putting load on the
// server" answer for this endpoint: one bounded disk read, no continuous
// background scanning, computed only when an operator has a site's detail
// panel open.
const activityWindowLines = 2000

// maxGeoLookups caps how many distinct IPs get a country lookup per call —
// the log window can contain thousands of addresses under real traffic, and
// a chart with the top handful of countries answers "who is visiting" just
// as well as one with all of them, for a fraction of the lookup cost.
const maxGeoLookups = 50

type ipCount struct {
	IP    string `json:"ip"`
	Count int    `json:"count"`
}

func (s *Server) handleSiteActivity(w http.ResponseWriter, r *http.Request) {
	c, err := newCtx(r.Context(), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	site, err := c.State.Find(r.PathValue("domain"))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}

	accessLog := fmt.Sprintf("/var/log/nginx/%s.access.log", site.Slug)
	linesRead, _, err := snapshotTail(accessLog, activityWindowLines)
	if err != nil {
		// A brand-new site with no traffic yet has nothing to report, not
		// an error — the access log may not exist until the first request.
		linesRead = nil
	}

	counts := map[string]int{}
	var order []string
	for _, line := range linesRead {
		ip := firstField(line)
		if ip == "" {
			continue
		}
		if _, seen := counts[ip]; !seen {
			order = append(order, ip)
		}
		counts[ip]++
	}

	top := make([]ipCount, 0, len(order))
	for _, ip := range order {
		top = append(top, ipCount{IP: ip, Count: counts[ip]})
	}
	sort.Slice(top, func(i, j int) bool { return top[i].Count > top[j].Count })
	distinctIPs := len(top)
	if len(top) > 20 {
		top = top[:20]
	}

	geo := map[string]any{"enabled": c.Config.GeoIPDatabasePath != ""}
	if c.Config.GeoIPDatabasePath != "" {
		lookupSet := order
		if len(lookupSet) > maxGeoLookups {
			lookupSet = lookupSet[:maxGeoLookups]
		}
		countries, ok := countryLookup(c.Config.GeoIPDatabasePath, lookupSet)
		if !ok {
			geo["enabled"] = false
			geo["error"] = "configured database could not be opened"
		} else {
			byCountry := map[string]int{}
			for ip, country := range countries {
				byCountry[country] += counts[ip]
			}
			type countryCount struct {
				Country string `json:"country"`
				Count   int    `json:"count"`
			}
			list := make([]countryCount, 0, len(byCountry))
			for country, n := range byCountry {
				list = append(list, countryCount{Country: country, Count: n})
			}
			sort.Slice(list, func(i, j int) bool { return list[i].Count > list[j].Count })
			geo["countries"] = list
		}
	}

	// Active PHP-FPM workers is the honest answer to "how many requests are
	// being handled for this site right now" — a cached, static response
	// never touches a worker at all, so this undercounts total traffic by
	// design in exchange for actually meaning "currently in flight" rather
	// than "recently seen."
	var current *stats.SiteStats
	if site.Enabled {
		samples := s.sampler.Sample(r.Context(), []stats.Site{{
			Slug: site.Slug, Domain: site.Domain, DBName: site.DBName,
			AccessLog: accessLog, MaxWorkers: c.Plan.PHP.MaxChildren,
			SocketPath: site.SocketPath,
		}})
		if len(samples) == 1 {
			current = &samples[0]
		}
	}

	resp := map[string]any{
		"domain":       site.Domain,
		"distinct_ips": distinctIPs,
		"top_ips":      top,
		"sample_lines": len(linesRead),
		"geo":          geo,
	}
	if current != nil {
		resp["workers"] = current.Workers
		resp["max_workers"] = current.MaxWorkers
		resp["req_per_sec"] = current.ReqPerSec
		resp["cache_hit_pct"] = current.CacheHitPercent
	}
	writeJSON(w, http.StatusOK, resp)
}

func firstField(line string) string {
	i := strings.IndexByte(line, ' ')
	if i < 0 {
		return ""
	}
	return line[:i]
}
