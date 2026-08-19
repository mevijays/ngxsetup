package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ngxsetup/internal/state"
)

// A dry-run uninstall must report what it would do without touching
// anything — the same contract every other mutating command in this tool
// honours.
func TestUninstallDryRunTouchesNothing(t *testing.T) {
	c := testCtx(t)
	// Apply for real first, so there is something on disk a dry-run
	// uninstall could (incorrectly) remove — a dry run against a machine
	// with nothing configured would trivially "change nothing."
	if err := c.ApplyNginx(); err != nil {
		t.Fatal(err)
	}
	c.Writer.Commit()
	before := read(t, c, "/etc/nginx/nginx.conf")

	c.Writer.DryRun = true
	if err := c.Uninstall(UninstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if !exists(t, c, "/etc/nginx/conf.d/00-ngxsetup-core.conf") {
		t.Error("a dry-run uninstall removed a managed file")
	}
	after := read(t, c, "/etc/nginx/nginx.conf")
	if before != after {
		t.Error("a dry-run uninstall modified nginx.conf")
	}
}

// A real uninstall must actually remove the managed files it listed in its
// own plan — the plan and the execution have to agree, or the confirmation
// prompt was lying about what would happen.
func TestUninstallRemovesConfigFiles(t *testing.T) {
	c := testCtx(t)
	if err := c.ApplyNginx(); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyPHP(); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyDB(); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplySystem(); err != nil {
		t.Fatal(err)
	}
	c.Writer.Commit()

	if !exists(t, c, "/etc/nginx/conf.d/00-ngxsetup-core.conf") {
		t.Fatal("test setup failed to produce the files uninstall is supposed to remove")
	}

	if err := c.Uninstall(UninstallOptions{}); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{
		"/etc/nginx/conf.d/00-ngxsetup-core.conf",
		"/etc/nginx/conf.d/30-ngxsetup-cache.conf",
		"/etc/nginx/snippets/ngxsetup",
		"/etc/sysctl.d/60-ngxsetup.conf",
		c.Plan.DB.ConfigPath,
		ConfigDir,
		StateDir,
	} {
		if exists(t, c, p) {
			t.Errorf("%s still exists after uninstall", p)
		}
	}
}

// Without --purge-sites, a site's document root and database must survive
// even though its nginx/PHP configuration is removed.
func TestUninstallWithoutPurgeSitesKeepsData(t *testing.T) {
	c := testCtx(t)
	if err := c.ApplyNginx(); err != nil {
		t.Fatal(err)
	}
	rec := state.Site{
		Slug: "example-com", Domain: "example.com",
		Root: c.DocumentRoot("example-com"), User: "web-example-com",
		SocketPath: c.SocketPath("example-com"), PHPVersion: "8.3", Enabled: true,
	}
	if err := c.writeSiteConfigs(rec); err != nil {
		t.Fatal(err)
	}
	c.Writer.Commit()
	c.State.Upsert(rec)

	// The site's "data" for this test: a marker file in its document root,
	// standing in for real WordPress content.
	marker := filepath.Join(c.Path(rec.Root), "wp-config.php")
	os.MkdirAll(filepath.Dir(marker), 0o755)
	os.WriteFile(marker, []byte("<?php // pretend site content\n"), 0o644)

	if err := c.Uninstall(UninstallOptions{}); err != nil {
		t.Fatal(err)
	}

	if exists(t, c, "/etc/nginx/sites-available/example-com.conf") {
		t.Error("the site's nginx config should have been removed")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("site data was deleted even though --purge-sites was not passed: %v", err)
	}
}

func TestUninstallSnapshotPreservesState(t *testing.T) {
	c := testCtx(t)
	c.State.Upsert(state.Site{Slug: "example-com", Domain: "example.com"})
	if err := c.State.Save(); err != nil {
		t.Fatal(err)
	}

	if err := c.snapshotBeforeUninstall(); err != nil {
		t.Fatal(err)
	}

	root := c.Path("/root")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "ngxsetup-uninstalled-") {
			continue
		}
		found = true
		data, err := os.ReadFile(filepath.Join(root, e.Name(), "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		if len(data) == 0 {
			t.Error("snapshotted state.json is empty")
		}
	}
	if !found {
		t.Errorf("no snapshot directory found under %v", entries)
	}
}
