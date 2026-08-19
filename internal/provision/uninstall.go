package provision

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ngxsetup/internal/logx"
	"ngxsetup/internal/system"
)

// UninstallOptions controls how much an uninstall removes. The default is
// deliberately conservative — matching every other destructive operation in
// this tool, nothing beyond ngxsetup's own configuration is touched unless
// explicitly asked for.
type UninstallOptions struct {
	// PurgeSites additionally removes every site's files, database and
	// system user. Without it, sites are disconnected from nginx/PHP but
	// their data is left exactly where it was.
	PurgeSites bool
	// PurgePackages additionally removes nginx, PHP and the database server
	// themselves — exactly the packages `setup` installs, so this is the
	// precise inverse of that step. Implies every site stops serving
	// regardless of PurgeSites, since the software to serve them is gone.
	PurgePackages bool
}

// UninstallPlan is what a run would do, computed up front and with no side
// effects so the CLI's confirmation prompt — and this file's tests — can see
// the real, specific list rather than a vague description of "some files."
type UninstallPlan struct {
	// ConfigPaths are ngxsetup-managed files and directories removed in
	// every uninstall, regardless of options.
	ConfigPaths []string
	// RestoredFiles are files ngxsetup overwrote or deleted from a package
	// (nginx.conf, the shared PHP-FPM www pool) — distinct from
	// PurgePackages, which removes packages outright. Restoring them is not
	// as simple as `apt-get install --reinstall`: confirmed live that dpkg
	// does not reliably re-extract a conffile on a same-version reinstall,
	// even when the file is genuinely missing and even with
	// --force-confmiss (which only ever addresses a missing conffile, not
	// one that merely has different content than dpkg's own record of it —
	// and in practice did not fire reliably either way in this environment).
	// The one method that behaved deterministically is extracting the file
	// directly out of the package archive, bypassing dpkg's conffile
	// negotiation entirely.
	RestoredFiles []RestoredFile
	// SitesDisconnected lose their nginx vhost and PHP-FPM pool in every
	// uninstall; their files, database and system user are untouched unless
	// PurgeSites is also set.
	SitesDisconnected []string
	// SitesPurged is SitesDisconnected's data as well — only when
	// PurgeSites is set.
	SitesPurged []string
	// PackagesRemoved is populated only when PurgePackages is set.
	PackagesRemoved []string
}

// RestoredFile is one packaged file to restore: PathInPackage is relative to
// the package archive root (as `dpkg-deb --fsys-tarfile` lists it, e.g.
// "./etc/nginx/nginx.conf"), LivePath is where it belongs on the running
// system.
type RestoredFile struct {
	Package       string
	PathInPackage string
	LivePath      string
}

// PlanUninstall computes what Uninstall would do without touching anything.
func (c *Ctx) PlanUninstall(opts UninstallOptions) UninstallPlan {
	var plan UninstallPlan

	plan.ConfigPaths = []string{
		filepath.Join(NginxConfD, "00-ngxsetup-core.conf"),
		filepath.Join(NginxConfD, "10-ngxsetup-compression.conf"),
		filepath.Join(NginxConfD, "11-ngxsetup-brotli.conf"),
		filepath.Join(NginxConfD, "20-ngxsetup-ssl.conf"),
		filepath.Join(NginxConfD, "30-ngxsetup-cache.conf"),
		filepath.Join(NginxConfD, "40-ngxsetup-limits.conf"),
		filepath.Join(NginxConfD, "50-ngxsetup-cloudflare.conf"),
		SnippetDir,
		"/etc/sysctl.d/60-ngxsetup.conf",
		"/etc/security/limits.d/60-ngxsetup.conf",
		"/etc/systemd/system/nginx.service.d/ngxsetup.conf",
		"/etc/systemd/system/" + c.FPMUnit + ".d/ngxsetup.conf",
		"/etc/systemd/system/" + c.DBUnit + ".d/ngxsetup.conf",
		c.Plan.DB.ConfigPath,
		"/etc/fail2ban/jail.d/ngxsetup.local",
		"/etc/fail2ban/filter.d/ngxsetup-wp-login.conf",
		"/etc/fail2ban/filter.d/ngxsetup-xmlrpc.conf",
		"/etc/fail2ban/filter.d/ngxsetup-scanner.conf",
		"/etc/apt/apt.conf.d/51ngxsetup-unattended",
		"/etc/logrotate.d/ngxsetup",
		"/etc/nginx/ngxsetup-phpmyadmin.htpasswd",
		filepath.Join(SitesAvailable, "phpmyadmin.conf"),
		filepath.Join(SitesEnabled, "phpmyadmin.conf"),
		pmaDir,
		fmt.Sprintf("/etc/php/%s/fpm/pool.d/%s.conf", c.PHPVersion, pmaSlug),
		fmt.Sprintf("/etc/php/%s/fpm/conf.d/99-ngxsetup.ini", c.PHPVersion),
		fmt.Sprintf("/etc/php/%s/cli/conf.d/99-ngxsetup.ini", c.PHPVersion),
		FPMConfigDir,
		FPMUnitTemplate,
		"/etc/tmpfiles.d/ngxsetup.conf",
		"/usr/local/bin/ngxsetup", "/usr/local/bin/vhostsetup", "/usr/local/bin/fixperm",
		"/usr/local/bin/mysqltune", "/usr/local/bin/loadcheck",
		ConfigDir, StateDir,
	}

	// nginx.conf and the shared PHP-FPM www pool were files ngxsetup
	// *overwrote* or *deleted*, not new files it added.
	plan.RestoredFiles = []RestoredFile{
		{Package: nginxConfigPackage(c), PathInPackage: "./etc/nginx/nginx.conf", LivePath: "/etc/nginx/nginx.conf"},
		{
			Package:       fmt.Sprintf("php%s-fpm", c.PHPVersion),
			PathInPackage: fmt.Sprintf("./etc/php/%s/fpm/pool.d/www.conf", c.PHPVersion),
			LivePath:      fmt.Sprintf("/etc/php/%s/fpm/pool.d/www.conf", c.PHPVersion),
		},
	}

	for _, s := range c.State.Sites {
		if opts.PurgeSites {
			plan.SitesPurged = append(plan.SitesPurged, s.Domain)
		} else {
			plan.SitesDisconnected = append(plan.SitesDisconnected, s.Domain)
		}
	}

	if opts.PurgePackages {
		plan.PackagesRemoved = append([]string{}, basePackages...)
		plan.PackagesRemoved = append(plan.PackagesRemoved, phpPackages...)
		plan.PackagesRemoved = append(plan.PackagesRemoved,
			"nginx", "mariadb-server", "mysql-server",
			"certbot", "fail2ban", "ufw", "unattended-upgrades")
	}

	return plan
}

// Describe renders the plan as confirmation-prompt lines.
func (p UninstallPlan) Describe() []string {
	var lines []string
	lines = append(lines, fmt.Sprintf("remove %d ngxsetup configuration file(s)/directorie(s)", len(p.ConfigPaths)))
	for _, f := range p.RestoredFiles {
		lines = append(lines, fmt.Sprintf("restore the packaged default of %s (from %s)", f.LivePath, f.Package))
	}
	for _, d := range p.SitesDisconnected {
		lines = append(lines, fmt.Sprintf("disconnect %s from nginx/PHP (files and database are KEPT)", d))
	}
	for _, d := range p.SitesPurged {
		lines = append(lines, fmt.Sprintf("PERMANENTLY DELETE %s: files, database and system user", d))
	}
	if len(p.PackagesRemoved) > 0 {
		lines = append(lines, fmt.Sprintf("remove %d package(s): %s", len(p.PackagesRemoved), strings.Join(p.PackagesRemoved, ", ")))
	}
	return lines
}

// nginxConfigPackage finds which installed package owns /etc/nginx/nginx.conf
// rather than assuming a name — Debian's nginx packaging has moved which
// package owns this file across releases, and reinstalling the wrong package
// name would silently do nothing.
func nginxConfigPackage(c *Ctx) string {
	out, err := c.Runner.Output(c.Context, "dpkg", "-S", "/etc/nginx/nginx.conf")
	if err == nil {
		if pkg, _, ok := strings.Cut(out, ":"); ok {
			return strings.TrimSpace(pkg)
		}
	}
	return "nginx-common" // the correct answer on every currently supported Debian/Ubuntu release
}

// Uninstall executes a previously reviewed plan.
//
// Order matters: sites are disconnected (or purged) before their supporting
// config is removed, packages are restored/removed last, and ngxsetup's own
// state is deleted only after everything it was tracking has actually been
// handled — so a failure partway through still leaves a state file that
// accurately describes what remains.
func (c *Ctx) Uninstall(opts UninstallOptions) error {
	plan := c.PlanUninstall(opts)
	logx.Section("Uninstalling")

	if !c.Writer.DryRun {
		if err := c.snapshotBeforeUninstall(); err != nil {
			logx.Warn("could not save a pre-uninstall snapshot: %v", err)
		}
	}

	for _, s := range append(append([]string{}, plan.SitesPurged...), plan.SitesDisconnected...) {
		purge := opts.PurgeSites
		if err := c.RemoveSite(s, purge, purge); err != nil {
			logx.Warn("removing %s: %v", s, err)
		}
	}

	for _, p := range plan.ConfigPaths {
		if err := c.removeConfigPath(p); err != nil {
			logx.Warn("removing %s: %v", p, err)
		}
	}

	if !c.Writer.DryRun {
		_ = system.DaemonReload(c.Context, c.Runner)
	}

	if opts.PurgePackages {
		if err := system.AptRemove(c.Context, c.Runner, plan.PackagesRemoved...); err != nil {
			logx.Warn("removing packages: %v", err)
		}
		system.AptAutoremove(c.Context, c.Runner)
	} else {
		// Packages stay installed, so restore what ngxsetup overwrote rather
		// than leaving nginx/php-fpm unable to start on a missing conffile.
		for _, f := range plan.RestoredFiles {
			if err := c.restorePackagedFile(f); err != nil {
				logx.Warn("restoring %s: %v", f.LivePath, err)
			}
		}
		if !c.Writer.DryRun {
			for _, unit := range []string{"nginx.service", c.FPMUnit, c.DBUnit, "fail2ban.service"} {
				if unit != "" && system.UnitExists(c.Context, c.Runner, unit) {
					_ = system.Restart(c.Context, c.Runner, unit)
				}
			}
		}
	}

	logx.Change("ngxsetup configuration removed")
	if len(plan.SitesDisconnected) > 0 && !opts.PurgeSites {
		logx.Info("site files and databases were kept — see the pre-uninstall snapshot for what was configured")
	}
	if !opts.PurgePackages {
		logx.Info("nginx, PHP and the database server remain installed, now running their packaged default configuration")
	}
	return nil
}

// restorePackagedFile brings back the exact packaged content of one file by
// downloading the package and extracting the file straight out of its
// archive, rather than asking dpkg to reconcile a conffile — confirmed live
// that `apt-get install --reinstall`, even combined with --force-confmiss and
// even with the file first deleted so it was genuinely absent, did not
// reliably restore it. Extracting directly from the .deb sidesteps dpkg's
// conffile negotiation entirely and was confirmed to produce a byte-exact
// match against dpkg's own recorded checksum for the file.
func (c *Ctx) restorePackagedFile(f RestoredFile) error {
	if c.Writer.DryRun {
		logx.Change("[dry-run] would restore %s from %s", f.LivePath, f.Package)
		return nil
	}
	script := fmt.Sprintf(
		`set -e
d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
cd "$d"
apt-get download %s >/dev/null
deb=$(ls %s_*.deb 2>/dev/null | head -1)
[ -n "$deb" ]
dpkg-deb --fsys-tarfile "$deb" | tar -xO %s > %s.new
mv %s.new %s
`,
		shellQuote(f.Package), shellQuote(f.Package), shellQuote(f.PathInPackage),
		shellQuote(c.Path(f.LivePath)), shellQuote(c.Path(f.LivePath)), shellQuote(c.Path(f.LivePath)))

	if err := c.Runner.Run(c.Context, "bash", "-c", script); err != nil {
		return err
	}
	logx.Change("restored %s from the packaged default", f.LivePath)
	return nil
}

// shellQuote wraps s in single quotes for safe interpolation into a POSIX
// shell command, escaping any single quote in s itself. Every value passed
// through this in practice is internally generated (a package name from
// dpkg's own output, a path this file's own constants define) rather than
// arbitrary input, but the script this feeds is still worth defending
// properly rather than trusting that invariant to hold forever.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// removeConfigPath deletes a file or directory, tolerating either kind and a
// missing target, since not every path in the plan exists on every machine
// (e.g. the Cloudflare conf only exists if that feature was ever turned on).
func (c *Ctx) removeConfigPath(p string) error {
	full := c.Path(p)
	info, err := os.Stat(full)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if c.Writer.DryRun {
		logx.Change("[dry-run] would remove %s", p)
		return nil
	}
	if info.IsDir() {
		if err := os.RemoveAll(full); err != nil {
			return err
		}
	} else if err := os.Remove(full); err != nil {
		return err
	}
	logx.Change("removed %s", p)
	return nil
}

// snapshotBeforeUninstall preserves a copy of state.json and config.json
// outside of what is about to be deleted — a cheap safety net so "what was
// this server configured to do" survives even a full uninstall, without
// costing anything toward how clean the revert itself is.
func (c *Ctx) snapshotBeforeUninstall() error {
	dir := c.Path(filepath.Join("/root", "ngxsetup-uninstalled-"+time.Now().UTC().Format("20060102-150405")))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, src := range []string{c.State.Path(), c.Config.Path()} {
		data, err := os.ReadFile(src)
		if err != nil {
			continue // e.g. config.json was never written because every setting stayed default
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(src)), data, 0o600); err != nil {
			return err
		}
	}
	logx.Info("saved a copy of the previous configuration to %s", dir)
	return nil
}
