package provision

import (
	"fmt"
	"path/filepath"

	"ngxsetup/internal/logx"
	"ngxsetup/internal/state"
	"ngxsetup/internal/system"
	"ngxsetup/internal/tmpl"
)

// Locations for the per-site FPM services.
const (
	// FPMConfigDir holds one master config per site.
	FPMConfigDir = "/etc/ngxsetup/fpm"
	// FPMPoolDir holds the pool definitions those configs include. Kept out
	// of /etc/php/<v>/fpm/pool.d entirely so the distribution's own service —
	// which is disabled, but may be re-enabled by a package update — cannot
	// pick up a site's pool and run it outside its jail.
	FPMPoolDir = "/etc/ngxsetup/fpm/pools"
	// FPMRunDir holds each service's pid file.
	FPMRunDir = "/run/ngxsetup"
	// FPMUnitTemplate is the systemd template unit all instances share.
	FPMUnitTemplate = "/etc/systemd/system/ngxsetup-fpm@.service"
)

// fpmUnitFor returns the systemd unit name for one site.
func fpmUnitFor(slug string) string { return "ngxsetup-fpm@" + slug + ".service" }

func (c *Ctx) fpmConfigPath(slug string) string {
	return filepath.Join(FPMConfigDir, slug+".conf")
}
func (c *Ctx) fpmPoolPath(slug string) string {
	return filepath.Join(FPMPoolDir, slug+".conf")
}
func (c *Ctx) fpmLimitsPath(slug string) string {
	return filepath.Join("/etc/systemd/system", fpmUnitFor(slug)+".d", "limits.conf")
}

// fpmBinary is the PHP-FPM executable for the detected version.
func (c *Ctx) fpmBinary() string { return "/usr/sbin/php-fpm" + c.PHPVersion }

// ApplyFPMServiceTemplate writes the shared systemd template unit. One
// rendering serves every site; systemd expands %i to the slug per instance.
func (c *Ctx) ApplyFPMServiceTemplate() error {
	if c.PHPVersion == "" {
		return fmt.Errorf("PHP is not installed; run `ngxsetup setup` first")
	}
	for _, d := range []string{FPMConfigDir, FPMPoolDir} {
		if err := c.Writer.EnsureDir(d, 0o755, ""); err != nil {
			return err
		}
	}

	body, err := tmpl.Render("system/fpm-service.tmpl", tmpl.FPMService{
		PHPVersion:   c.PHPVersion,
		FPMBinary:    c.fpmBinary(),
		FPMConfigDir: FPMConfigDir,
		WebRoot:      WebRoot,
		PHPLogDir:    PHPLogDir,
		PHPSocketDir: PHPSocketDir,
		RunDir:       FPMRunDir,
		DBUnit:       c.DBUnit,
	})
	if err != nil {
		return err
	}
	if _, err := c.Writer.Write(FPMUnitTemplate, body, 0o644, false); err != nil {
		return err
	}

	// The pid directory lives under /run, which is a tmpfs cleared on every
	// boot, so it needs recreating rather than just existing once.
	tmpfiles := tmpl.ManagedHeader + "\nd " + FPMRunDir + " 0755 root root -\n"
	if _, err := c.Writer.Write("/etc/tmpfiles.d/ngxsetup.conf", []byte(tmpfiles), 0o644, false); err != nil {
		return err
	}
	if !c.Writer.DryRun {
		c.Runner.TryRun(c.Context, "systemd-tmpfiles", "--create", "/etc/tmpfiles.d/ngxsetup.conf")
		if err := system.DaemonReload(c.Context, c.Runner); err != nil {
			return err
		}
	}
	return nil
}

// WriteFPMService renders one site's pool, master config and resource cap.
// It does not start anything — callers validate first, then start.
func (c *Ctx) WriteFPMService(rec state.Site) error {
	poolBody, err := tmpl.Render("php/pool.conf.tmpl", tmpl.Pool{
		Plan:            c.Plan,
		Slug:            rec.Slug,
		Domain:          rec.Domain,
		User:            rec.User,
		Group:           rec.User,
		SocketPath:      rec.SocketPath,
		Root:            rec.Root,
		TmpDir:          c.siteTmpDir(rec.Slug),
		SessionDir:      c.siteSessionDir(rec.Slug),
		TLS:             rec.TLS,
		StrictFunctions: c.Config.StrictPHPFunctions,
	})
	if err != nil {
		return err
	}
	if _, err := c.Writer.Write(c.fpmPoolPath(rec.Slug), poolBody, 0o644, false); err != nil {
		return err
	}

	globalBody, err := tmpl.Render("php/fpm-global.conf.tmpl", tmpl.FPMGlobal{
		Domain:   rec.Domain,
		PidFile:  filepath.Join(FPMRunDir, "fpm-"+rec.Slug+".pid"),
		ErrorLog: filepath.Join(PHPLogDir, rec.Slug+"-fpm.log"),
		PoolFile: c.fpmPoolPath(rec.Slug),
	})
	if err != nil {
		return err
	}
	if _, err := c.Writer.Write(c.fpmConfigPath(rec.Slug), globalBody, 0o644, false); err != nil {
		return err
	}

	limitsBody, err := tmpl.Render("system/fpm-limits.conf.tmpl", tmpl.FPMLimits{
		Domain:      rec.Domain,
		MemoryMaxMB: c.Plan.Budget.PHPMB,
	})
	if err != nil {
		return err
	}
	if _, err := c.Writer.Write(c.fpmLimitsPath(rec.Slug), limitsBody, 0o644, false); err != nil {
		return err
	}
	return nil
}

// ValidateFPMService runs PHP-FPM's own config test against one site.
func (c *Ctx) ValidateFPMService(slug string) error {
	if !c.Runner.Look(c.fpmBinary()) && !c.Runner.Look("php-fpm"+c.PHPVersion) {
		return nil
	}
	out, err := c.Runner.CombinedOutput(c.Context, c.fpmBinary(),
		"-t", "--fpm-config", c.Path(c.fpmConfigPath(slug)))
	if err != nil {
		return fmt.Errorf("php-fpm rejected the configuration for %s:\n%s", slug, indentBlock(out))
	}
	return nil
}

// StartFPMService enables and starts one site's isolated FPM instance.
func (c *Ctx) StartFPMService(slug string) error {
	if c.Writer.DryRun {
		logx.Change("[dry-run] would enable and start %s", fpmUnitFor(slug))
		return nil
	}
	if err := system.DaemonReload(c.Context, c.Runner); err != nil {
		return err
	}
	unit := fpmUnitFor(slug)
	if err := c.Runner.Run(c.Context, "systemctl", "enable", unit); err != nil {
		return err
	}
	if err := c.Runner.Run(c.Context, "systemctl", "restart", unit); err != nil {
		return fmt.Errorf("%w\n%s", err, indentBlock(system.JournalTail(c.Context, c.Runner, unit, 25)))
	}
	logx.Change("started isolated PHP-FPM service for %s", slug)
	return nil
}

// StopFPMService stops, disables and removes one site's instance.
func (c *Ctx) StopFPMService(slug string) error {
	unit := fpmUnitFor(slug)
	if !c.Writer.DryRun {
		c.Runner.TryRun(c.Context, "systemctl", "disable", "--now", unit)
	}
	for _, p := range []string{
		c.fpmConfigPath(slug),
		c.fpmPoolPath(slug),
		filepath.Join("/etc/systemd/system", unit+".d"),
	} {
		if err := c.removeConfigPath(p); err != nil {
			logx.Warn("removing %s: %v", p, err)
		}
	}
	if !c.Writer.DryRun {
		_ = system.DaemonReload(c.Context, c.Runner)
	}
	return nil
}

// DisableSharedFPM stops the distribution's single php-fpm service.
//
// Every site now has its own isolated instance, so the shared one has no
// pools left to run — and leaving it enabled would be actively harmful: a
// PHP package upgrade that restores the packaged www pool would start an
// unjailed worker running as www-data with read access to every site.
func (c *Ctx) DisableSharedFPM() error {
	unit := "php" + c.PHPVersion + "-fpm.service"
	if !system.UnitExists(c.Context, c.Runner, unit) {
		return nil
	}
	if c.Writer.DryRun {
		logx.Change("[dry-run] would disable the shared %s in favour of per-site isolated services", unit)
		return nil
	}
	if system.IsActive(c.Context, c.Runner, unit) || system.IsEnabled(c.Context, c.Runner, unit) {
		c.Runner.TryRun(c.Context, "systemctl", "disable", "--now", unit)
		logx.Change("disabled the shared %s; each site now runs its own isolated instance", unit)
	}
	return nil
}
