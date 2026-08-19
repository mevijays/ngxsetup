package provision

import (
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ngxsetup/internal/facts"
	"ngxsetup/internal/logx"
	"ngxsetup/internal/system"
	"ngxsetup/internal/tmpl"
	"ngxsetup/internal/tuning"
)

// SetupRequest configures the initial provisioning run.
type SetupRequest struct {
	// Database selects the server to install. Empty means MariaDB, which is
	// the default on Debian-family systems and the better fit for WordPress.
	Database string
	// SkipPackages assumes the stack is already installed, for re-running the
	// configuration side of setup on a working machine.
	SkipPackages bool
	// InstallRedis adds a Redis server for the WordPress object cache.
	InstallRedis bool
}

// basePackages are needed on every machine.
var basePackages = []string{
	"ca-certificates", "curl", "unzip", "cron", "logrotate",
	// libfcgi-bin provides cgi-fcgi, which stats.QueryFPMStatus uses to read
	// each site's PHP-FPM pool status (listen queue, max children reached,
	// slow requests) directly over its unix socket for the web UI's Live
	// Stats page — a base dependency, not an opt-in one like borg, since
	// Live Stats is always part of the UI rather than a feature an operator
	// turns on.
	"libfcgi-bin",
	// yara is a small, fast, no-daemon package — installing it costs a
	// couple of seconds and a few megabytes, so it goes in unconditionally
	// like the rest of this list. ClamAV does not: its database download
	// alone can take minutes and its daemon holds several hundred MB of
	// resident memory, real costs an operator who never runs a security
	// scan should not pay just because `setup` ran — see
	// Ctx.InstallClamAV for that one-click, opt-in install instead.
	"yara",
}

// phpPackages are the extensions WordPress and its ecosystem actually require.
// The previous list installed php-cgi and php-json, which are respectively
// unnecessary alongside FPM and built into PHP 8, and omitted intl, zip and
// bcmath, which many plugins need.
var phpPackages = []string{
	"php-fpm", "php-mysql", "php-xml", "php-curl", "php-gd", "php-mbstring",
	"php-zip", "php-intl", "php-bcmath", "php-apcu", "php-opcache", "php-imagick",
}

// Setup brings a bare machine up to a working, tuned, hardened stack.
func (c *Ctx) Setup(req SetupRequest) error {
	if err := c.preflight(); err != nil {
		return err
	}

	if !req.SkipPackages {
		if err := c.installPackages(req); err != nil {
			return err
		}
		// Versions and paths are only knowable once the packages exist, and
		// every subsequent decision depends on them.
		if err := c.Refresh(); err != nil {
			return err
		}
	}
	if c.PHPVersion == "" {
		return fmt.Errorf("PHP-FPM was installed but no version could be detected; is php on PATH?")
	}
	if c.Facts.DBFlavor == facts.DBNone {
		return fmt.Errorf("no database server was detected after installation")
	}

	c.bootstrapOwnership()

	logx.Section("Machine profile")
	logx.KV(c.Facts.Describe())
	logx.Section("Tuning plan")
	logx.KV(c.Plan.Summary())
	for _, w := range c.Plan.Warnings {
		logx.Warn("%s", w)
	}

	if err := c.Transaction("Applying nginx configuration", c.ApplyNginx, c.ValidateNginx); err != nil {
		return err
	}
	if err := c.applyBrotliIfAvailable(); err != nil {
		return err
	}
	if err := c.Transaction("Applying PHP configuration", c.ApplyPHP, c.ValidatePHP); err != nil {
		return err
	}
	if err := c.Transaction("Applying database configuration", c.ApplyDB, c.ValidateDB); err != nil {
		return err
	}
	if err := c.Transaction("Applying kernel and service limits", c.ApplySystem, nil); err != nil {
		return err
	}
	if err := c.Transaction("Hardening", c.ApplySecurity, nil); err != nil {
		return err
	}

	if err := c.installWPCLI(); err != nil {
		logx.Warn("wp-cli could not be installed: %v", err)
	}
	if err := c.installSelf(); err != nil {
		logx.Warn("could not install ngxsetup to /usr/local/bin: %v", err)
	}

	if !c.Writer.DryRun {
		if err := c.hardenDatabase(); err != nil {
			logx.Warn("database hardening incomplete: %v", err)
		}
		// c.FPMUnit — the distribution's shared php-fpm service — is
		// deliberately absent. ApplyPHP disables it, because each site now
		// runs its own isolated instance and the shared one owns no pools;
		// enabling it here would both undo that and fail outright, since FPM
		// refuses to start with zero pools configured.
		for _, unit := range []string{"nginx.service", c.DBUnit, "fail2ban.service"} {
			if unit == "" || !system.UnitExists(c.Context, c.Runner, unit) {
				continue
			}
			if err := system.EnableNow(c.Context, c.Runner, unit); err != nil {
				logx.Warn("could not enable %s: %v", unit, err)
			}
		}
		if err := system.Reload(c.Context, c.Runner, "nginx.service"); err != nil {
			return err
		}

		c.State.SetupCompleted = true
		c.State.PHPVersion = c.PHPVersion
		c.State.DBFlavor = string(c.Facts.DBFlavor)
		c.State.Profile = string(c.Plan.Profile)
		c.State.Touch()
		if err := c.State.Save(); err != nil {
			return err
		}
		if err := c.Config.Save(); err != nil {
			logx.Warn("could not write %s: %v", c.Config.Path(), err)
		}
	}
	return nil
}

// bootstrapOwnership lets the very first setup run take ownership of whatever
// the distribution packages just dropped in place — a stock nginx.conf, a
// stock mysqld.cnf — none of which carry the ngxsetup marker the managed-file
// check looks for. There is nothing to protect there: nothing on a freshly
// provisioned machine can be a human's hand-tuned config until a human has had
// the chance to touch it after setup completes once.
//
// Later runs (a second `setup`, or `tune`) go back to requiring --force, so an
// edit made after that first run is still protected.
func (c *Ctx) bootstrapOwnership() {
	if c.State.SetupCompleted || c.Writer.Force {
		return
	}
	c.Writer.Force = true
	logx.Info("first-time setup: taking ownership of the distribution's default configuration files")
}

// preflight refuses to start on a machine this tool cannot provision, rather
// than failing halfway through with a confusing error.
func (c *Ctx) preflight() error {
	if err := system.RequireRoot(); err != nil {
		return err
	}
	if !c.Facts.OS.DebianFamily() {
		return fmt.Errorf("this tool provisions Debian and Ubuntu systems; detected %q",
			orUnknown(c.Facts.OS.PrettyName))
	}
	if c.Facts.MemTotalMB > 0 && c.Facts.MemTotalMB < 512 {
		return fmt.Errorf("%d MB of memory is not enough to run nginx, PHP-FPM and a database; 1 GB is a realistic minimum",
			c.Facts.MemTotalMB)
	}
	if c.Facts.Storage.FreeMB > 0 && c.Facts.Storage.FreeMB < 2048 {
		return fmt.Errorf("only %d MB free on %s; at least 2 GB is needed for packages, WordPress and the cache",
			c.Facts.Storage.FreeMB, c.Facts.Storage.Path)
	}
	if system.HoldingLock(c.Context, c.Runner) {
		return fmt.Errorf("another package manager is running (unattended-upgrades, apt or dpkg); wait for it to finish and try again")
	}
	return nil
}

func (c *Ctx) installPackages(req SetupRequest) error {
	logx.Section("Installing packages")

	if err := system.AptUpdate(c.Context, c.Runner); err != nil {
		return err
	}
	if err := system.AptInstall(c.Context, c.Runner, basePackages...); err != nil {
		return err
	}

	dbPackage := "mariadb-server"
	if strings.EqualFold(req.Database, "mysql") {
		dbPackage = "mysql-server"
	}
	if err := system.AptInstall(c.Context, c.Runner, dbPackage); err != nil {
		return err
	}
	if err := system.AptInstall(c.Context, c.Runner, "nginx"); err != nil {
		return err
	}
	if err := system.AptInstall(c.Context, c.Runner, phpPackages...); err != nil {
		return err
	}
	if err := system.AptInstall(c.Context, c.Runner, "certbot", "fail2ban", "ufw", "unattended-upgrades"); err != nil {
		return err
	}

	// Optional: absence is a warning, not a failed provision.
	system.AptInstallOptional(c.Context, c.Runner, "libnginx-mod-http-brotli")
	if req.InstallRedis {
		if pkgs := system.AptInstallOptional(c.Context, c.Runner, "redis-server", "php-redis"); len(pkgs) == 2 {
			logx.Change("installed Redis for the WordPress object cache")
		}
	}

	// Apache is installed by some images and binds port 80, which stops nginx
	// from starting with an error that does not mention Apache at all.
	if system.PackageInstalled(c.Context, c.Runner, "apache2") {
		logx.Step("removing apache2, which would compete for port 80")
		if err := system.AptRemove(c.Context, c.Runner, "apache2"); err != nil {
			logx.Warn("could not remove apache2: %v", err)
		}
	}
	system.AptAutoremove(c.Context, c.Runner)
	return nil
}

// applyBrotliIfAvailable writes the brotli configuration only when the module
// is actually loadable. An unconditional block would prevent nginx from
// starting on a machine where the optional package is unavailable.
func (c *Ctx) applyBrotliIfAvailable() error {
	dest := filepath.Join(NginxConfD, "11-ngxsetup-brotli.conf")
	if !c.brotliAvailable() {
		logx.Skip("brotli module not present; gzip only")
		return c.Writer.Remove(dest)
	}
	body, err := tmpl.Render("nginx/conf.d/11-brotli.conf.tmpl", tmpl.Global{Plan: c.Plan})
	if err != nil {
		return err
	}
	if _, err := c.Writer.Write(dest, body, 0o644, false); err != nil {
		return err
	}
	if err := c.ValidateNginx(); err != nil {
		// The module exists but nginx will not accept the directives; drop the
		// file rather than leaving the server unable to start.
		logx.Warn("brotli configuration rejected by nginx, disabling it: %v", err)
		return c.Writer.Remove(dest)
	}
	return nil
}

func (c *Ctx) brotliAvailable() bool {
	for _, p := range []string{
		"/etc/nginx/modules-enabled/50-mod-http-brotli-filter.conf",
		"/etc/nginx/modules-enabled/50-mod-http-brotli-static.conf",
		"/usr/lib/nginx/modules/ngx_http_brotli_filter_module.so",
	} {
		if _, err := os.Stat(c.Path(p)); err == nil {
			return true
		}
	}
	return false
}

// Refresh re-detects software and recomputes the plan. Called after installing
// packages, when versions and config paths first become knowable.
func (c *Ctx) Refresh() error {
	c.Facts.DetectSoftware(c.Context, c.Runner)
	c.PHPVersion = c.Facts.PHPVersion
	c.FPMUnit = fpmUnit(c.Facts.PHPVersion)
	c.DBUnit = dbUnit(c.Facts.DBFlavor)

	prof, err := tuning.ParseProfile(c.Config.Profile)
	if err != nil {
		return err
	}
	c.Plan = tuning.Compute(c.Facts, tuning.Options{
		Profile:           prof,
		Sites:             c.State.Count(),
		AvgPHPWorkerMB:    c.Config.AvgPHPWorkerMB,
		ReserveMB:         c.Config.ReserveMB,
		UploadMaxMB:       c.Config.UploadMaxMB,
		EnableBinlog:      c.Config.EnableBinlog,
		AggressiveOpcache: c.Config.AggressiveOpcache,
	})
	return nil
}

// wpCLIVersion is pinned so that provisioning is reproducible and so the
// download can be checked against a published digest.
const wpCLIVersion = "2.11.0"

// installWPCLI fetches wp-cli and verifies it against the digest published
// beside it.
//
// This checks integrity, not provenance: it detects a corrupted download or a
// tampering mirror, but not a compromise of the release itself. The previous
// implementation fetched an unversioned phar over HTTPS and executed it with no
// check at all.
func (c *Ctx) installWPCLI() error {
	dest := "/usr/local/bin/wp"
	if c.Writer.DryRun {
		logx.Change("[dry-run] would install wp-cli %s", wpCLIVersion)
		return nil
	}
	if out, err := c.Runner.Output(c.Context, "wp", "--version", "--allow-root"); err == nil &&
		strings.Contains(out, wpCLIVersion) {
		logx.Skip("wp-cli %s already installed", wpCLIVersion)
		return nil
	}

	base := fmt.Sprintf("https://github.com/wp-cli/wp-cli/releases/download/v%s/wp-cli-%s.phar",
		wpCLIVersion, wpCLIVersion)
	tmpPath := filepath.Join(os.TempDir(), "wp-cli.phar")
	defer os.Remove(tmpPath)

	logx.Step("downloading wp-cli %s", wpCLIVersion)
	if err := download(base, tmpPath, 0o755, 32<<20); err != nil {
		return err
	}
	want, err := httpGet(base + ".sha512")
	if err != nil {
		return fmt.Errorf("fetching the published digest: %w", err)
	}
	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return err
	}
	sum := sha512.Sum512(data)
	got := hex.EncodeToString(sum[:])
	if !strings.Contains(strings.ToLower(want), got) {
		return fmt.Errorf("wp-cli digest mismatch; refusing to install (expected %s, got %s)",
			strings.TrimSpace(want), got)
	}

	if err := os.WriteFile(c.Path(dest), data, 0o755); err != nil {
		return err
	}
	logx.Change("installed wp-cli %s (digest verified)", wpCLIVersion)
	return nil
}

// installSelf copies the running binary to a stable location and creates the
// compatibility aliases the previous shell tooling exposed.
func (c *Ctx) installSelf() error {
	target := "/usr/local/bin/ngxsetup"
	if c.Writer.DryRun {
		logx.Change("[dry-run] would install ngxsetup to %s with legacy command aliases", target)
		return nil
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, _ = filepath.EvalSymlinks(self)
	if self == c.Path(target) {
		logx.Skip("ngxsetup already installed at %s", target)
	} else {
		data, err := os.ReadFile(self)
		if err != nil {
			return err
		}
		// Written through a temporary file and renamed: overwriting a running
		// executable in place produces ETXTBSY.
		tmpPath := c.Path(target) + ".new"
		if err := os.WriteFile(tmpPath, data, 0o755); err != nil {
			return err
		}
		if err := os.Rename(tmpPath, c.Path(target)); err != nil {
			return err
		}
		logx.Change("installed ngxsetup to %s", target)
	}

	// The old stack exposed these as separate commands and operators have them
	// in muscle memory and in scripts; each is a symlink that dispatches on
	// argv[0].
	for _, alias := range []string{"vhostsetup", "fixperm", "loadcheck", "mysqltune"} {
		link := c.Path(filepath.Join("/usr/local/bin", alias))
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			logx.Warn("could not create the %s alias: %v", alias, err)
		}
	}
	return nil
}

func (c *Ctx) hardenDatabase() error {
	client := c.dbClient()
	if err := client.Ping(c.Context); err != nil {
		return err
	}
	if err := client.Harden(c.Context); err != nil {
		return err
	}
	remote, err := client.RemoteRootAccounts(c.Context)
	if err == nil && len(remote) > 0 {
		// Reported rather than removed: an operator may depend on it, and
		// silently revoking their access is its own kind of outage.
		logx.Warn("database root is reachable from outside this machine (%s); remove these accounts unless you need them",
			strings.Join(remote, ", "))
	}
	return nil
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
