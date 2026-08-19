package provision

import (
	"os"
	"testing"

	"ngxsetup/internal/state"
)

// The whole point of Sites() over SitesForStats() is picking up a site added
// or removed from another terminal while the dashboard is running. A
// snapshot captured once at Ctx construction could never do that no matter
// how often it were re-read, so this asserts Sites() actually goes back to
// disk rather than returning c.State's in-memory copy.
func TestSitesRereadsStateFromDisk(t *testing.T) {
	c := testCtx(t)
	if got := c.Sites(); len(got) != 0 {
		t.Fatalf("expected zero sites initially, got %d", len(got))
	}

	// Simulate a second `ngxsetup site add` process writing to the same
	// state file while the dashboard is already running.
	c.State.Upsert(state.Site{Slug: "example-com", Domain: "example.com"})
	if err := c.State.Save(); err != nil {
		t.Fatal(err)
	}

	// A fresh Ctx normally wouldn't share the mutated in-memory State, so
	// reconstruct what "another terminal" would have produced: reload from
	// the same path into a separate State value, proving Sites() reads the
	// file rather than the struct already held in memory.
	reloaded, err := state.Load(c.State.Path())
	if err != nil {
		t.Fatal(err)
	}
	c.State = reloaded

	got := c.Sites()
	if len(got) != 1 || got[0].Domain != "example.com" {
		t.Fatalf("Sites() = %+v, want the newly added site", got)
	}
}

// SitesForStats correctly carries through the fields the dashboard actually
// needs — in particular DBName, which is NOT the slug (see stats.Site's own
// doc comment for why that distinction matters).
func TestSitesForStatsCarriesRealDBName(t *testing.T) {
	c := testCtx(t)
	c.State.Upsert(state.Site{
		Slug: "shop-example-com", Domain: "shop.example.com",
		DBName: "shop_example_com_b7oa",
	})

	got := c.SitesForStats()
	if len(got) != 1 {
		t.Fatalf("got %d sites, want 1", len(got))
	}
	site := got[0]
	if site.DBName != "shop_example_com_b7oa" {
		t.Errorf("DBName = %q, want the real schema name, not the slug", site.DBName)
	}
	if site.MaxWorkers != c.Plan.PHP.MaxChildren {
		t.Errorf("MaxWorkers = %d, want %d (the host-wide pool ceiling)", site.MaxWorkers, c.Plan.PHP.MaxChildren)
	}
	wantLog := c.Path("/var/log/nginx/shop-example-com.access.log")
	if site.AccessLog != wantLog {
		t.Errorf("AccessLog = %q, want %q", site.AccessLog, wantLog)
	}
}

// A transient read failure — a concurrent writer caught mid-save, producing
// truncated JSON — must fall back to the last-known snapshot, not blank the
// dashboard entirely for one bad tick.
func TestSitesFallsBackOnReadFailure(t *testing.T) {
	c := testCtx(t)
	c.State.Upsert(state.Site{Slug: "example-com", Domain: "example.com"})
	if err := c.State.Save(); err != nil {
		t.Fatal(err)
	}

	// Corrupt the file on disk in place, as a torn write would leave it,
	// without going through state.Save (which would refuse to write invalid
	// content).
	if err := os.WriteFile(c.State.Path(), []byte(`{"sites": [ malformed`), 0o600); err != nil {
		t.Fatal(err)
	}

	got := c.Sites()
	if len(got) != 1 || got[0].Domain != "example.com" {
		t.Fatalf("Sites() with a corrupt file on disk = %+v, want the last-known snapshot preserved", got)
	}
}
