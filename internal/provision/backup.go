package provision

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ngxsetup/internal/db"
	"ngxsetup/internal/logx"
	"ngxsetup/internal/migrate"
	"ngxsetup/internal/state"
)

// DefaultBackupDir is where a database backup lands when the operator does
// not specify one. Root-only (0700), matching CredentialsDir — a database
// dump is at least as sensitive as the credentials that unlock it.
const DefaultBackupDir = "/var/backups/ngxsetup/db"

// BackupResult records the outcome of dumping one site's database.
type BackupResult struct {
	Domain string
	DBName string
	Path   string
	SizeMB float64
	Err    error
}

// BackupDatabase dumps one site's database to a timestamped .sql file in
// outDir ("" uses DefaultBackupDir).
//
// This is read-only against the database — a logical dump via
// --single-transaction takes a consistent snapshot without blocking writers
// — so unlike most of what this package does, there is nothing to validate
// or roll back afterward: either the file was written or it was not.
func (c *Ctx) BackupDatabase(nameOrSlug, outDir string) (*BackupResult, error) {
	site, err := c.State.Find(nameOrSlug)
	if err != nil {
		return nil, err
	}
	if site.DBName == "" {
		return nil, fmt.Errorf("%s has no database (a plain vhost, not a WordPress site)", site.Domain)
	}
	return c.backupOne(*site, orDefaultDir(outDir))
}

// BackupAllDatabases dumps every registered site's database — the "one
// click, everything" case. A failure on one site's dump is recorded on that
// site's BackupResult.Err rather than aborting the batch, so one locked
// table or a transient permission problem does not cost every other site its
// backup for the run.
func (c *Ctx) BackupAllDatabases(outDir string) ([]BackupResult, error) {
	dir := orDefaultDir(outDir)
	var results []BackupResult
	for _, site := range c.State.Sites {
		if site.DBName == "" {
			continue // a plain vhost has nothing to back up
		}
		result, err := c.backupOne(site, dir)
		if err != nil {
			results = append(results, BackupResult{Domain: site.Domain, DBName: site.DBName, Err: err})
			continue
		}
		results = append(results, *result)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("no site has a database to back up")
	}
	return results, nil
}

// RestoreResult records the outcome of loading a dump into one site's
// database.
type RestoreResult struct {
	Domain       string
	DBName       string
	SafetyBackup string // path of the pre-restore backup, "" if skipped
}

// EnsureSiteDatabase makes sure a site's database schema exists, recreating
// it if it does not — the recovery path for an accidental `DROP DATABASE`,
// as distinct from a dropped table or deleted rows, which never leave the
// schema itself missing. db.Client.Restore only ever replaces a schema's
// contents; it does not create one (see its own doc comment), so without
// this step restoring after a full `DROP DATABASE` would fail with
// "Unknown database" before a single row loaded — this is what makes
// `db restore` / `borg restore-db` a genuine one-command recovery in that
// case too, rather than requiring the operator to recreate the schema by
// hand first.
//
// The database user account almost always survives a `DROP DATABASE` —
// both MySQL and MariaDB leave a granted account's privileges in place even
// after the schema they were granted on disappears — so the common case is
// just recreating the empty schema and re-issuing its grant, via
// db.Client.EnsureSchemaAndGrant, which never touches the account's
// password. If the account is gone too (an unusual combination — it means
// something beyond a plain `DROP DATABASE` also happened), a password is
// needed to recreate it, and the one place that password still lives is
// wp-config.php on disk, since a database-only accident never touches the
// file tree. That is read as a last resort, via the same wp-config.php
// parser the migration feature uses, so the recreated account's password
// matches what the site's files already expect and wp-config.php itself
// never needs rewriting.
func (c *Ctx) EnsureSiteDatabase(site state.Site) error {
	if site.DBName == "" {
		return nil // a plain vhost, not a WordPress site — nothing to ensure
	}
	client := c.DBClient()
	exists, err := client.DatabaseExists(c.Context, site.DBName)
	if err != nil {
		return fmt.Errorf("checking whether database %s exists: %w", site.DBName, err)
	}
	if exists {
		return nil
	}

	const host = "localhost"
	userExists, err := client.UserExists(c.Context, site.DBUser, host)
	if err != nil {
		return fmt.Errorf("checking whether database user %s exists: %w", site.DBUser, err)
	}

	if userExists {
		logx.Warn("database %s is missing; recreating the schema and its grant for the existing account before continuing", site.DBName)
		return client.EnsureSchemaAndGrant(c.Context, site.DBName, site.DBUser, host, c.Plan.DB.Collation)
	}

	logx.Warn("database %s and its user account are both missing; recovering the account's password from wp-config.php before continuing", site.DBName)
	wpConfigPath := c.Path(filepath.Join(site.Root, "wp-config.php"))
	raw, err := os.ReadFile(wpConfigPath)
	if err != nil {
		return fmt.Errorf("database %s and its user account are both gone, and wp-config.php could not be read to recover a password (%w) — restore the site's files first, then retry", site.DBName, err)
	}
	info, ok := migrate.ParseWPConfig(string(raw))
	if !ok || info.DBPassword == "" {
		return fmt.Errorf("database %s and its user account are both gone, and wp-config.php did not contain a usable password — the account needs to be recreated by hand before this site can be restored", site.DBName)
	}
	return client.Provision(c.Context, site.DBName, site.DBUser, info.DBPassword, host, c.Plan.DB.Collation)
}

// RestoreDatabase loads a .sql file into one site's database, overwriting its
// current contents.
//
// Unlike backup, this is destructive and irreversible by anything ngxsetup
// itself does — which is why, unless the caller opts out, it takes a safety
// backup of the database's current contents first. That backup is not a
// substitute for the caller confirming the action (the CLI does, before this
// is ever reached); it exists so that "restore the wrong file" is a mistake
// recoverable in one more command rather than a permanent one.
func (c *Ctx) RestoreDatabase(nameOrSlug, sqlPath string, skipSafetyBackup bool) (*RestoreResult, error) {
	site, err := c.State.Find(nameOrSlug)
	if err != nil {
		return nil, err
	}
	if site.DBName == "" {
		return nil, fmt.Errorf("%s has no database (a plain vhost, not a WordPress site)", site.Domain)
	}
	if _, err := os.Stat(sqlPath); err != nil {
		return nil, fmt.Errorf("reading %s: %w", sqlPath, err)
	}

	if !c.Writer.DryRun {
		if err := c.EnsureSiteDatabase(*site); err != nil {
			return nil, err
		}
	}

	result := &RestoreResult{Domain: site.Domain, DBName: site.DBName}

	if !skipSafetyBackup && !c.Writer.DryRun {
		safety, err := c.backupOne(*site, DefaultBackupDir)
		if err != nil {
			return nil, fmt.Errorf("safety backup before restore: %w (restore aborted; use --no-safety-backup to skip this step)", err)
		}
		result.SafetyBackup = safety.Path
	}

	if c.Writer.DryRun {
		logx.Change("[dry-run] would restore %s into %s from %s", sqlPath, site.DBName, sqlPath)
		return result, nil
	}

	logx.Step("restoring %s (%s) from %s", site.Domain, site.DBName, sqlPath)
	if err := c.DBClient().Restore(c.Context, site.DBName, sqlPath); err != nil {
		return nil, err
	}
	logx.Change("restored %s from %s", site.Domain, sqlPath)
	return result, nil
}

func (c *Ctx) backupOne(site state.Site, dir string) (*BackupResult, error) {
	if err := c.Writer.EnsureDir(dir, 0o700, ""); err != nil {
		return nil, err
	}
	filename := fmt.Sprintf("%s-%s.sql", site.Slug, time.Now().UTC().Format("20060102-150405"))
	path := c.Path(filepath.Join(dir, filename))

	if c.Writer.DryRun {
		// Dump() shells out to mysqldump and writes the file itself — unlike
		// most of this package's mutations, it does not go through
		// render.Writer, so DryRun has to be handled explicitly here rather
		// than by a Writer method that already knows how to no-op. Validating
		// the identifier here too (Dump would reject it the same way) is
		// what keeps a dry-run preview honest: without this, --dry-run would
		// report success for a site whose real backup fails immediately.
		if err := db.ValidateIdentifier(site.DBName); err != nil {
			return nil, err
		}
		logx.Change("[dry-run] would back up %s to %s", site.DBName, path)
		return &BackupResult{Domain: site.Domain, DBName: site.DBName, Path: path}, nil
	}

	logx.Step("backing up %s (%s)", site.Domain, site.DBName)
	if err := c.DBClient().Dump(c.Context, site.DBName, path); err != nil {
		return nil, err
	}

	info, err := os.Stat(path)
	sizeMB := 0.0
	if err == nil {
		sizeMB = float64(info.Size()) / (1 << 20)
	}
	logx.Change("backed up %s to %s (%.1f MB)", site.Domain, path, sizeMB)
	return &BackupResult{Domain: site.Domain, DBName: site.DBName, Path: path, SizeMB: sizeMB}, nil
}

// BackupFile describes one dump already on disk.
type BackupFile struct {
	Name    string  `json:"name"`
	Path    string  `json:"path"`
	SizeMB  float64 `json:"size_mb"`
	ModTime string  `json:"mod_time"`
}

// ListBackups returns every .sql file in dir ("" uses DefaultBackupDir),
// newest first — what the web UI's backup/restore page and `db restore`'s
// file picker both need: "what do I already have to restore from."
func (c *Ctx) ListBackups(dir string) ([]BackupFile, error) {
	dir = orDefaultDir(dir)
	entries, err := os.ReadDir(c.Path(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []BackupFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupFile{
			Name:    e.Name(),
			Path:    filepath.Join(dir, e.Name()),
			SizeMB:  float64(info.Size()) / (1 << 20),
			ModTime: info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime > out[j].ModTime })
	return out, nil
}

func orDefaultDir(dir string) string {
	if dir == "" {
		return DefaultBackupDir
	}
	return dir
}
