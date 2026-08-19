package provision

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ngxsetup/internal/facts"
	"ngxsetup/internal/logx"
	"ngxsetup/internal/system"
	"ngxsetup/internal/tmpl"
)

// ApplyNginx writes the server-wide nginx configuration.
func (c *Ctx) ApplyNginx() error {
	global := tmpl.Global{
		Plan:            c.Plan,
		ACMERoot:        ACMERoot,
		Resolvers:       c.Resolvers(),
		TrustedNetworks: c.Config.TrustedNetworks,
		RejectHandshake: c.SupportsRejectHandshake(),
		CacheVaryDevice: c.Config.CacheVaryDevice,
	}

	for _, d := range []string{SnippetDir, SitesAvailable, SitesEnabled, CacheDir, ACMERoot} {
		if err := c.Writer.EnsureDir(d, 0o755, ""); err != nil {
			return err
		}
	}
	// The cache directory is written to by nginx workers, which run as
	// www-data rather than as the master process's root.
	if !c.Writer.DryRun {
		_ = c.Writer.EnsureDir(CacheDir, 0o700, "www-data")
	}

	// The distribution ships its own default site, which would otherwise
	// compete with ours for default_server and answer for unknown hosts.
	if err := c.Writer.Remove(filepath.Join(SitesEnabled, "default")); err != nil {
		return err
	}

	files := []struct {
		template string
		dest     string
		data     any
		managed  bool
	}{
		{"nginx/nginx.conf.tmpl", "/etc/nginx/nginx.conf", global, true},
		{"nginx/conf.d/00-core.conf.tmpl", filepath.Join(NginxConfD, "00-ngxsetup-core.conf"), global, false},
		{"nginx/conf.d/10-compression.conf.tmpl", filepath.Join(NginxConfD, "10-ngxsetup-compression.conf"), global, false},
		{"nginx/conf.d/20-ssl.conf.tmpl", filepath.Join(NginxConfD, "20-ngxsetup-ssl.conf"), global, false},
		{"nginx/conf.d/30-cache.conf.tmpl", filepath.Join(NginxConfD, "30-ngxsetup-cache.conf"), global, false},
		{"nginx/conf.d/40-limits.conf.tmpl", filepath.Join(NginxConfD, "40-ngxsetup-limits.conf"), global, false},
		{"nginx/snippets/hardening.conf.tmpl", filepath.Join(SnippetDir, "hardening.conf"), tmpl.Headers{}, false},
		{"nginx/snippets/fastcgi-php.conf.tmpl", filepath.Join(SnippetDir, "fastcgi-php.conf"), tmpl.Headers{}, false},
		// Two header variants rather than one per site: HSTS must only be sent
		// by a site with a real certificate, and nginx snippets cannot branch.
		{"nginx/snippets/security-headers.conf.tmpl", filepath.Join(SnippetDir, "security-headers.conf"),
			tmpl.Headers{HSTS: false}, false},
		{"nginx/snippets/security-headers.conf.tmpl", filepath.Join(SnippetDir, "security-headers-hsts.conf"),
			tmpl.Headers{HSTS: true}, false},
	}

	for _, f := range files {
		body, err := tmpl.Render(f.template, f.data)
		if err != nil {
			return err
		}
		if _, err := c.Writer.Write(f.dest, body, 0o644, f.managed); err != nil {
			return err
		}
	}

	if c.Config.TrustCloudflare {
		if err := c.applyCloudflareRealIP(); err != nil {
			logx.Warn("Cloudflare address ranges could not be refreshed: %v", err)
		}
	}

	// Re-render every existing site's server block against the current
	// config, not just the host-wide files above.
	//
	// writeSiteConfigs was previously only ever called from AddSite and
	// UpgradeToTLS — which meant a policy toggle that lives in config.json
	// (block_xmlrpc, block_bad_agents, block_bad_referrers,
	// block_scraper_bots, admin_allow_list) took effect for every *new* site
	// but silently never reached a site that already existed when the
	// setting changed, no matter how many times `tune --apply` or `secure
	// --apply` ran afterward. Confirmed live: `config set block_scraper_bots
	// true` followed by `tune --apply` left an existing site's rendered
	// .conf with no `$ngx_scraper_bot` reference at all, while a brand-new
	// site created after the same config change got it correctly. Looping
	// over every registered site here is what makes ApplyNginx live up to
	// its name — "the server-wide nginx configuration" has always included
	// the sites, they just were not being re-applied.
	for _, site := range c.State.Sites {
		if err := c.writeSiteConfigs(site); err != nil {
			return fmt.Errorf("re-applying configuration for %s: %w", site.Domain, err)
		}
	}
	return nil
}

// ValidateNginx runs nginx's own configuration test.
func (c *Ctx) ValidateNginx() error {
	if !c.Runner.Look("nginx") {
		return nil
	}
	out, err := c.Runner.Output(c.Context, "nginx", "-t")
	if err != nil {
		return fmt.Errorf("nginx rejected the configuration:\n%s", indentBlock(out+err.Error()))
	}
	return nil
}

// ApplyPHP writes the php.ini drop-ins for both SAPIs.
func (c *Ctx) ApplyPHP() error {
	if c.PHPVersion == "" {
		return fmt.Errorf("PHP is not installed; run `ngxsetup setup` first")
	}
	for _, d := range []string{PHPLogDir, OpcacheDir} {
		if err := c.Writer.EnsureDir(d, 0o755, "www-data"); err != nil {
			return err
		}
	}

	for _, sapi := range []string{"fpm", "cli"} {
		body, err := tmpl.Render("php/php.ini.tmpl", tmpl.PHPIni{
			Plan:             c.Plan,
			SAPI:             sapi,
			Timezone:         c.timezone(),
			OpcacheFileCache: OpcacheDir,
		})
		if err != nil {
			return err
		}
		dest := fmt.Sprintf("/etc/php/%s/%s/conf.d/99-ngxsetup.ini", c.PHPVersion, sapi)
		if _, err := c.Writer.Write(dest, body, 0o644, false); err != nil {
			return err
		}
	}

	// The packaged www pool listens on a socket owned by www-data and runs as
	// www-data. Leaving it in place would undo per-site isolation: any site
	// could be pointed at it and would then run with access to every other
	// site's files.
	def := fmt.Sprintf("/etc/php/%s/fpm/pool.d/www.conf", c.PHPVersion)
	if _, err := os.Stat(c.Path(def)); err == nil {
		if err := c.Writer.Remove(def); err != nil {
			return err
		}
		logx.Change("removed the shared www PHP-FPM pool in favour of per-site pools")
	}

	// Older builds also wrote each site's pool straight into the
	// distribution's pool.d, alongside the isolated one this tool actually
	// runs from FPMPoolDir — a leftover from before per-site isolation
	// existed, fixed once writeSiteConfigs stopped doing it. A stray copy
	// there is inert as long as the shared service above stays disabled,
	// but it is exactly the file a re-enabled shared service would pick up
	// and run outside the site's jail, so an install upgrading past that
	// bug gets every site's stray copy swept up here rather than needing
	// each one touched by hand.
	for _, s := range c.State.Sites {
		legacy := fmt.Sprintf("/etc/php/%s/fpm/pool.d/%s.conf", c.PHPVersion, s.Slug)
		if _, err := os.Stat(c.Path(legacy)); err == nil {
			if err := c.Writer.Remove(legacy); err != nil {
				return err
			}
			logx.Change("removed a stray legacy PHP-FPM pool for %s from the distribution's pool.d", s.Domain)
		}
	}

	// No placeholder pool is needed here any more. That hack existed because
	// the single shared FPM service refuses to start with zero pools, which
	// was the state a fresh machine landed in once the packaged www pool was
	// removed. Each site now runs its own service with exactly one pool, so
	// "zero pools" is no longer a state that can occur — the service simply
	// does not exist until a site does.
	if err := c.DisableSharedFPM(); err != nil {
		return err
	}
	return c.ApplyFPMServiceTemplate()
}

// ValidatePHP runs PHP-FPM's configuration test against every site's own
// config.
//
// Testing the distribution's shared php-fpm.conf would be meaningless now:
// that service is disabled and owns no pools. What has to be valid is each
// site's isolated master config, so each one is checked individually — and a
// failure names which site, rather than reporting a generic "php-fpm rejected
// the configuration" for a file that no longer runs anything.
func (c *Ctx) ValidatePHP() error {
	if c.PHPVersion == "" || !c.Runner.Look("php-fpm"+c.PHPVersion) {
		return nil
	}
	for _, s := range c.State.Sites {
		if _, err := os.Stat(c.Path(c.fpmConfigPath(s.Slug))); err != nil {
			continue // not yet written; AddSite validates its own before starting
		}
		if err := c.ValidateFPMService(s.Slug); err != nil {
			return err
		}
	}
	return nil
}

// ApplyDB writes the database tuning drop-in.
func (c *Ctx) ApplyDB() error {
	if c.Facts.DBFlavor == facts.DBNone {
		return fmt.Errorf("no database server detected; run `ngxsetup setup` first")
	}

	// Confirmed live: Ubuntu 24.04's mariadb-server package does not create
	// /var/log/mysql/ at all — it defaults to journald, not file logging —
	// so a config that turns on slow_query_log without also creating this
	// directory starts the server successfully but silently disables slow
	// query logging entirely ("Turning logging off for the whole duration of
	// the MariaDB server process"), defeating the point of enabling it.
	if err := c.Writer.EnsureDir(DBLogDir, 0o750, "mysql:mysql"); err != nil {
		return err
	}

	body, err := tmpl.Render("db/server.cnf.tmpl", tmpl.DB{Plan: c.Plan})
	if err != nil {
		return err
	}
	_, err = c.Writer.Write(c.Plan.DB.ConfigPath, body, 0o644, false)
	return err
}

// ValidateDB restarts the database and confirms it came back.
//
// Neither MariaDB nor every MySQL build offers an offline config validator, so
// the only honest test is to start the server. A failure here triggers the
// same rollback as any other, and the journal tail is included because
// "job failed" on its own tells an operator nothing.
//
// The restart is skipped when ApplyDB wrote nothing and the service is
// already up. Unlike nginx -t or php-fpm -t — cheap syntax checks safe to run
// on every apply — "validating" the database means actually bouncing it,
// which drops connections and pauses queries. Paying that cost on every
// idempotent re-run (confirmed live: re-running `setup` after only a site
// count or disk-free change, with an unchanged database config, still
// restarted MariaDB every time) turns "safe to run twice" into "bounces
// production on every run." c.Writer.Changed() reflects only this
// transaction's writes — the previous transaction's Commit cleared the
// journal — so it accurately answers "did ApplyDB just produce a new file?"
func (c *Ctx) ValidateDB() error {
	if !system.UnitExists(c.Context, c.Runner, c.DBUnit) {
		return nil
	}
	if c.Writer.Changed() == 0 && system.IsActive(c.Context, c.Runner, c.DBUnit) {
		logx.Skip("database configuration unchanged; leaving %s running", c.DBUnit)
		return nil
	}
	if err := system.Restart(c.Context, c.Runner, c.DBUnit); err != nil {
		return fmt.Errorf("%w\n%s", err, indentBlock(system.JournalTail(c.Context, c.Runner, c.DBUnit, 25)))
	}
	// systemd reports the unit active as soon as the process starts; InnoDB
	// can still abort a moment later while recovering or sizing the pool.
	for i := 0; i < 15; i++ {
		time.Sleep(time.Second)
		if !system.IsActive(c.Context, c.Runner, c.DBUnit) {
			return fmt.Errorf("the database stopped shortly after starting:\n%s",
				indentBlock(system.JournalTail(c.Context, c.Runner, c.DBUnit, 25)))
		}
	}
	return nil
}

// ApplySystem writes kernel tuning, descriptor limits and unit drop-ins.
func (c *Ctx) ApplySystem() error {
	sysctlBody, err := tmpl.Render("system/sysctl.conf.tmpl", tmpl.System{Plan: c.Plan})
	if err != nil {
		return err
	}
	if _, err := c.Writer.Write("/etc/sysctl.d/60-ngxsetup.conf", sysctlBody, 0o644, false); err != nil {
		return err
	}

	limitsBody, err := tmpl.Render("system/limits.conf.tmpl", tmpl.System{Plan: c.Plan})
	if err != nil {
		return err
	}
	if _, err := c.Writer.Write("/etc/security/limits.d/60-ngxsetup.conf", limitsBody, 0o644, false); err != nil {
		return err
	}

	// systemd ignores /etc/security/limits.d entirely, so services need their
	// own drop-ins or the descriptor limits above have no effect where it
	// matters most.
	units := []tmpl.Unit{
		{Unit: "nginx.service", Nofile: c.Plan.Limits.NginxNofile, Restart: true, OOMAdjust: "-100"},
		{Unit: c.FPMUnit, Nofile: c.Plan.Limits.PHPNofile, Restart: true},
		// The database is the most expensive process to lose and the slowest
		// to recover, so it is made the least attractive OOM target.
		{Unit: c.DBUnit, Nofile: c.Plan.Limits.DBNofile, Restart: true, OOMAdjust: "-500"},
	}
	for _, u := range units {
		if u.Unit == "" || !system.UnitExists(c.Context, c.Runner, u.Unit) {
			continue
		}
		body, err := tmpl.Render("system/unit-override.conf.tmpl", u)
		if err != nil {
			return err
		}
		dest := filepath.Join("/etc/systemd/system", u.Unit+".d", "ngxsetup.conf")
		if _, err := c.Writer.Write(dest, body, 0o644, false); err != nil {
			return err
		}
	}

	logBody, err := tmpl.Render("system/logrotate.tmpl", tmpl.Logrotate{
		KeepDays:   orInt(c.Config.LogKeepDays, 14),
		PHPVersion: c.PHPVersion,
	})
	if err != nil {
		return err
	}
	if _, err := c.Writer.Write("/etc/logrotate.d/ngxsetup", logBody, 0o644, false); err != nil {
		return err
	}

	if c.Writer.DryRun {
		return nil
	}

	// Applying sysctl is allowed to fail: an unprivileged container cannot set
	// most network parameters, and that is a warning rather than an error.
	if out, err := c.Runner.Output(c.Context, "sysctl", "-p", "/etc/sysctl.d/60-ngxsetup.conf"); err != nil {
		logx.Warn("some kernel parameters could not be applied (normal inside a container): %v", err)
		logx.Debug("%s", out)
	} else {
		logx.Change("applied kernel tuning")
	}
	return system.DaemonReload(c.Context, c.Runner)
}

// applyCloudflareRealIP rewrites the client address from Cloudflare's header,
// but only for connections arriving from Cloudflare's published ranges.
//
// The distinction is the whole point: `real_ip_header CF-Connecting-IP` without
// a matching set_real_ip_from list lets anyone spoof their address by sending
// the header themselves, which defeats rate limiting and fail2ban at once.
func (c *Ctx) applyCloudflareRealIP() error {
	var b strings.Builder
	b.WriteString(tmpl.ManagedHeader + "\n")
	b.WriteString("#\n# Cloudflare origin ranges. Refresh with `ngxsetup secure refresh-cloudflare`.\n#\n\n")

	total := 0
	for _, url := range []string{"https://www.cloudflare.com/ips-v4", "https://www.cloudflare.com/ips-v6"} {
		body, err := httpGet(url)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if err := validateCIDR(line); err != nil {
				return fmt.Errorf("unexpected content from %s: %w", url, err)
			}
			fmt.Fprintf(&b, "set_real_ip_from %s;\n", line)
			total++
		}
	}
	// A truncated or hijacked response must not be written: an empty list
	// combined with the header directive is exactly the spoofable state.
	if total < 10 {
		return fmt.Errorf("only %d ranges returned, refusing to write a partial trust list", total)
	}
	b.WriteString("\nreal_ip_header CF-Connecting-IP;\nreal_ip_recursive on;\n")

	_, err := c.Writer.Write(filepath.Join(NginxConfD, "50-ngxsetup-cloudflare.conf"), []byte(b.String()), 0o644, false)
	if err == nil {
		logx.Change("trusted %d Cloudflare ranges for client address rewriting", total)
	}
	return err
}

func (c *Ctx) timezone() string {
	if c.Config.Timezone != "" {
		return c.Config.Timezone
	}
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(link, "zoneinfo/"); i >= 0 {
			return link[i+len("zoneinfo/"):]
		}
	}
	return "UTC"
}

func orInt(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

func indentBlock(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "    (no output)"
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}
