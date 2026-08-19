package provision

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"ngxsetup/internal/migrate"
	"ngxsetup/internal/site"
	"ngxsetup/internal/state"
	"ngxsetup/internal/system"
	"ngxsetup/internal/tmpl"
	"ngxsetup/internal/tuning"
)

// MigrateSiteRequest is what one site being moved onto this machine needs
// decided up front — everything AddSite's SiteRequest also needs for a
// vhost, minus everything specific to installing WordPress fresh, since a
// migrated site's files and database already exist.
type MigrateSiteRequest struct {
	Domain     string
	Aliases    []string
	TLS        bool
	SelfSigned bool
}

// MigrateAllocateSite reserves a slug, creates the system account and an
// empty, correctly-permissioned document root, and registers the site in
// state as disabled — everything AddSite does before installing
// WordPress, without installing it, since a migrated site's files are
// about to be rsynced into the document root this returns instead.
//
// Registering the site in state immediately (rather than only once
// migration finishes) is what lets MigrateAbortSite clean up everything
// this allocated through the exact same RemoveSite path a normal removal
// uses, rather than a second, independently-maintained teardown — if any
// later step in the migration fails, the site is torn down exactly as
// completely as `site remove --purge-files --purge-db` would.
func (c *Ctx) MigrateAllocateSite(req MigrateSiteRequest) (*state.Site, error) {
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
		return nil, fmt.Errorf("%s is already served by an existing site on this machine", domain)
	}
	if c.PHPVersion == "" {
		return nil, fmt.Errorf("PHP is not installed; run `ngxsetup setup` first")
	}

	slug := site.UniqueSlug(domain, c.State.SlugTaken)
	user := site.UserName(slug)
	rec := state.Site{
		Slug:       slug,
		Domain:     domain,
		Aliases:    dedupeAliases(domain, req.Aliases),
		Root:       c.DocumentRoot(slug),
		User:       user,
		SocketPath: c.SocketPath(slug),
		PHPVersion: c.PHPVersion,
		WordPress:  true,
		Enabled:    false, // not reachable until MigrateFinishSite completes
	}
	if err := c.createSiteAccount(rec); err != nil {
		return nil, err
	}
	if c.Writer.DryRun {
		return &rec, nil
	}
	c.State.Upsert(rec)
	if err := c.State.Save(); err != nil {
		return nil, err
	}
	return &rec, nil
}

// MigrateAbortSite tears down everything MigrateAllocateSite (and any
// step after it) created for one site — the failure path every step of a
// migration after allocation shares, via RemoveSite's own, already
// thorough teardown.
func (c *Ctx) MigrateAbortSite(rec *state.Site) {
	if rec == nil || c.Writer.DryRun {
		return
	}
	if err := c.RemoveSite(rec.Slug, true, true); err != nil {
		// Best effort: the operator is already looking at a failed
		// migration in the log; a second warning about cleanup itself
		// failing is useful but must not replace the real error.
		fmt.Fprintf(os.Stderr, "cleaning up %s after a failed migration: %v\n", rec.Domain, err)
	}
}

// MigrateProvisionDatabase creates a brand new local database and account
// — never the migrated site's original credentials, the same reasoning
// AddSite's own WordPress installs never let an operator choose a password
// either — and restores an already-downloaded, already-decompressed SQL
// dump into it. Returns the new local password, which MigrateFinishSite
// needs to write into wp-config.php.
func (c *Ctx) MigrateProvisionDatabase(rec *state.Site, localDumpPath string) (string, error) {
	suffix, err := system.Suffix(4)
	if err != nil {
		return "", err
	}
	rec.DBName = site.DBName(rec.Slug, suffix)
	rec.DBUser = rec.DBName
	password, err := system.Password(24)
	if err != nil {
		return "", err
	}
	client := c.dbClient()
	if err := client.Ping(c.Context); err != nil {
		return "", err
	}
	if err := client.Provision(c.Context, rec.DBName, rec.DBUser, password, "localhost", c.Plan.DB.Collation); err != nil {
		return "", err
	}
	if c.Writer.DryRun {
		return password, nil
	}
	if err := client.Restore(c.Context, rec.DBName, localDumpPath); err != nil {
		return "", fmt.Errorf("restoring the migrated database: %w", err)
	}
	if err := c.State.Save(); err != nil {
		return "", err
	}
	return password, nil
}

// MigrateFinishSite writes wp-config.php — preserving the migrated
// database's actual table prefix, since the tables just restored into it
// use whatever prefix the original site did, not a freshly generated one —
// fixes ownership on the now-populated document root, issues a
// certificate, writes and validates the nginx and PHP-FPM configuration,
// starts the site's isolated FPM service, reloads nginx, and marks the
// site enabled. Everything AddSite does after WordPress is in place,
// applied to files that arrived by rsync instead of a fresh download.
func (c *Ctx) MigrateFinishSite(rec *state.Site, dbInfo migrate.WPConfigInfo, dbPassword string, req MigrateSiteRequest) error {
	if !c.Writer.DryRun {
		salts, err := generateSalts()
		if err != nil {
			return err
		}
		scheme := "http"
		if req.TLS || req.SelfSigned {
			scheme = "https"
		}
		cfg, err := tmpl.Render("wordpress/wp-config.php.tmpl", tmpl.WPConfig{
			Domain:           rec.Domain,
			DBName:           rec.DBName,
			DBUser:           rec.DBUser,
			DBPassword:       dbPassword,
			TablePrefix:      dbInfo.TablePrefix,
			Salts:            salts,
			SiteURL:          scheme + "://" + rec.Domain,
			MemoryLimit:      tuning.MemString(c.Plan.PHP.MemoryLimitMB),
			AdminMemoryLimit: tuning.MemString(c.Plan.PHP.CLIMemoryLimitMB),
			DisableCron:      true,
			AllowFileMods:    false,
		})
		if err != nil {
			return err
		}
		wpConfigPath := filepath.Join(c.Path(rec.Root), "wp-config.php")
		if err := os.WriteFile(wpConfigPath, cfg, 0o640); err != nil {
			return fmt.Errorf("writing wp-config.php: %w", err)
		}
		if err := c.chownRecursive(rec.Root, rec.User+":www-data"); err != nil {
			return err
		}
		if err := os.Chmod(wpConfigPath, 0o640); err != nil {
			return err
		}
	}

	sreq := SiteRequest{Domain: rec.Domain, Aliases: rec.Aliases, TLS: req.TLS, SelfSigned: req.SelfSigned}
	if err := c.issueCertificate(rec, sreq); err != nil {
		return err
	}
	rec.Enabled = true
	if err := c.writeSiteConfigs(*rec); err != nil {
		return err
	}
	if err := c.WriteFPMService(*rec); err != nil {
		return err
	}
	if err := c.ValidateNginx(); err != nil {
		return err
	}
	if err := c.ValidateFPMService(rec.Slug); err != nil {
		return err
	}
	if !c.Writer.DryRun {
		if err := c.StartFPMService(rec.Slug); err != nil {
			return err
		}
		if err := system.Reload(c.Context, c.Runner, "nginx.service"); err != nil {
			return err
		}
		c.State.Upsert(*rec)
		if err := c.State.Save(); err != nil {
			return err
		}
	}
	return nil
}

// migratePathFixExtensions are the file types worth scanning for a
// hardcoded absolute path to the remote site's old document root. PHP
// specifically: this is squarely a caching-plugin problem, not a general
// one. WP Super Cache is the textbook case — it writes a WPCACHEHOME
// constant (into wp-config.php itself, on some versions) and its own
// wp-content/advanced-cache.php and wp-content/wp-cache-config.php
// drop-ins, all containing the site's absolute path burned in, because
// advanced-cache.php is loaded by WordPress core before the plugin's own
// autoloading can resolve where it lives any other way. Other caching and
// backup plugins (W3 Total Cache's object-cache.php among them) do the
// same thing for the same reason. None of that is specific to
// wp-config.php — the plugin drop-ins live under wp-content/, which this
// package's own rsync already transfers — so the fix has to look at every
// PHP file in the tree, not just the one this tool generates itself.
var migratePathFixExtensions = map[string]bool{".php": true}

// maxPathFixFileSize skips anything bigger than this. A real hardcoded-path
// drop-in is a few kilobytes; anything larger is not one, and reading
// megabytes of unrelated plugin code just to scan it for one literal
// string would cost real time on a large site for no benefit.
const maxPathFixFileSize = 2 << 20 // 2 MB

// MigrateFixHardcodedPaths is the sanity pass that runs after everything
// else has succeeded: it walks a freshly migrated site's document root and
// rewrites any file that still contains the remote site's old absolute
// path (oldRoot) to this site's real local path (newRoot) instead.
//
// This is a plain literal byte-string replace, not a template render or an
// attempt to parse PHP — these are files this tool did not write, and the
// only thing that needs correcting is the one absolute path that changed
// when the site moved to a different machine and (very likely) a
// different filesystem location. os.WriteFile truncates and rewrites the
// existing file in place rather than replacing it, so ownership and mode
// — already set correctly by MigrateFinishSite's chownRecursive by the
// time this runs — are left exactly as they were.
//
// Returns how many files were changed, purely for the operator-visible
// log line ("0 files" is itself useful information: it means this site
// had nothing to fix).
func (c *Ctx) MigrateFixHardcodedPaths(localRoot, oldRoot, newRoot string) (int, error) {
	if c.Writer.DryRun || oldRoot == "" || newRoot == "" || oldRoot == newRoot {
		return 0, nil
	}
	oldBytes, newBytes := []byte(oldRoot), []byte(newRoot)
	fixed := 0
	walkErr := filepath.WalkDir(c.Path(localRoot), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !migratePathFixExtensions[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() == 0 || info.Size() > maxPathFixFileSize {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil // one unreadable file is not worth failing an otherwise-successful migration over
		}
		if !bytes.Contains(data, oldBytes) {
			return nil
		}
		if err := os.WriteFile(p, bytes.ReplaceAll(data, oldBytes, newBytes), info.Mode()); err != nil {
			return fmt.Errorf("rewriting %s: %w", p, err)
		}
		fixed++
		return nil
	})
	if walkErr != nil {
		return fixed, walkErr
	}
	return fixed, nil
}

// DiscoveredSite is what a discovery pass found for one remote vhost,
// combined with what MigrateRunOne needs to actually move it — the single
// shape both the CLI and the web UI build from their own discovery results
// so the two front ends drive one migration pipeline instead of two
// independently maintained copies of it.
type DiscoveredSite struct {
	Domain  string
	Aliases []string
	Root    string // remote document root
	DBInfo  migrate.WPConfigInfo
}

// MigrateProgress receives live updates as MigrateRunOne works through one
// site. The CLI implements it with direct logx calls, blocking and
// streaming to the terminal the way every other command already does; the
// web UI implements it against a job status a browser polls. Percent is
// meaningful only during the two transfer steps (the database dump and the
// document root) — 0 at the start of every other step, since those either
// finish in well under a second or have no natural progress fraction of
// their own to report.
type MigrateProgress interface {
	Step(step string)
	Percent(pct int)
	Log(line string)
}

// remoteMigrateStagingDir is where a remote host's database dump is
// written before this side pulls it — cleaned up after every attempt,
// success or failure, by MigrateRunOne itself.
const remoteMigrateStagingDir = "/tmp/ngxsetup-migrate"

// MigrateRunOne runs the full pipeline for one remote site: allocate local
// infrastructure, dump and transfer its database, restore it, transfer its
// files, then finish configuration — rolling the whole site back if any
// step fails, so a failure never leaves a half-registered, half-populated
// site behind. localStagingDir holds the database dump only as long as it
// takes to decompress and restore it; the caller owns creating and
// eventually removing that directory, since it likely holds more than one
// site's staging files across a multi-site migration run.
func (c *Ctx) MigrateRunOne(ctx context.Context, remote migrate.RemoteConfig, ds DiscoveredSite, opts MigrateSiteRequest, localStagingDir string, progress MigrateProgress) error {
	client := migrate.Client{Cfg: remote, Log: progress.Log}

	progress.Step("allocating local site")
	rec, err := c.MigrateAllocateSite(MigrateSiteRequest{Domain: ds.Domain, Aliases: ds.Aliases, TLS: opts.TLS, SelfSigned: opts.SelfSigned})
	if err != nil {
		return fmt.Errorf("allocating local site: %w", err)
	}
	fail := func(step string, err error) error {
		c.MigrateAbortSite(rec)
		return fmt.Errorf("%s: %w", step, err)
	}

	remoteDump := fmt.Sprintf("%s/%s.sql.gz", remoteMigrateStagingDir, rec.Slug)
	progress.Step("dumping the remote database")
	if err := client.DumpDatabase(ctx, ds.DBInfo, remoteDump); err != nil {
		return fail("dumping the remote database", err)
	}
	defer client.RemoveRemotePath(ctx, remoteDump)

	localDumpGz := filepath.Join(localStagingDir, rec.Slug+".sql.gz")
	progress.Step("transferring the database dump")
	if err := client.RsyncPull(ctx, migrate.DefaultMaxAttempts, remoteDump, localDumpGz, nil, progressAdapter(progress)); err != nil {
		return fail("transferring the database dump", err)
	}

	localDumpSQL := strings.TrimSuffix(localDumpGz, ".gz")
	progress.Step("decompressing the database dump")
	if err := DecompressGzip(localDumpGz, localDumpSQL); err != nil {
		return fail("decompressing the database dump", err)
	}
	_ = os.Remove(localDumpGz)

	progress.Step("restoring the database")
	password, err := c.MigrateProvisionDatabase(rec, localDumpSQL)
	_ = os.Remove(localDumpSQL)
	if err != nil {
		return fail("restoring the database", err)
	}

	progress.Step("transferring site files")
	remoteRoot, localRoot := ds.Root, c.Path(rec.Root)
	if !strings.HasSuffix(remoteRoot, "/") {
		remoteRoot += "/"
	}
	if !strings.HasSuffix(localRoot, "/") {
		localRoot += "/"
	}
	if err := client.RsyncPull(ctx, migrate.DefaultMaxAttempts, remoteRoot, localRoot, []string{"wp-config.php"}, progressAdapter(progress)); err != nil {
		return fail("transferring site files", err)
	}

	progress.Step("configuring nginx and PHP-FPM")
	if err := c.MigrateFinishSite(rec, ds.DBInfo, password, MigrateSiteRequest{
		Domain: ds.Domain, Aliases: ds.Aliases, TLS: opts.TLS, SelfSigned: opts.SelfSigned,
	}); err != nil {
		return fail("configuring nginx and PHP-FPM", err)
	}

	// Last, because it needs the site's actual final local path — and
	// because a caching plugin's hardcoded old path is a real but minor
	// problem compared to everything above it: worth fixing, not worth
	// rolling the whole migration back over if this one step somehow
	// failed on an otherwise fully working site.
	progress.Step("checking for hardcoded local paths")
	if n, err := c.MigrateFixHardcodedPaths(rec.Root, ds.Root, rec.Root); err != nil {
		progress.Log(fmt.Sprintf("could not finish checking for hardcoded paths: %v (the site is otherwise fully migrated)", err))
	} else if n > 0 {
		progress.Log(fmt.Sprintf("rewrote the old absolute path (%s) to this site's local path (%s) in %d file(s) — WP Super Cache's advanced-cache.php and similar plugin drop-ins are the usual reason", ds.Root, rec.Root, n))
	} else {
		progress.Log("no hardcoded references to the old server path were found")
	}
	return nil
}

func progressAdapter(p MigrateProgress) func(migrate.TransferProgress) {
	return func(tp migrate.TransferProgress) {
		if tp.Percent >= 0 {
			p.Percent(tp.Percent)
		}
	}
}

// DecompressGzip writes the ungzipped contents of src to dst — the local,
// pure counterpart to the remote gzip DumpDatabase applies before transfer,
// since db.Client.Restore expects plain SQL and the whole point of
// compressing for the network hop was to make it smaller in flight, not to
// keep it compressed at rest.
func DecompressGzip(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	gz, err := gzip.NewReader(in)
	if err != nil {
		return fmt.Errorf("%s does not look like a gzip-compressed dump: %w", src, err)
	}
	defer gz.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, gz); err != nil {
		return fmt.Errorf("decompressing %s: %w", src, err)
	}
	return nil
}
