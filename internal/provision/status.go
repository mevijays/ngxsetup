package provision

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"ngxsetup/internal/facts"
	"ngxsetup/internal/logx"
	"ngxsetup/internal/state"
	"ngxsetup/internal/system"
)

// Status reports the live state of the machine and the stack.
//
// This replaces the old `loadcheck`, which stopped nginx whenever the load
// average exceeded the core count. That converts a busy server into a down
// server — precisely the wrong response, since the load is usually caused by
// the traffic you were hoping to serve. Reporting is the correct behaviour;
// shedding load belongs in the rate limiters, which are configured to do it
// per-client rather than by taking the whole site off the internet.
func (c *Ctx) Status() ([][2]string, error) {
	f := facts.Detect(facts.OSSource{})
	rows := [][2]string{
		{"host", hostname()},
		{"os", f.OS.PrettyName},
		{"uptime", uptime()},
	}

	load1, load5, load15 := loadAverages()
	cores := f.CPUCores
	verdict := "normal"
	switch {
	case load1 > float64(cores)*2:
		verdict = "saturated"
	case load1 > float64(cores):
		verdict = "busy"
	}
	rows = append(rows, [2]string{"load",
		fmt.Sprintf("%.2f %.2f %.2f across %d core(s) — %s", load1, load5, load15, cores, verdict)})

	if f.MemTotalMB > 0 {
		usedPct := 0
		if f.MemAvailMB > 0 {
			usedPct = 100 - (f.MemAvailMB * 100 / f.MemTotalMB)
		}
		rows = append(rows, [2]string{"memory",
			fmt.Sprintf("%d MB used of %d MB (%d%%), %d MB available",
				f.MemTotalMB-f.MemAvailMB, f.MemTotalMB, usedPct, f.MemAvailMB)})
		if f.SwapMB > 0 {
			rows = append(rows, [2]string{"swap", fmt.Sprintf("%d MB configured", f.SwapMB)})
		}
	}

	st := facts.DetectStorage(facts.OSSource{}, "/var")
	if st.TotalMB > 0 {
		rows = append(rows, [2]string{"disk",
			fmt.Sprintf("%d MB free of %d MB", st.FreeMB, st.TotalMB)})
	}

	for _, u := range []struct{ unit, label string }{
		{"nginx.service", "nginx"},
		{c.DBUnit, "database"},
		{"fail2ban.service", "fail2ban"},
	} {
		if u.unit == "" || !system.UnitExists(c.Context, c.Runner, u.unit) {
			continue
		}
		state := "stopped"
		if system.IsActive(c.Context, c.Runner, u.unit) {
			state = "running"
		}
		rows = append(rows, [2]string{u.label, state})
	}

	if n := len(c.State.Sites); n > 0 {
		enabled := 0
		for _, s := range c.State.Sites {
			if s.Enabled {
				enabled++
			}
		}
		rows = append(rows, [2]string{"sites", fmt.Sprintf("%d configured, %d enabled", n, enabled)})
	}

	if entries, bytesUsed, err := c.CacheStats(); err == nil && entries > 0 {
		rows = append(rows, [2]string{"cache",
			fmt.Sprintf("%d entries, %d MB of %d MB capacity",
				entries, bytesUsed/(1<<20), c.Plan.Nginx.CacheMaxSizeMB)})
	}

	rows = append(rows, [2]string{"profile", string(c.Plan.Profile)})
	if c.State.LastAppliedAt != "" {
		rows = append(rows, [2]string{"last applied", c.State.LastAppliedAt})
	}
	return rows, nil
}

// ReloadServices asks each service to pick up new configuration.
//
// nginx and PHP-FPM reload without dropping connections. The database is left
// alone: its settings were already activated by the restart that validated
// them, and restarting it again would be an outage for no reason.
func (c *Ctx) ReloadServices() error {
	if err := system.Reload(c.Context, c.Runner, "nginx.service"); err != nil {
		return err
	}
	// Each site's isolated FPM instance is reloaded individually. Reloading
	// the shared unit would do nothing useful — it is disabled and owns no
	// pools — while missing every service that actually serves traffic.
	for _, s := range c.State.Sites {
		unit := fpmUnitFor(s.Slug)
		if !system.UnitExists(c.Context, c.Runner, unit) {
			continue
		}
		if err := system.Reload(c.Context, c.Runner, unit); err != nil {
			return err
		}
	}
	return nil
}

// UpgradeToTLS obtains a real certificate for a site that does not have one,
// and rewrites its server block to use it.
func (c *Ctx) UpgradeToTLS(nameOrSlug string) error {
	rec, err := c.State.Find(nameOrSlug)
	if err != nil {
		return err
	}
	if rec.CertSource == "letsencrypt" {
		logx.Info("%s already has a Let's Encrypt certificate; renew it with `ngxsetup ssl renew`", rec.Domain)
		return nil
	}

	updated := *rec
	if err := c.issueLetsEncrypt(&updated); err != nil {
		return err
	}
	if err := c.writeSiteConfigs(updated); err != nil {
		return err
	}
	if err := c.ValidateNginx(); err != nil {
		return err
	}
	if c.Writer.DryRun {
		return nil
	}
	if err := system.Reload(c.Context, c.Runner, "nginx.service"); err != nil {
		return err
	}
	c.State.Upsert(updated)
	if err := c.State.Save(); err != nil {
		return err
	}
	logx.Change("%s now serves HTTPS with a Let's Encrypt certificate", updated.Domain)
	return nil
}

// ---- small system readers --------------------------------------------------

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func loadAverages() (float64, float64, float64) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return 0, 0, 0
	}
	one, _ := strconv.ParseFloat(f[0], 64)
	five, _ := strconv.ParseFloat(f[1], 64)
	fifteen, _ := strconv.ParseFloat(f[2], 64)
	return one, five, fifteen
}

func uptime() string {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return "unknown"
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return "unknown"
	}
	secs, _ := strconv.ParseFloat(f[0], 64)
	d := int(secs) / 86400
	h := (int(secs) % 86400) / 3600
	m := (int(secs) % 3600) / 60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh", d, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// ensure the state package stays referenced for the Site type used above.
var _ = state.Site{}
