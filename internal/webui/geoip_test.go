package webui

import "testing"

// Geography is optional by design (see config's geoip_database_path doc) —
// every one of these must degrade to "not available" rather than error, so
// a site's activity page never breaks just because nobody configured a
// database.
func TestCountryLookupDegradesGracefullyWithNoDatabaseConfigured(t *testing.T) {
	countries, ok := countryLookup("", []string{"8.8.8.8"})
	if ok {
		t.Error("countryLookup reported ok with an empty database path")
	}
	if countries != nil {
		t.Error("countryLookup returned a non-nil map with no database configured")
	}
}

func TestCountryLookupDegradesGracefullyWithNoIPs(t *testing.T) {
	if _, ok := countryLookup("/some/path.mmdb", nil); ok {
		t.Error("countryLookup reported ok with no IPs to look up")
	}
}

func TestCountryLookupDegradesGracefullyWithUnreadableDatabase(t *testing.T) {
	if _, ok := countryLookup("/no/such/database-really-not-there.mmdb", []string{"8.8.8.8"}); ok {
		t.Error("countryLookup reported ok for a database file that does not exist")
	}
}
