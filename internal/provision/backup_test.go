package provision

import (
	"strings"
	"testing"

	"ngxsetup/internal/state"
)

func TestBackupDatabaseUnknownSite(t *testing.T) {
	c := testCtx(t)
	if _, err := c.BackupDatabase("does-not-exist.com", ""); err == nil {
		t.Error("expected an error for an unregistered site")
	}
}

func TestBackupDatabasePlainVhostHasNoDatabase(t *testing.T) {
	c := testCtx(t)
	c.State.Upsert(state.Site{Slug: "plain-com", Domain: "plain.com"}) // no DBName: not a WordPress site
	if _, err := c.BackupDatabase("plain.com", ""); err == nil {
		t.Error("expected an error backing up a site with no database")
	}
}

// EnsureSiteDatabase must not touch the database at all for a plain vhost —
// there is no schema to check for, let alone recreate.
func TestEnsureSiteDatabasePlainVhostIsNoOp(t *testing.T) {
	c := testCtx(t)
	site := state.Site{Slug: "plain-com", Domain: "plain.com"} // no DBName
	if err := c.EnsureSiteDatabase(site); err != nil {
		t.Errorf("EnsureSiteDatabase on a plain vhost should be a no-op, got: %v", err)
	}
}

// RestoreDatabase's early guards (unknown site, plain vhost, missing dump
// file) must all fire before anything reaches EnsureSiteDatabase or the
// database client — same ordering RestoreDatabase already documented for
// the safety backup.
func TestRestoreDatabaseUnknownSite(t *testing.T) {
	c := testCtx(t)
	if _, err := c.RestoreDatabase("does-not-exist.com", "/no/such/file.sql", true); err == nil {
		t.Error("expected an error for an unregistered site")
	}
}

func TestRestoreDatabasePlainVhostHasNoDatabase(t *testing.T) {
	c := testCtx(t)
	c.State.Upsert(state.Site{Slug: "plain-com", Domain: "plain.com"})
	if _, err := c.RestoreDatabase("plain.com", "/no/such/file.sql", true); err == nil {
		t.Error("expected an error restoring into a site with no database")
	}
}

func TestRestoreDatabaseRejectsMissingDumpFile(t *testing.T) {
	c := testCtx(t)
	c.State.Upsert(state.Site{Slug: "wp-com", Domain: "wp.com", DBName: "wp_db", DBUser: "wp_user"})
	if _, err := c.RestoreDatabase("wp.com", "/no/such/dump-file-really-not-there.sql", true); err == nil {
		t.Error("expected an error for a dump file that does not exist")
	}
}

// dry-run must report what it would do without actually shelling out to
// mysqldump — Dump() has no dry-run awareness of its own (it always
// executes, being a real read against the database), so the guard has to
// live at this call site.
func TestBackupDatabaseDryRunDoesNotDump(t *testing.T) {
	c := testCtx(t)
	c.Writer.DryRun = true
	c.State.Upsert(state.Site{Slug: "example-com", Domain: "example.com", DBName: "example_db"})

	result, err := c.BackupDatabase("example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Path == "" {
		t.Error("expected a planned path even under dry-run, for the operator to see what would be written")
	}
	if exists(t, c, result.Path) {
		t.Error("a dry-run backup must not actually write the .sql file")
	}
}

func TestBackupAllDatabasesNoSites(t *testing.T) {
	c := testCtx(t)
	if _, err := c.BackupAllDatabases(""); err == nil {
		t.Error("expected an error when there is nothing to back up")
	}
}

func TestBackupAllDatabasesSkipsPlainVhosts(t *testing.T) {
	c := testCtx(t)
	c.State.Upsert(state.Site{Slug: "plain-com", Domain: "plain.com"}) // no database
	if _, err := c.BackupAllDatabases(""); err == nil {
		t.Error("a registry with only plain vhosts should still report nothing to back up")
	}
}

func TestBackupPathUsesDefaultDirWhenUnset(t *testing.T) {
	c := testCtx(t)
	c.Writer.DryRun = true
	c.State.Upsert(state.Site{Slug: "example-com", Domain: "example.com", DBName: "example_db"})

	result, err := c.BackupDatabase("example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Path, DefaultBackupDir) {
		t.Errorf("Path = %q, want it under the default backup directory %q", result.Path, DefaultBackupDir)
	}
}

func TestBackupPathHonoursCustomOutDir(t *testing.T) {
	c := testCtx(t)
	c.Writer.DryRun = true
	c.State.Upsert(state.Site{Slug: "example-com", Domain: "example.com", DBName: "example_db"})

	result, err := c.BackupDatabase("example.com", "/custom/backup/dir")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Path, "/custom/backup/dir") {
		t.Errorf("Path = %q, want it under the custom directory", result.Path)
	}
}

// One site's backup failing (a locked table, a transient permission issue)
// must not stop the rest of the batch from being attempted — an operator
// running the "one click, everything" backup should get every other site's
// dump even if one fails.
func TestBackupAllDatabasesContinuesAfterOneFailure(t *testing.T) {
	c := testCtx(t)
	c.Writer.DryRun = true
	// An invalid database name (would fail ValidateIdentifier inside Dump)
	// stands in for a site whose dump fails, without needing a real broken
	// database to reproduce that.
	c.State.Upsert(state.Site{Slug: "bad-com", Domain: "bad.com", DBName: "not a valid identifier"})
	c.State.Upsert(state.Site{Slug: "good-com", Domain: "good.com", DBName: "good_db"})

	results, err := c.BackupAllDatabases("")
	if err != nil {
		t.Fatalf("the batch itself should not fail just because one site's dump would: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (one per site, including the failed one)", len(results))
	}
	var goodOK, badFailed bool
	for _, r := range results {
		if r.Domain == "good.com" && r.Err == nil {
			goodOK = true
		}
		if r.Domain == "bad.com" && r.Err != nil {
			badFailed = true
		}
	}
	if !goodOK {
		t.Error("good.com's backup should have succeeded")
	}
	if !badFailed {
		t.Error("bad.com's backup should be recorded as failed, not silently dropped")
	}
}
