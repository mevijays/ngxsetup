package provision

import (
	"os"
	"path/filepath"

	"ngxsetup/internal/logx"
	"ngxsetup/internal/state"
	"ngxsetup/internal/system"
	"ngxsetup/internal/tmpl"
)

// Every WordPress site this tool writes a wp-config.php for has
// DISABLE_WP_CRON set: the visitor-triggered scheduler is replaced by a
// per-site systemd timer, so scheduled posts and plugin jobs run on time on
// a quiet site and no reader pays for maintenance work. These functions are
// what install that replacement — without them, DISABLE_WP_CRON alone means
// WordPress's scheduler never runs at all.

// wpCronInterval is how often each site's due events are run.
const wpCronInterval = "5min"

// wpCLIPath is where setup installs wp-cli.
const wpCLIPath = "/usr/local/bin/wp"

func wpCronUnitBase(slug string) string { return "ngxsetup-wpcron-" + slug }

func wpCronServicePath(slug string) string {
	return filepath.Join("/etc/systemd/system", wpCronUnitBase(slug)+".service")
}

func wpCronTimerPath(slug string) string {
	return filepath.Join("/etc/systemd/system", wpCronUnitBase(slug)+".timer")
}

func wpCronTimerUnit(slug string) string { return wpCronUnitBase(slug) + ".timer" }

// wpCronExecStart prefers wp-cli (which runs every due event and reports
// failures in the journal), falling back to PHP running wp-cron.php
// directly when wp-cli is unavailable — the same work, just less verbose.
func (c *Ctx) wpCronExecStart(rec state.Site) (start, condition string) {
	if _, err := os.Stat(c.Path(wpCLIPath)); err == nil {
		return wpCLIPath + " cron event run --due-now --quiet --path=" + rec.Root,
			wpCLIPath + " core is-installed --path=" + rec.Root
	}
	return "/usr/bin/php" + c.PHPVersion + " -f " + filepath.Join(rec.Root, "wp-cron.php"), ""
}

// WriteWPCronUnits renders one WordPress site's scheduler service and timer.
// A non-WordPress vhost has no scheduler and gets nothing.
func (c *Ctx) WriteWPCronUnits(rec state.Site) error {
	if !rec.WordPress {
		return nil
	}
	start, condition := c.wpCronExecStart(rec)
	data := tmpl.WPCron{
		Domain:        rec.Domain,
		User:          rec.User,
		Root:          rec.Root,
		SiteBase:      c.SiteRoot(rec.Slug),
		DBUnit:        c.DBUnit,
		Interval:      wpCronInterval,
		ExecStart:     start,
		ExecCondition: condition,
	}
	svc, err := tmpl.Render("system/wp-cron.service.tmpl", data)
	if err != nil {
		return err
	}
	if _, err := c.Writer.Write(wpCronServicePath(rec.Slug), svc, 0o644, false); err != nil {
		return err
	}
	timer, err := tmpl.Render("system/wp-cron.timer.tmpl", data)
	if err != nil {
		return err
	}
	_, err = c.Writer.Write(wpCronTimerPath(rec.Slug), timer, 0o644, false)
	return err
}

// StartWPCronTimer enables and starts a site's scheduler timer.
func (c *Ctx) StartWPCronTimer(rec state.Site) error {
	if !rec.WordPress {
		return nil
	}
	if c.Writer.DryRun {
		logx.Change("[dry-run] would enable %s", wpCronTimerUnit(rec.Slug))
		return nil
	}
	if err := system.DaemonReload(c.Context, c.Runner); err != nil {
		return err
	}
	if err := c.Runner.Run(c.Context, "systemctl", "enable", "--now", wpCronTimerUnit(rec.Slug)); err != nil {
		return err
	}
	logx.Change("enabled scheduled WordPress tasks for %s (every %s)", rec.Domain, wpCronInterval)
	return nil
}

// StopWPCronTimer stops a site's scheduler. With remove, its unit files are
// deleted too (site removal); without, they stay for a later re-enable.
func (c *Ctx) StopWPCronTimer(slug string, remove bool) {
	if !c.Writer.DryRun {
		c.Runner.TryRun(c.Context, "systemctl", "disable", "--now", wpCronTimerUnit(slug))
	}
	if !remove {
		return
	}
	for _, p := range []string{wpCronTimerPath(slug), wpCronServicePath(slug)} {
		if err := c.removeConfigPath(p); err != nil {
			logx.Warn("removing %s: %v", p, err)
		}
	}
}

// applyWPCronUnits writes every registered WordPress site's scheduler units —
// what brings sites created by a build that never installed them up to date
// on the next `tune --apply`.
func (c *Ctx) applyWPCronUnits() error {
	for _, s := range c.State.Sites {
		if err := c.WriteWPCronUnits(s); err != nil {
			return err
		}
	}
	return nil
}

// enableWPCronTimers turns on the timer of every enabled WordPress site whose
// timer is not already enabled. Called after a daemon-reload.
func (c *Ctx) enableWPCronTimers() {
	for _, s := range c.State.Sites {
		if !s.WordPress || !s.Enabled {
			continue
		}
		unit := wpCronTimerUnit(s.Slug)
		if system.IsEnabled(c.Context, c.Runner, unit) {
			continue
		}
		if err := c.Runner.Run(c.Context, "systemctl", "enable", "--now", unit); err != nil {
			logx.Warn("could not enable scheduled tasks for %s: %v", s.Domain, err)
			continue
		}
		logx.Change("enabled scheduled WordPress tasks for %s", s.Domain)
	}
}
