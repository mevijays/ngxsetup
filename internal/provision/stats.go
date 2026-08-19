package provision

import (
	"path/filepath"

	"ngxsetup/internal/state"
	"ngxsetup/internal/stats"
)

// sitesFromState adapts a registry snapshot into what the live dashboard
// needs: kept here, rather than inside internal/stats or internal/tui, so
// neither of those packages has to depend on the much larger provision
// package just to learn what an access log path convention or a pool's
// worker ceiling is. *Ctx already satisfies tui.CachePurger structurally
// through PurgeCache below — no adapter needed for that half.
func (c *Ctx) sitesFromState(st *state.State) []stats.Site {
	out := make([]stats.Site, 0, len(st.Sites))
	for _, s := range st.Sites {
		out = append(out, stats.Site{
			Slug:       s.Slug,
			Domain:     s.Domain,
			DBName:     s.DBName,
			AccessLog:  c.Path(filepath.Join("/var/log/nginx", s.Slug+".access.log")),
			MaxWorkers: c.Plan.PHP.MaxChildren,
			SocketPath: s.SocketPath,
		})
	}
	return out
}

// SitesForStats returns the sites known at the time Ctx was constructed. Use
// this for a single, one-shot read; Sites (below) is what the live dashboard
// actually drives itself from.
func (c *Ctx) SitesForStats() []stats.Site { return c.sitesFromState(c.State) }

// Sites implements tui.SiteProvider by re-reading the registry from disk on
// every call, rather than returning c.State's in-memory snapshot from when
// the dashboard started. That distinction is the point: a site added or
// removed from another terminal while the dashboard is open must show up on
// the next tick, and an in-memory struct captured once at startup could
// never do that no matter how often it were re-read.
//
// A read failure (a concurrent writer mid-save, most likely) falls back to
// the last-known snapshot rather than blanking the whole dashboard over a
// transient race.
func (c *Ctx) Sites() []stats.Site {
	fresh, err := state.Load(c.State.Path())
	if err != nil {
		return c.sitesFromState(c.State)
	}
	c.State = fresh
	return c.sitesFromState(fresh)
}
