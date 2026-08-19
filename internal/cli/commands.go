package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"ngxsetup/internal/logx"
	"ngxsetup/internal/provision"
	"ngxsetup/internal/security"
	"ngxsetup/internal/state"
	"ngxsetup/internal/stats"
	"ngxsetup/internal/system"
	"ngxsetup/internal/tui"
	"ngxsetup/internal/webui"
)

// ---- setup -----------------------------------------------------------------

func cmdSetup(ctx context.Context, args []string) error {
	fs := newFlagSet("setup")
	var g globalOpts
	g.register(fs)
	database := fs.String("db", "mariadb", "database server: mariadb or mysql")
	skipPackages := fs.Bool("skip-packages", false, "assume packages are installed; apply configuration only")
	redis := fs.Bool("redis", false, "install Redis for the WordPress object cache")
	_, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	g.applyLogging()

	if *database != "mariadb" && *database != "mysql" {
		return fmt.Errorf("--db must be mariadb or mysql, got %q", *database)
	}

	c, err := provision.New(ctx, g.provisionOptions())
	if err != nil {
		return err
	}
	if err := c.Setup(provision.SetupRequest{
		Database:     *database,
		SkipPackages: *skipPackages,
		InstallRedis: *redis,
	}); err != nil {
		return err
	}

	logx.Section("Done")
	if g.dryRun {
		logx.Info("This was a dry run. Re-run without --dry-run to apply.")
		return nil
	}
	logx.Info("Add your first site:")
	logx.Info("  ngxsetup site add example.com --wordpress --tls")
	logx.Info("")
	logx.Info("Check the result at any time with: ngxsetup doctor")
	return nil
}

// ---- site ------------------------------------------------------------------

func cmdSite(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ngxsetup site <add|list|info|remove|enable|disable|fix-perms>")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add", "create":
		return cmdSiteAdd(ctx, rest)
	case "list", "ls":
		return cmdSiteList(ctx, rest)
	case "info":
		return cmdSiteInfo(ctx, rest)
	case "remove", "rm", "delete":
		return cmdSiteRemove(ctx, rest)
	case "enable":
		return cmdSiteEnable(ctx, rest, true)
	case "disable":
		return cmdSiteEnable(ctx, rest, false)
	case "fix-perms", "fixperm":
		return cmdSiteFixPerms(ctx, rest)
	default:
		return fmt.Errorf("unknown site subcommand %q", sub)
	}
}

func cmdSiteAdd(ctx context.Context, args []string) error {
	fs := newFlagSet("site add")
	var g globalOpts
	g.register(fs)
	wordpress := fs.Bool("wordpress", false, "install WordPress and provision a database")
	tls := fs.Bool("tls", false, "obtain a Let's Encrypt certificate (DNS must already point here)")
	selfSigned := fs.Bool("self-signed", false, "generate a local certificate instead of requesting one")
	aliases := fs.String("alias", "", "additional server names, comma separated")
	install := fs.Bool("install", false, "complete the WordPress installation unattended")
	adminUser := fs.String("admin-user", "", "WordPress administrator name (with --install)")
	adminEmail := fs.String("admin-email", "", "WordPress administrator email (with --install)")
	title := fs.String("title", "", "site title (with --install)")
	noCache := fs.Bool("no-cache", false, "disable the FastCGI micro-cache for this site")
	allowFileMods := fs.Bool("allow-file-mods", false, "let WordPress write to its own code directories")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	g.applyLogging()

	domain := arg(pos, 0)
	if domain == "" {
		// The previous tool was a menu; asking once when the argument is
		// missing keeps that path working without making it the only path.
		fmt.Print("Domain name: ")
		if _, err := fmt.Scanln(&domain); err != nil {
			return fmt.Errorf("a domain name is required: ngxsetup site add example.com")
		}
	}
	if *tls && *selfSigned {
		return fmt.Errorf("--tls and --self-signed are mutually exclusive")
	}
	if *install && !*wordpress {
		return fmt.Errorf("--install requires --wordpress")
	}
	if *install && *adminEmail == "" {
		return fmt.Errorf("--install requires --admin-email")
	}

	c, err := provision.New(ctx, g.provisionOptions())
	if err != nil {
		return err
	}
	if err := requireSetup(c); err != nil {
		return err
	}

	rec, err := c.AddSite(provision.SiteRequest{
		Domain:        domain,
		Aliases:       splitList(*aliases),
		WordPress:     *wordpress,
		TLS:           *tls,
		SelfSigned:    *selfSigned,
		InstallWP:     *install,
		AdminUser:     *adminUser,
		AdminEmail:    *adminEmail,
		Title:         *title,
		DisableCache:  *noCache,
		AllowFileMods: *allowFileMods,
	})
	if err != nil {
		return err
	}

	scheme := "http"
	if rec.TLS {
		scheme = "https"
	}
	logx.Section("%s is ready", rec.Domain)
	logx.Info("  %s://%s", scheme, rec.Domain)
	if rec.DBName != "" {
		logx.Info("  credentials: %s/%s.txt (mode 0600, root only)", provision.CredentialsDir, rec.Slug)
	}
	if rec.CertSource == "self-signed" {
		logx.Info("")
		logx.Info("  The certificate is self-signed, so browsers will warn. Once DNS points")
		logx.Info("  here, issue a real one with: ngxsetup ssl issue %s", rec.Domain)
	}
	return nil
}

func cmdSiteList(ctx context.Context, args []string) error {
	fs := newFlagSet("site list")
	var g globalOpts
	g.register(fs)
	_, err := parseArgs(fs, args)
	if err != nil {
		return err
	}

	c, err := provision.New(ctx, g.provisionOptions())
	if err != nil {
		return err
	}
	if len(c.State.Sites) == 0 {
		logx.Info("No sites configured. Add one with: ngxsetup site add example.com --wordpress")
		return nil
	}

	fmt.Printf("%-28s  %-8s  %-6s  %-18s  %s\n", "DOMAIN", "STATE", "TLS", "DATABASE", "ROOT")
	for _, s := range c.State.Sites {
		state := "enabled"
		if !s.Enabled {
			state = "disabled"
		}
		tlsLabel := "-"
		switch s.CertSource {
		case "letsencrypt":
			tlsLabel = "LE"
		case "self-signed":
			tlsLabel = "self"
		case "custom":
			tlsLabel = "custom"
		}
		fmt.Printf("%-28s  %-8s  %-6s  %-18s  %s\n",
			truncate(s.Domain, 28), state, tlsLabel, orDash(s.DBName), s.Root)
	}
	return nil
}

func cmdSiteInfo(ctx context.Context, args []string) error {
	fs := newFlagSet("site info")
	var g globalOpts
	g.register(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if arg(pos, 0) == "" {
		return fmt.Errorf("usage: ngxsetup site info <domain>")
	}

	c, err := provision.New(ctx, g.provisionOptions())
	if err != nil {
		return err
	}
	s, err := c.State.Find(arg(pos, 0))
	if err != nil {
		return err
	}

	logx.Section("%s", s.Domain)
	logx.KV([][2]string{
		{"server names", s.ServerNames()},
		{"document root", s.Root},
		{"system user", s.User},
		{"php pool", s.SocketPath},
		{"php version", s.PHPVersion},
		{"database", orDash(s.DBName)},
		{"database user", orDash(s.DBUser)},
		{"tls", orDash(s.CertSource)},
		{"certificate", orDash(s.CertPath)},
		{"micro-cache", boolLabel(s.CacheEnabled, "enabled", "disabled")},
		{"state", boolLabel(s.Enabled, "enabled", "disabled")},
		{"created", s.CreatedAt},
		{"nginx config", "/etc/nginx/sites-available/" + s.Slug + ".conf"},
		{"custom overrides", "/etc/nginx/sites-available/" + s.Slug + ".custom.conf"},
		{"access log", "/var/log/nginx/" + s.Slug + ".access.log"},
	})
	return nil
}

func cmdSiteRemove(ctx context.Context, args []string) error {
	fs := newFlagSet("site remove")
	var g globalOpts
	g.register(fs)
	purgeFiles := fs.Bool("purge-files", false, "also delete the site's files and system user")
	purgeDB := fs.Bool("purge-db", false, "also drop the site's database and database user")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	g.applyLogging()

	if arg(pos, 0) == "" {
		return fmt.Errorf("usage: ngxsetup site remove <domain> [--purge-files] [--purge-db]")
	}

	c, err := provision.New(ctx, g.provisionOptions())
	if err != nil {
		return err
	}
	s, err := c.State.Find(arg(pos, 0))
	if err != nil {
		return err
	}

	// Data loss is confirmed explicitly, and the confirmation says exactly
	// what will be destroyed rather than asking "are you sure?".
	if !*yes && !g.dryRun && (*purgeFiles || *purgeDB) {
		logx.Warn("This will permanently delete:")
		if *purgeFiles {
			logx.Bullet("all files under %s", c.SiteRoot(s.Slug))
			logx.Bullet("the system user %s", s.User)
		}
		if *purgeDB && s.DBName != "" {
			logx.Bullet("the database %s and all of its content", s.DBName)
		}
		if !confirm(fmt.Sprintf("Remove %s and the items above?", s.Domain)) {
			logx.Info("Cancelled; nothing was changed.")
			return nil
		}
	}
	return c.RemoveSite(arg(pos, 0), *purgeFiles, *purgeDB)
}

func cmdSiteEnable(ctx context.Context, args []string, enabled bool) error {
	fs := newFlagSet("site enable")
	var g globalOpts
	g.register(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	g.applyLogging()
	if arg(pos, 0) == "" {
		return fmt.Errorf("usage: ngxsetup site enable|disable <domain>")
	}

	c, err := provision.New(ctx, g.provisionOptions())
	if err != nil {
		return err
	}
	return c.SetEnabled(arg(pos, 0), enabled)
}

func cmdSiteFixPerms(ctx context.Context, args []string) error {
	fs := newFlagSet("site fix-perms")
	var g globalOpts
	g.register(fs)
	all := fs.Bool("all", false, "apply to every site")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	g.applyLogging()

	c, err := provision.New(ctx, g.provisionOptions())
	if err != nil {
		return err
	}
	var slugs []string
	if !*all {
		slugs = pos
		if len(slugs) == 0 {
			return fmt.Errorf("name a site, or pass --all")
		}
	}
	return c.FixPermissions(slugs)
}

// ---- tune ------------------------------------------------------------------

func cmdTune(ctx context.Context, args []string) error {
	fs := newFlagSet("tune")
	var g globalOpts
	g.register(fs)
	apply := fs.Bool("apply", false, "write and activate the plan (default is to show it)")
	explain := fs.Bool("explain", false, "show how each number was derived")
	workerMB := fs.Int("php-worker-mb", 0, "assumed memory per PHP worker; raise for heavy plugin sets")
	reserveMB := fs.Int("reserve-mb", 0, "memory to withhold for the operating system")
	memoryMB := fs.Int("memory-mb", 0, "override detected total memory (for planning)")
	save := fs.Bool("save", false, "persist --profile and the overrides to the config file")
	_, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	g.applyLogging()

	opts := g.provisionOptions()
	opts.AvgPHPWorkerMB = *workerMB
	opts.ReserveMB = *reserveMB
	opts.MemoryMB = *memoryMB

	c, err := provision.New(ctx, opts)
	if err != nil {
		return err
	}

	logx.Section("Machine")
	logx.KV(c.Facts.Describe())
	logx.Section("Plan")
	logx.KV(c.Plan.Summary())

	if *explain {
		logx.Section("Derivation")
		for _, line := range c.Plan.Explain() {
			logx.Info("%s", line)
		}
	}
	for _, w := range c.Plan.Warnings {
		logx.Warn("%s", w)
	}

	if !*apply {
		logx.Info("")
		logx.Info("This is a preview. Apply it with: ngxsetup tune --apply")
		logx.Info("See the reasoning with:        ngxsetup tune --explain")
		return nil
	}

	if err := requireSetup(c); err != nil {
		return err
	}
	if err := c.Transaction("Applying nginx configuration", c.ApplyNginx, c.ValidateNginx); err != nil {
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

	if !g.dryRun {
		if err := c.ReloadServices(); err != nil {
			return err
		}
		if *save {
			c.Config.Profile = string(c.Plan.Profile)
			if *workerMB > 0 {
				c.Config.AvgPHPWorkerMB = *workerMB
			}
			if *reserveMB > 0 {
				c.Config.ReserveMB = *reserveMB
			}
			if err := c.Config.Save(); err != nil {
				return err
			}
			logx.Change("saved settings to %s", c.Config.Path())
		}
		c.State.Profile = string(c.Plan.Profile)
		c.State.Touch()
		if err := c.State.Save(); err != nil {
			return err
		}
	}
	return nil
}

// ---- doctor and status -----------------------------------------------------

func cmdDoctor(ctx context.Context, args []string) error {
	fs := newFlagSet("doctor")
	var g globalOpts
	g.register(fs)
	_, err := parseArgs(fs, args)
	if err != nil {
		return err
	}

	c, err := provision.New(ctx, g.provisionOptions())
	if err != nil {
		return err
	}

	logx.Section("Diagnostics")
	checks := c.Diagnose()

	var failures, warnings int
	for _, ch := range checks {
		switch ch.Status {
		case provision.StatusOK:
			logx.Change("%-24s %s", ch.Name, ch.Detail)
		case provision.StatusWarn:
			warnings++
			logx.Warn("%-24s %s", ch.Name, ch.Detail)
			if ch.Fix != "" {
				logx.Info("    → %s", ch.Fix)
			}
		case provision.StatusFail:
			failures++
			logx.Error("%-24s %s", ch.Name, ch.Detail)
			if ch.Fix != "" {
				logx.Info("    → %s", ch.Fix)
			}
		}
	}

	logx.Section("Summary")
	logx.Info("%d checks, %d failures, %d warnings", len(checks), failures, warnings)
	if failures > 0 {
		// A non-zero exit makes this usable from a monitoring check.
		return fmt.Errorf("%d check(s) failed", failures)
	}
	return nil
}

func cmdStatus(ctx context.Context, args []string) error {
	fs := newFlagSet("status")
	var g globalOpts
	g.register(fs)
	_, err := parseArgs(fs, args)
	if err != nil {
		return err
	}

	c, err := provision.New(ctx, g.provisionOptions())
	if err != nil {
		return err
	}
	rows, err := c.Status()
	if err != nil {
		return err
	}
	logx.Section("ngxsetup %s", Version)
	logx.KV(rows)
	return nil
}

// ---- uninstall ---------------------------------------------------------------

func cmdUninstall(ctx context.Context, args []string) error {
	fs := newFlagSet("uninstall")
	var g globalOpts
	g.register(fs)
	purgeSites := fs.Bool("purge-sites", false, "also delete every site's files, database and system user")
	purgePackages := fs.Bool("purge-packages", false, "also remove nginx, PHP and the database server themselves")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	g.applyLogging()

	c, err := provision.New(ctx, g.provisionOptions())
	if err != nil {
		return err
	}

	opts := provision.UninstallOptions{PurgeSites: *purgeSites, PurgePackages: *purgePackages}
	plan := c.PlanUninstall(opts)

	logx.Section("This will:")
	for _, line := range plan.Describe() {
		logx.Bullet("%s", line)
	}
	if !*purgeSites && len(c.State.Sites) > 0 {
		logx.Info("")
		logx.Info("Site files and databases are kept by default. Pass --purge-sites to delete them too.")
	}
	if !*purgePackages {
		logx.Info("nginx, PHP and the database server stay installed. Pass --purge-packages to remove them too.")
	}

	if !g.dryRun && !*yes {
		logx.Info("")
		if !confirm("This cannot be undone. Continue?") {
			logx.Info("Cancelled; nothing was changed.")
			return nil
		}
	}

	return c.Uninstall(opts)
}

// ---- db --------------------------------------------------------------------

func cmdDB(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ngxsetup db <backup|restore> ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "backup":
		return cmdDBBackup(ctx, rest)
	case "restore":
		return cmdDBRestore(ctx, rest)
	default:
		return fmt.Errorf("unknown db subcommand %q", sub)
	}
}

func cmdDBBackup(ctx context.Context, args []string) error {
	fs := newFlagSet("db backup")
	var g globalOpts
	g.register(fs)
	out := fs.String("out", "", "destination directory (default: "+provision.DefaultBackupDir+")")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	g.applyLogging()

	c, err := provision.New(ctx, g.provisionOptions())
	if err != nil {
		return err
	}

	domain := arg(pos, 0)
	if domain != "" {
		result, err := c.BackupDatabase(domain, *out)
		if err != nil {
			return err
		}
		logx.Section("Backup complete")
		logx.KV([][2]string{{"database", result.DBName}, {"file", result.Path}})
		return nil
	}

	// No domain given: back up every site in one pass — the "one click"
	// case. --out set once applies to all of them, so a single command
	// really does produce one file per database in one destination.
	results, err := c.BackupAllDatabases(*out)
	if err != nil {
		return err
	}
	logx.Section("Summary")
	failed := 0
	for _, r := range results {
		if r.Err != nil {
			logx.Error("%s: %v", r.Domain, r.Err)
			failed++
			continue
		}
		logx.Info("%s -> %s (%.1f MB)", r.Domain, r.Path, r.SizeMB)
	}
	logx.Info("%d database(s) backed up, %d failed", len(results)-failed, failed)
	if failed > 0 {
		return fmt.Errorf("%d database backup(s) failed — see above", failed)
	}
	return nil
}

func cmdDBRestore(ctx context.Context, args []string) error {
	fs := newFlagSet("db restore")
	var g globalOpts
	g.register(fs)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	noSafetyBackup := fs.Bool("no-safety-backup", false, "skip backing up the database's current contents before overwriting it")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	g.applyLogging()

	domain := arg(pos, 0)
	sqlPath := arg(pos, 1)
	if domain == "" || sqlPath == "" {
		return fmt.Errorf("usage: ngxsetup db restore <domain> <file.sql> [--yes] [--no-safety-backup]")
	}

	c, err := provision.New(ctx, g.provisionOptions())
	if err != nil {
		return err
	}

	site, err := c.State.Find(domain)
	if err != nil {
		return err
	}

	if !*yes && !g.dryRun {
		logx.Warn("this replaces every table in %s (%s) with the contents of %s — anything written since the file was taken is gone", site.Domain, site.DBName, sqlPath)
		if !confirm(fmt.Sprintf("Restore %s from %s?", site.Domain, sqlPath)) {
			logx.Info("cancelled")
			return nil
		}
	}

	result, err := c.RestoreDatabase(domain, sqlPath, *noSafetyBackup)
	if err != nil {
		return err
	}
	logx.Section("Restore complete")
	rows := [][2]string{{"database", result.DBName}, {"restored from", sqlPath}}
	if result.SafetyBackup != "" {
		rows = append(rows, [2]string{"safety backup", result.SafetyBackup})
	}
	logx.KV(rows)
	return nil
}

// ---- security ----------------------------------------------------------------

func cmdSecurity(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ngxsetup security <scan|patch|install-clamav> [domain]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "scan":
		return cmdSecurityScan(ctx, rest)
	case "patch":
		return cmdSecurityPatch(ctx, rest)
	case "install-clamav":
		return cmdSecurityInstallClamAV(ctx, rest)
	default:
		return fmt.Errorf("unknown security subcommand %q", sub)
	}
}

// cmdSecurityInstallClamAV is the one-click alternative to an operator
// running `apt install clamav` themselves after a scan reports it missing.
func cmdSecurityInstallClamAV(ctx context.Context, args []string) error {
	fs := newFlagSet("security install-clamav")
	var g globalOpts
	g.register(fs)
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	g.applyLogging()
	c, err := provision.New(ctx, g.provisionOptions())
	if err != nil {
		return err
	}
	return c.InstallClamAV()
}

// securityTargets resolves the site (or every site) a security command
// should act on, building the document-root and system-user pair the
// security package needs to run wp-cli safely as that site's own account.
func securityTargets(c *provision.Ctx, domain string) ([]security.Target, error) {
	var sites []state.Site
	if domain != "" {
		s, err := c.State.Find(domain)
		if err != nil {
			return nil, err
		}
		sites = []state.Site{*s}
	} else {
		sites = c.State.Sites
	}
	if len(sites) == 0 {
		return nil, fmt.Errorf("no sites configured; add one with `ngxsetup site add example.com --wordpress`")
	}

	targets := make([]security.Target, 0, len(sites))
	for _, s := range sites {
		if !s.WordPress {
			continue // nothing to scan: no wp-cli-manageable install exists for a plain vhost
		}
		targets = append(targets, security.Target{
			Domain: s.Domain,
			User:   s.User,
			Root:   c.Path(s.Root),
		})
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no WordPress sites to scan (plain vhosts have nothing for wp-cli or a malware scan to check)")
	}
	return targets, nil
}

func cmdSecurityScan(ctx context.Context, args []string) error {
	fs := newFlagSet("security scan")
	var g globalOpts
	g.register(fs)
	yaraRules := fs.String("yara-rules", "", "additional YARA rules directory, supplementing the bundled ruleset")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	g.applyLogging()

	c, err := provision.New(ctx, g.provisionOptions())
	if err != nil {
		return err
	}
	targets, err := securityTargets(c, arg(pos, 0))
	if err != nil {
		return err
	}

	rulesDir := c.Config.SecurityYARARulesDir
	if *yaraRules != "" {
		rulesDir = *yaraRules
	}
	scanner := security.Scanner{Runner: c.Runner, YARARulesDir: rulesDir}

	totalCritical, totalWarning := 0, 0
	for _, target := range targets {
		logx.Section("Scanning %s", target.Domain)
		report, err := scanner.Scan(ctx, target)
		if err != nil {
			logx.Error("%v", err)
			continue
		}
		printSecurityReport(*report)
		c, w, _ := report.CountBySeverity()
		totalCritical += c
		totalWarning += w
	}

	logx.Section("Summary")
	logx.Info("%d site(s) scanned: %d critical, %d warning finding(s)", len(targets), totalCritical, totalWarning)
	if totalCritical > 0 {
		return fmt.Errorf("%d critical finding(s) — see above", totalCritical)
	}
	return nil
}

func printSecurityReport(report security.Report) {
	if len(report.LayersRun) > 0 {
		logx.Info("layers run: %s", strings.Join(report.LayersRun, ", "))
	}
	for layer, reason := range report.LayersSkipped {
		logx.Warn("%s skipped: %s", layer, reason)
	}
	if report.Clean() {
		logx.Change("no critical or warning findings")
	}
	for _, f := range report.Findings {
		line := f.Title
		if f.Path != "" {
			line += " (" + f.Path + ")"
		}
		switch f.Severity {
		case security.Critical:
			logx.Error("%s", line)
		case security.Warning:
			logx.Warn("%s", line)
		default:
			logx.Info("%s", line)
		}
		logx.Info("    %s", f.Detail)
		if f.Fix != "" {
			logx.Info("    -> %s", f.Fix)
		}
	}
}

func cmdSecurityPatch(ctx context.Context, args []string) error {
	fs := newFlagSet("security patch")
	var g globalOpts
	g.register(fs)
	yes := fs.Bool("yes", false, "do not ask for confirmation before applying updates")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	g.applyLogging()

	c, err := provision.New(ctx, g.provisionOptions())
	if err != nil {
		return err
	}
	targets, err := securityTargets(c, arg(pos, 0))
	if err != nil {
		return err
	}

	patched, failed := 0, 0
	for _, target := range targets {
		wp := security.WPCLI{Runner: c.Runner, User: target.User, Path: target.Root}
		if !wp.Available() {
			logx.Warn("%s: wp-cli is not installed; skipping", target.Domain)
			continue
		}

		logx.Section("Checking %s", target.Domain)
		plan, err := wp.PlanPatch(ctx, target.Domain)
		if err != nil {
			logx.Error("%s: %v", target.Domain, err)
			failed++
			continue
		}
		if plan.Empty() {
			logx.Change("%s is already up to date", target.Domain)
			continue
		}

		for _, line := range plan.Describe() {
			logx.Bullet("%s", line)
		}
		if !*yes && !g.dryRun {
			if !confirm(fmt.Sprintf("Apply these updates to %s?", target.Domain)) {
				logx.Info("skipped %s", target.Domain)
				continue
			}
		}
		if g.dryRun {
			logx.Info("[dry-run] would apply the updates above to %s", target.Domain)
			continue
		}

		result := wp.ApplyPatch(ctx, plan)
		if result.CoreUpdated {
			logx.Change("%s: WordPress core updated to %s", target.Domain, plan.CoreUpdate)
		}
		if result.CoreErr != nil {
			logx.Error("%s: core update failed: %v", target.Domain, result.CoreErr)
		}
		for _, p := range result.PluginsUpdated {
			logx.Change("%s: plugin %s updated", target.Domain, p)
		}
		for name, err := range result.PluginErrs {
			logx.Error("%s: plugin %s update failed: %v", target.Domain, name, err)
		}
		for _, t := range result.ThemesUpdated {
			logx.Change("%s: theme %s updated", target.Domain, t)
		}
		for name, err := range result.ThemeErrs {
			logx.Error("%s: theme %s update failed: %v", target.Domain, name, err)
		}

		if result.Success() {
			patched++
		} else {
			failed++
		}
	}

	logx.Section("Summary")
	logx.Info("%d site(s) patched, %d with failures", patched, failed)
	if failed > 0 {
		return fmt.Errorf("%d site(s) had one or more failed updates — see above", failed)
	}
	return nil
}

// ---- top -------------------------------------------------------------------

func cmdTop(ctx context.Context, args []string) error {
	fs := newFlagSet("top")
	var g globalOpts
	g.register(fs)
	interval := fs.Duration("interval", tui.DefaultInterval, "refresh interval")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}

	c, err := provision.New(ctx, g.provisionOptions())
	if err != nil {
		return err
	}
	if len(c.State.Sites) == 0 {
		return fmt.Errorf("no sites configured yet; add one with `ngxsetup site add example.com --wordpress`")
	}

	// The sampler owns its own DB client independent of anything else this
	// process might be doing, and needs no --dry-run awareness: reading
	// process stats, tailing logs and querying schema sizes are all
	// read-only, the same way `status` and `doctor` never ask about dry-run.
	db := c.DBClient()
	sampler := stats.NewSampler(db)

	// *provision.Ctx satisfies both tui.SiteProvider (via Sites) and
	// tui.CachePurger (via PurgeCache) structurally — no adapter needed.
	return tui.Run(c, sampler, c, *interval)
}

// ---- web --------------------------------------------------------------------

func cmdWeb(ctx context.Context, args []string) error {
	fs := newFlagSet("web")
	bind := fs.String("bind", "0.0.0.0", "address to listen on")
	port := fs.Int("port", 0, "port to listen on (0 picks a random free port)")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}

	if err := system.RequireRoot(); err != nil {
		return err
	}

	// This command is meant to live exactly as long as the terminal session
	// that started it, never longer — see the package doc in
	// internal/webui/server.go for why there is no login to begin with.
	// cli.Run's own signal handling covers Ctrl+C (SIGINT) and SIGTERM
	// already; SIGHUP — what the process actually receives when its
	// controlling terminal closes — is added here rather than globally,
	// since aborting every other command (mid `apt-get install`, say) on a
	// dropped SSH connection would trade one problem for a worse one.
	// Catching it, rather than leaving it at its default disposition (which
	// would also kill the process, just without the firewall-rule cleanup
	// Serve's deferred shutdown path performs), is what makes a closed
	// terminal a *clean* shutdown instead of an abrupt one.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGHUP)
	defer stop()

	srv, err := webui.New(webui.Config{Bind: *bind, Port: *port})
	if err != nil {
		return err
	}

	urlReady := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ctx, urlReady) }()

	select {
	case url := <-urlReady:
		logx.Section("ngxsetup web")
		logx.Info("  %s", url)
		logx.Info("")
		logx.Warn("no login is required — anyone who can reach this address has full")
		logx.Warn("control of this server. Prefer --bind 127.0.0.1 behind an SSH tunnel,")
		logx.Warn("or a network only you can reach, over leaving this open to the internet.")
		logx.Info("")
		logx.Info("  The certificate is self-signed, so browsers will warn on first visit.")
		logx.Info("  This stops when the terminal does. Press Ctrl+C to stop it now.")
	case err := <-errCh:
		return err
	}

	return <-errCh
}

// ---- cache -----------------------------------------------------------------

func cmdCache(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ngxsetup cache <purge|stats> [domain]")
	}
	sub, rest := args[0], args[1:]

	fs := newFlagSet("cache")
	var g globalOpts
	g.register(fs)
	pos, err := parseArgs(fs, rest)
	if err != nil {
		return err
	}
	g.applyLogging()

	c, err := provision.New(ctx, g.provisionOptions())
	if err != nil {
		return err
	}

	switch sub {
	case "purge":
		return c.PurgeCache(arg(pos, 0))
	case "stats":
		entries, bytesUsed, err := c.CacheStats()
		if err != nil {
			return err
		}
		logx.KV([][2]string{
			{"entries", fmt.Sprint(entries)},
			{"on disk", fmt.Sprintf("%d MB", bytesUsed/(1<<20))},
			{"capacity", fmt.Sprintf("%d MB", c.Plan.Nginx.CacheMaxSizeMB)},
			{"keys zone", fmt.Sprintf("%d MB", c.Plan.Nginx.CacheKeysZoneMB)},
		})
		return nil
	default:
		return fmt.Errorf("unknown cache subcommand %q", sub)
	}
}

// ---- ssl -------------------------------------------------------------------

func cmdSSL(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ngxsetup ssl <issue|renew> [domain]")
	}
	sub, rest := args[0], args[1:]

	fs := newFlagSet("ssl")
	var g globalOpts
	g.register(fs)
	pos, err := parseArgs(fs, rest)
	if err != nil {
		return err
	}
	g.applyLogging()

	c, err := provision.New(ctx, g.provisionOptions())
	if err != nil {
		return err
	}

	switch sub {
	case "issue":
		if arg(pos, 0) == "" {
			return fmt.Errorf("usage: ngxsetup ssl issue <domain>")
		}
		return c.UpgradeToTLS(arg(pos, 0))
	case "renew":
		return c.RenewCertificates()
	default:
		return fmt.Errorf("unknown ssl subcommand %q", sub)
	}
}

// ---- secure ----------------------------------------------------------------

func cmdSecure(ctx context.Context, args []string) error {
	fs := newFlagSet("secure")
	var g globalOpts
	g.register(fs)
	apply := fs.Bool("apply", false, "apply the hardening configuration")
	refreshCF := fs.Bool("refresh-cloudflare", false, "refresh the trusted Cloudflare address ranges")
	pmaUser := fs.String("phpmyadmin-user", "", "set the phpMyAdmin HTTP credential for this user")
	_, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	g.applyLogging()

	c, err := provision.New(ctx, g.provisionOptions())
	if err != nil {
		return err
	}

	if *pmaUser != "" {
		password, err := readSecret("Password for phpMyAdmin user " + *pmaUser + ": ")
		if err != nil {
			return err
		}
		if err := c.SetPhpMyAdminCredential(*pmaUser, password); err != nil {
			return err
		}
	}
	if *refreshCF {
		c.Config.TrustCloudflare = true
	}
	if !*apply && !*refreshCF && *pmaUser == "" {
		return fmt.Errorf("nothing to do; pass --apply, --refresh-cloudflare or --phpmyadmin-user")
	}
	if *apply || *refreshCF {
		if err := c.Transaction("Hardening", func() error {
			if err := c.ApplySecurity(); err != nil {
				return err
			}
			if *refreshCF {
				return c.ApplyNginx()
			}
			return c.ApplyPhpMyAdmin()
		}, c.ValidateNginx); err != nil {
			return err
		}
		if !g.dryRun {
			return c.ReloadServices()
		}
	}
	return nil
}

// ---- config ----------------------------------------------------------------

func cmdConfig(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ngxsetup config <show|get|set> [key] [value]")
	}
	sub, rest := args[0], args[1:]

	fs := newFlagSet("config")
	var g globalOpts
	g.register(fs)
	pos, err := parseArgs(fs, rest)
	if err != nil {
		return err
	}

	c, err := provision.New(ctx, g.provisionOptions())
	if err != nil {
		return err
	}

	switch sub {
	case "show":
		data, err := os.ReadFile(c.Config.Path())
		if err != nil {
			logx.Info("No configuration file yet; these are the defaults.")
			logx.KV(configRows(c))
			return nil
		}
		logx.Raw(string(data))
		return nil
	case "get":
		if arg(pos, 0) == "" {
			return fmt.Errorf("usage: ngxsetup config get <key>")
		}
		for _, row := range configRows(c) {
			if row[0] == arg(pos, 0) {
				fmt.Println(row[1])
				return nil
			}
		}
		return fmt.Errorf("unknown key %q", arg(pos, 0))
	case "set":
		if arg(pos, 0) == "" || arg(pos, 1) == "" {
			return fmt.Errorf("usage: ngxsetup config set <key> <value>")
		}
		if err := setConfigKey(c, arg(pos, 0), arg(pos, 1)); err != nil {
			return err
		}
		if g.dryRun {
			logx.Info("[dry-run] would set %s = %s", arg(pos, 0), arg(pos, 1))
			return nil
		}
		if err := c.Config.Save(); err != nil {
			return err
		}
		logx.Change("set %s = %s", arg(pos, 0), arg(pos, 1))
		logx.Info("Apply it with: ngxsetup tune --apply")
		return nil
	default:
		return fmt.Errorf("unknown config subcommand %q", sub)
	}
}

// ---- helpers ---------------------------------------------------------------

func requireSetup(c *provision.Ctx) error {
	return provision.RequireSetup(c)
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func boolLabel(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}
