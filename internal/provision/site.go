package provision

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ngxsetup/internal/db"
	"ngxsetup/internal/logx"
	"ngxsetup/internal/render"
	"ngxsetup/internal/site"
	"ngxsetup/internal/state"
	"ngxsetup/internal/system"
	"ngxsetup/internal/tmpl"
	"ngxsetup/internal/tuning"
)

// SiteRequest describes a site to create.
type SiteRequest struct {
	Domain  string
	Aliases []string

	// WordPress installs WordPress into the document root and provisions a
	// database. Without it the site is an empty, correctly configured vhost.
	WordPress bool
	// TLS requests a Let's Encrypt certificate. Requires DNS already pointing
	// at this machine.
	TLS bool
	// SelfSigned issues a local certificate instead, for staging or for a site
	// behind a CDN that terminates TLS itself.
	SelfSigned bool

	// AdminUser and friends drive an unattended WordPress installation.
	AdminUser     string
	AdminEmail    string
	InstallWP     bool
	Title         string
	DisableCache  bool
	AllowFileMods bool
}

// AddSite provisions a virtual host end to end.
//
// The order is chosen so that a failure at any step leaves as little behind as
// possible: the account and directories come first because they are cheap to
// remove, the database next, and nginx last so a half-built site is never
// reachable.
func (c *Ctx) AddSite(req SiteRequest) (*state.Site, error) {
	domain := site.NormalizeDomain(req.Domain)
	if err := site.ValidateDomain(domain); err != nil {
		return nil, err
	}
	for i, a := range req.Aliases {
		a = site.NormalizeDomain(a)
		if a == domain {
			continue
		}
		if err := site.ValidateDomain(a); err != nil {
			return nil, fmt.Errorf("alias: %w", err)
		}
		req.Aliases[i] = a
	}
	if c.State.DomainTaken(domain, "") {
		return nil, fmt.Errorf("%s is already served by an existing site (see `ngxsetup site list`)", domain)
	}
	if c.PHPVersion == "" {
		return nil, fmt.Errorf("PHP is not installed; run `ngxsetup setup` first")
	}

	slug := site.UniqueSlug(domain, c.State.SlugTaken)
	user := site.UserName(slug)
	rec := state.Site{
		Slug:         slug,
		Domain:       domain,
		Aliases:      dedupeAliases(domain, req.Aliases),
		Root:         c.DocumentRoot(slug),
		User:         user,
		SocketPath:   c.SocketPath(slug),
		PHPVersion:   c.PHPVersion,
		WordPress:    req.WordPress,
		CacheEnabled: !req.DisableCache,
		Enabled:      true,
	}

	logx.Section("Creating %s", domain)
	logx.KV([][2]string{
		{"slug", slug},
		{"document root", rec.Root},
		{"system user", user},
		{"php pool", rec.SocketPath},
	})

	if err := c.createSiteAccount(rec); err != nil {
		return nil, err
	}

	var dbPassword string
	if req.WordPress {
		suffix, err := system.Suffix(4)
		if err != nil {
			return nil, err
		}
		rec.DBName = site.DBName(slug, suffix)
		rec.DBUser = rec.DBName
		if dbPassword, err = system.Password(24); err != nil {
			return nil, err
		}
		client := c.dbClient()
		if err := client.Ping(c.Context); err != nil {
			return nil, err
		}
		if err := client.Provision(c.Context, rec.DBName, rec.DBUser, dbPassword, "localhost", c.Plan.DB.Collation); err != nil {
			return nil, err
		}
		if err := c.installWordPress(rec, dbPassword, suffix, req); err != nil {
			return nil, err
		}
	}

	// Certificates before the vhost, so the server block never references a
	// certificate file that does not exist — which nginx treats as fatal and
	// which would take every other site down with it on reload.
	if err := c.issueCertificate(&rec, req); err != nil {
		return nil, err
	}

	if err := c.writeSiteConfigs(rec); err != nil {
		return nil, err
	}
	// This site's own isolated PHP-FPM instance: its pool, its master config
	// and its memory ceiling. Written before nginx is reloaded so the socket
	// exists by the time nginx starts routing to it.
	if err := c.WriteFPMService(rec); err != nil {
		return nil, err
	}

	if err := c.ValidateNginx(); err != nil {
		return nil, err
	}
	// Validate only this site's config here. ValidatePHP checks every
	// registered site, and this one is not in state yet.
	if err := c.ValidateFPMService(rec.Slug); err != nil {
		return nil, err
	}

	if !c.Writer.DryRun {
		// Only this site's service starts. Adding a site no longer restarts
		// PHP for every other site on the box — which the shared-service
		// model forced, briefly dropping in-flight requests everywhere.
		if err := c.StartFPMService(rec.Slug); err != nil {
			return nil, err
		}
		if err := system.Reload(c.Context, c.Runner, "nginx.service"); err != nil {
			return nil, err
		}
	}

	c.State.Upsert(rec)
	if !c.Writer.DryRun {
		if err := c.State.Save(); err != nil {
			return nil, err
		}
		if req.WordPress {
			if err := c.writeCredentials(rec, dbPassword, req); err != nil {
				logx.Warn("credentials file could not be written: %v", err)
			}
		}
	}
	return &rec, nil
}

// createSiteAccount builds the user and directory tree.
//
// The permission model is what makes multi-tenancy safe here. Directories are
// 2750 owned by the site user with group www-data: the setgid bit means every
// file WordPress creates inherits the www-data group, so nginx can read it,
// while 0750 means no other site's user can traverse into the tree at all.
func (c *Ctx) createSiteAccount(rec state.Site) error {
	base := c.SiteRoot(rec.Slug)
	if err := system.EnsureSystemUser(c.Context, c.Runner, rec.User, base); err != nil {
		return err
	}

	dirs := []struct {
		path  string
		mode  os.FileMode
		owner string
	}{
		{base, 0o750, rec.User + ":www-data"},
		{rec.Root, 0o2750, rec.User + ":www-data"},
		// Not under the document root, so a path-traversal bug cannot reach
		// session files and a misconfiguration cannot serve them.
		{c.siteTmpDir(rec.Slug), 0o700, rec.User},
		{c.siteSessionDir(rec.Slug), 0o700, rec.User},
	}
	for _, d := range dirs {
		if err := c.Writer.EnsureDir(d.path, d.mode, ""); err != nil {
			return err
		}
		if err := c.chown(d.path, d.owner); err != nil {
			return err
		}
		if !c.Writer.DryRun {
			if err := os.Chmod(c.Path(d.path), d.mode); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeSiteConfigs renders the nginx server block. The PHP-FPM pool is
// WriteFPMService's job, not this one — see its doc comment for why a pool
// belongs in FPMPoolDir and nowhere near the distribution's own pool.d.
func (c *Ctx) writeSiteConfigs(rec state.Site) error {
	headers := "security-headers.conf"
	if rec.TLS {
		headers = "security-headers-hsts.conf"
	}
	overridePath := filepath.Join(SitesAvailable, rec.Slug+".custom.conf")

	siteData := tmpl.Site{
		Plan:              c.Plan,
		Slug:              rec.Slug,
		Domain:            rec.Domain,
		PrimaryName:       rec.Domain,
		ServerNames:       rec.ServerNames(),
		Root:              rec.Root,
		SocketPath:        rec.SocketPath,
		TLS:               rec.TLS,
		CertPath:          rec.CertPath,
		KeyPath:           certKeyPath(rec),
		ChainPath:         rec.ChainPath,
		OCSPStapling:      rec.CertSource == "letsencrypt" && rec.ChainPath != "",
		HTTP2Style:        c.HTTP2Style(),
		HeadersSnippet:    headers,
		OverridePath:      overridePath,
		ACMERoot:          ACMERoot,
		CacheEnabled:      rec.CacheEnabled,
		BlockXMLRPC:       c.Config.BlockXMLRPC,
		BlockBadAgents:    c.Config.BlockBadAgents,
		BlockBadReferrers: c.Config.BlockBadReferrers,
		BlockScraperBots:  c.Config.BlockScraperBots,
		AdminAllowList:    c.Config.AdminAllowList,
	}
	body, err := tmpl.Render("nginx/sites/site.conf.tmpl", siteData)
	if err != nil {
		return err
	}
	confPath := filepath.Join(SitesAvailable, rec.Slug+".conf")
	if _, err := c.Writer.Write(confPath, body, 0o644, false); err != nil {
		return err
	}

	// Created once, never rewritten: this is where an operator puts redirects
	// and custom locations that must survive a re-apply.
	override := fmt.Sprintf("# Site-specific nginx directives for %s.\n"+
		"# ngxsetup creates this file once and never modifies it.\n"+
		"# Anything here is included at the end of the server block.\n", rec.Domain)
	if _, err := c.Writer.WriteIfAbsent(overridePath, []byte(override), 0o644); err != nil {
		return err
	}

	if rec.Enabled {
		if err := c.Writer.Symlink(confPath, filepath.Join(SitesEnabled, rec.Slug+".conf")); err != nil {
			return err
		}
	}
	return nil
}

// installWordPress downloads core and writes wp-config.php.
func (c *Ctx) installWordPress(rec state.Site, dbPassword, prefixSuffix string, req SiteRequest) error {
	if c.Writer.DryRun {
		logx.Change("[dry-run] would install WordPress into %s", rec.Root)
		return nil
	}

	logx.Step("downloading WordPress")
	tarball := filepath.Join(os.TempDir(), "wordpress-"+rec.Slug+".tar.gz")
	defer os.Remove(tarball)

	if err := download("https://wordpress.org/latest.tar.gz", tarball, 0o600, 128<<20); err != nil {
		return fmt.Errorf("downloading WordPress: %w", err)
	}
	if err := extractTarGz(tarball, c.Path(rec.Root), 1); err != nil {
		return fmt.Errorf("extracting WordPress: %w", err)
	}
	logx.Change("installed WordPress into %s", rec.Root)

	salts, err := generateSalts()
	if err != nil {
		return err
	}
	scheme := "http"
	if rec.TLS {
		scheme = "https"
	}
	cfg, err := tmpl.Render("wordpress/wp-config.php.tmpl", tmpl.WPConfig{
		Domain:           rec.Domain,
		DBName:           rec.DBName,
		DBUser:           rec.DBUser,
		DBPassword:       dbPassword,
		TablePrefix:      site.TablePrefix(prefixSuffix),
		Salts:            salts,
		SiteURL:          scheme + "://" + rec.Domain,
		MemoryLimit:      tuning.MemString(c.Plan.PHP.MemoryLimitMB),
		AdminMemoryLimit: tuning.MemString(c.Plan.PHP.CLIMemoryLimitMB),
		DisableCron:      true,
		AllowFileMods:    req.AllowFileMods,
	})
	if err != nil {
		return err
	}
	// wp-config.php holds the database password, so it is readable only by the
	// site user and the nginx group — never 0644.
	wpConfig := filepath.Join(rec.Root, "wp-config.php")
	if _, err := c.Writer.WriteIfAbsent(wpConfig, cfg, 0o640); err != nil {
		return err
	}

	// WordPress ships this and it tells a scanner the exact core version.
	_ = os.Remove(filepath.Join(c.Path(rec.Root), "readme.html"))
	_ = os.Remove(filepath.Join(c.Path(rec.Root), "license.txt"))

	if err := c.chownRecursive(rec.Root, rec.User+":www-data"); err != nil {
		return err
	}
	if err := os.Chmod(c.Path(wpConfig), 0o640); err != nil {
		return err
	}

	if req.InstallWP {
		if err := c.runWPInstall(rec, req); err != nil {
			logx.Warn("automatic WordPress installation did not complete: %v", err)
			logx.Info("the site is configured; finish setup at %s://%s", scheme, rec.Domain)
		}
	}
	return nil
}

// runWPInstall completes the installation with wp-cli, as the site user.
func (c *Ctx) runWPInstall(rec state.Site, req SiteRequest) error {
	if !c.Runner.Look("wp") {
		return fmt.Errorf("wp-cli is not installed")
	}
	adminUser := req.AdminUser
	if adminUser == "" {
		adminUser = "admin-" + rec.Slug
	}
	if req.AdminEmail == "" {
		return fmt.Errorf("--admin-email is required to install WordPress unattended")
	}
	password, err := system.Password(24)
	if err != nil {
		return err
	}
	scheme := "http"
	if rec.TLS {
		scheme = "https"
	}
	title := req.Title
	if title == "" {
		title = rec.Domain
	}

	// Run as the site user so every file created belongs to the site rather
	// than to root, which would leave WordPress unable to manage its own
	// uploads directory.
	args := []string{"-u", rec.User, "--", "wp", "core", "install",
		"--path=" + c.Path(rec.Root),
		"--url=" + scheme + "://" + rec.Domain,
		"--title=" + title,
		"--admin_user=" + adminUser,
		"--admin_email=" + req.AdminEmail,
		"--skip-email",
	}
	// The password goes in on stdin via the prompt form, not as an argument,
	// so it never appears in the process list.
	if _, err := c.Runner.RunStdin(c.Context, password+"\n", "runuser",
		append(args, "--prompt=admin_password")...); err != nil {
		return err
	}
	logx.Change("installed WordPress; administrator is %s", adminUser)
	c.pendingAdmin = adminUser
	c.pendingAdminPassword = password
	return nil
}

// writeCredentials records what an operator needs, readable only by root.
//
// The previous implementation wrote this file with mode 0644, so every local
// account — including the unprivileged accounts running PHP — could read every
// site's database password.
func (c *Ctx) writeCredentials(rec state.Site, dbPassword string, req SiteRequest) error {
	if err := c.Writer.EnsureDir(CredentialsDir, 0o700, ""); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Site:            %s\n", rec.Domain)
	fmt.Fprintf(&b, "Document root:   %s\n", rec.Root)
	fmt.Fprintf(&b, "System user:     %s\n", rec.User)
	fmt.Fprintf(&b, "PHP-FPM socket:  %s\n\n", rec.SocketPath)
	fmt.Fprintf(&b, "Database:        %s\n", rec.DBName)
	fmt.Fprintf(&b, "Database user:   %s\n", rec.DBUser)
	fmt.Fprintf(&b, "Database pass:   %s\n", dbPassword)
	if c.pendingAdmin != "" {
		fmt.Fprintf(&b, "\nWordPress admin: %s\n", c.pendingAdmin)
		fmt.Fprintf(&b, "WordPress pass:  %s\n", c.pendingAdminPassword)
	}
	b.WriteString("\nThese credentials are also in wp-config.php. This file is mode 0600 and\n")
	b.WriteString("readable only by root; delete it once you have stored them elsewhere.\n")

	path := filepath.Join(CredentialsDir, rec.Slug+".txt")
	if err := os.WriteFile(c.Path(path), []byte(b.String()), 0o600); err != nil {
		return err
	}
	logx.Change("credentials written to %s (mode 0600)", path)
	return nil
}

// RemoveSite tears a site down.
//
// Nothing is deleted implicitly. Files and databases are removed only when
// asked for explicitly, because the single most damaging thing a provisioning
// tool can do is delete data the operator expected to keep.
func (c *Ctx) RemoveSite(nameOrSlug string, purgeFiles, purgeDB bool) error {
	rec, err := c.State.Find(nameOrSlug)
	if err != nil {
		return err
	}
	// Captured now, not read from rec again after State.Delete: Find
	// returns a pointer into State.Sites, and Delete removes an element by
	// shifting everything after it backward in the same backing array —
	// confirmed live, that shift left rec pointing at whatever site had
	// been immediately after this one, so a late "removed %s" reported
	// that site's domain instead of the one actually just removed. The
	// removal itself was never affected — Delete is called with rec.Slug
	// evaluated before the shift — only this diagnostic ever read stale
	// data.
	domain := rec.Domain
	logx.Section("Removing %s", domain)

	if err := c.Writer.Remove(filepath.Join(SitesEnabled, rec.Slug+".conf")); err != nil {
		return err
	}
	if err := c.Writer.Remove(filepath.Join(SitesAvailable, rec.Slug+".conf")); err != nil {
		return err
	}
	// Stop and remove this site's isolated FPM instance: its unit's drop-in,
	// master config and pool. Removing the last site is no longer a special
	// case — with one service per site there is no shared service left
	// holding zero pools, so nothing needs propping up.
	if err := c.StopFPMService(rec.Slug); err != nil {
		return err
	}
	// Older installs put pools in the distribution's pool.d; clean that up
	// too so an upgraded machine does not leave an unjailed pool behind for
	// the shared service to pick up if a package update re-enables it.
	legacyPool := fmt.Sprintf("/etc/php/%s/fpm/pool.d/%s.conf", rec.PHPVersion, rec.Slug)
	if err := c.Writer.Remove(legacyPool); err != nil {
		return err
	}

	if purgeDB && rec.DBName != "" {
		if err := c.dbClient().Drop(c.Context, rec.DBName, rec.DBUser, "localhost"); err != nil {
			logx.Warn("database removal failed: %v", err)
		}
	} else if rec.DBName != "" {
		logx.Info("database %s kept; remove it with --purge-db", rec.DBName)
	}

	if purgeFiles {
		if !c.Writer.DryRun {
			if err := os.RemoveAll(c.Path(c.SiteRoot(rec.Slug))); err != nil {
				logx.Warn("could not remove %s: %v", c.SiteRoot(rec.Slug), err)
			} else {
				logx.Change("removed %s", c.SiteRoot(rec.Slug))
			}
		}
		if err := system.DeleteSystemUser(c.Context, c.Runner, rec.User); err != nil {
			logx.Warn("could not remove user %s: %v", rec.User, err)
		}
	} else {
		logx.Info("files in %s kept; remove them with --purge-files", c.SiteRoot(rec.Slug))
	}

	if err := c.ValidateNginx(); err != nil {
		return err
	}
	if err := c.ValidatePHP(); err != nil {
		return err
	}
	if !c.Writer.DryRun {
		// StopFPMService already stopped this site's own instance; no other
		// site's PHP is touched, so removing one site can no longer
		// interrupt requests being served by the others.
		if err := system.Reload(c.Context, c.Runner, "nginx.service"); err != nil {
			return err
		}
		c.State.Delete(rec.Slug)
		if err := c.State.Save(); err != nil {
			return err
		}
	}
	logx.Change("removed %s", domain)
	return nil
}

// SetEnabled enables or disables a site without deleting anything.
func (c *Ctx) SetEnabled(nameOrSlug string, enabled bool) error {
	rec, err := c.State.Find(nameOrSlug)
	if err != nil {
		return err
	}
	link := filepath.Join(SitesEnabled, rec.Slug+".conf")
	if enabled {
		if err := c.Writer.Symlink(filepath.Join(SitesAvailable, rec.Slug+".conf"), link); err != nil {
			return err
		}
	} else if err := c.Writer.Remove(link); err != nil {
		return err
	}

	if err := c.ValidateNginx(); err != nil {
		return err
	}
	rec.Enabled = enabled
	if !c.Writer.DryRun {
		if err := system.Reload(c.Context, c.Runner, "nginx.service"); err != nil {
			return err
		}
		return c.State.Save()
	}
	return nil
}

// FixPermissions restores the ownership and modes a site needs.
//
// The previous `fixperm` chowned wp-content to www-data, which is precisely the
// arrangement that lets a compromised plugin rewrite the site's own code. Here
// ownership goes to the site user, with the group set so nginx can still read.
func (c *Ctx) FixPermissions(slugs []string) error {
	targets := c.State.Sites
	if len(slugs) > 0 {
		targets = nil
		for _, s := range slugs {
			rec, err := c.State.Find(s)
			if err != nil {
				return err
			}
			targets = append(targets, *rec)
		}
	}
	if len(targets) == 0 {
		logx.Info("no sites registered")
		return nil
	}

	for _, rec := range targets {
		logx.Step("fixing permissions for %s", rec.Domain)
		if c.Writer.DryRun {
			continue
		}
		root := c.Path(rec.Root)
		if _, err := os.Stat(root); err != nil {
			logx.Warn("%s: document root %s is missing", rec.Domain, rec.Root)
			continue
		}
		if err := c.chownRecursive(rec.Root, rec.User+":www-data"); err != nil {
			return err
		}
		if err := applyModes(root); err != nil {
			return err
		}
		// wp-config.php is the one file whose exposure costs the database.
		wpConfig := filepath.Join(root, "wp-config.php")
		if _, err := os.Stat(wpConfig); err == nil {
			_ = os.Chmod(wpConfig, 0o640)
		}
		logx.Change("%s: ownership %s:www-data, directories 2750, files 0640", rec.Domain, rec.User)
	}
	return nil
}

// applyModes sets directories to 2750 and files to 0640 across a site tree.
// The setgid bit on directories is what keeps newly uploaded media readable by
// nginx without making it readable by every other site.
func applyModes(root string) error {
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			return os.Chmod(p, 0o2750)
		}
		return os.Chmod(p, 0o640)
	})
}

// chown applies ownership on the live filesystem only. Under --root the tree is
// a copy, the accounts do not exist there, and nothing runs out of it.
func (c *Ctx) chown(path, spec string) error {
	if c.Writer.DryRun || c.Writer.Root != "" {
		return nil
	}
	return render.Chown(c.Path(path), spec)
}

func (c *Ctx) chownRecursive(path, spec string) error {
	if c.Writer.DryRun || c.Writer.Root != "" {
		return nil
	}
	return render.ChownRecursive(c.Path(path), spec)
}

// DBClient exposes the database client for callers outside this package that
// need it directly — the live stats dashboard, in particular, which queries
// schema sizes on its own timer rather than through a provision.Ctx method.
func (c *Ctx) DBClient() db.Client { return c.dbClient() }

func (c *Ctx) dbClient() db.Client {
	return db.Client{Runner: c.Runner, Flavor: c.Facts.DBFlavor}
}

func dedupeAliases(domain string, aliases []string) []string {
	seen := map[string]bool{domain: true}
	var out []string
	// www is the alias almost every site wants and the one most often
	// forgotten, so it is added unless the domain is itself a subdomain.
	if strings.Count(domain, ".") == 1 {
		out = append(out, "www."+domain)
		seen["www."+domain] = true
	}
	for _, a := range aliases {
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

func certKeyPath(rec state.Site) string {
	if rec.CertPath == "" {
		return ""
	}
	return strings.Replace(rec.CertPath, "fullchain.pem", "privkey.pem", 1)
}
