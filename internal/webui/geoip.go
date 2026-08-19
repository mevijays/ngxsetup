package webui

import (
	"net"

	"github.com/oschwald/maxminddb-golang"
)

// countryLookup resolves a small set of IPs to country names using an
// operator-supplied MaxMind-format database (see config's
// geoip_database_path). Opened fresh per call rather than held open on the
// Server: this runs only when someone is actively looking at a site's detail
// page, not on any polling path, so the cost of one mmap per lookup batch is
// not worth the complexity of tracking when a configured path changes.
//
// Returns (nil, false) whenever geography simply isn't available — no path
// configured, or the file can't be opened — so callers degrade to "not
// configured" rather than surfacing an error for what is an optional feature
// by design.
func countryLookup(dbPath string, ips []string) (map[string]string, bool) {
	if dbPath == "" || len(ips) == 0 {
		return nil, false
	}
	db, err := maxminddb.Open(dbPath)
	if err != nil {
		return nil, false
	}
	defer db.Close()

	var record struct {
		Country struct {
			Names map[string]string `maxminddb:"names"`
		} `maxminddb:"country"`
	}

	out := make(map[string]string, len(ips))
	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil || ip.IsPrivate() || ip.IsLoopback() {
			continue // nothing meaningful to look up for local/lab traffic
		}
		record.Country.Names = nil
		if err := db.Lookup(ip, &record); err != nil {
			continue
		}
		name := record.Country.Names["en"]
		if name == "" {
			continue
		}
		out[ipStr] = name
	}
	return out, true
}
