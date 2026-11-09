package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ngxsetup/internal/migrate"
	"ngxsetup/internal/state"
)

// Every generated wp-config.php sets DISABLE_WP_CRON, so every WordPress
// site needs the timer that replaces it — including sites created before
// the timer was installed at all, which `tune --apply` must pick up.
func TestApplySystemInstallsWPCronTimersForWordPressSites(t *testing.T) {
	c := testCtx(t)
	c.State.Upsert(state.Site{Slug: "blog", Domain: "blog.example", User: "web-blog",
		Root: c.DocumentRoot("blog"), WordPress: true, Enabled: true})
	c.State.Upsert(state.Site{Slug: "plain", Domain: "plain.example", User: "web-plain",
		Root: c.DocumentRoot("plain"), Enabled: true})

	if err := c.ApplySystem(); err != nil {
		t.Fatal(err)
	}

	svc := read(t, c, wpCronServicePath("blog"))
	for _, want := range []string{"User=web-blog", "wp-cron.php", "ReadWritePaths=" + c.SiteRoot("blog")} {
		if !strings.Contains(svc, want) {
			t.Errorf("wp-cron service is missing %q:\n%s", want, svc)
		}
	}
	if timer := read(t, c, wpCronTimerPath("blog")); !strings.Contains(timer, "OnUnitActiveSec="+wpCronInterval) {
		t.Errorf("wp-cron timer has no interval:\n%s", timer)
	}
	if exists(t, c, wpCronServicePath("plain")) || exists(t, c, wpCronTimerPath("plain")) {
		t.Error("a non-WordPress vhost got a WordPress scheduler")
	}
}

func TestWPCronPrefersWPCLIWhenInstalled(t *testing.T) {
	c := testCtx(t)
	if err := os.MkdirAll(filepath.Dir(c.Path(wpCLIPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.Path(wpCLIPath), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := state.Site{Slug: "blog", Domain: "blog.example", User: "web-blog", Root: c.DocumentRoot("blog"), WordPress: true}
	if err := c.WriteWPCronUnits(rec); err != nil {
		t.Fatal(err)
	}
	svc := read(t, c, wpCronServicePath("blog"))
	if !strings.Contains(svc, "ExecStart="+wpCLIPath+" cron event run --due-now") {
		t.Errorf("wp-cli is installed but not used:\n%s", svc)
	}
	if !strings.Contains(svc, "ExecCondition="+wpCLIPath+" core is-installed") {
		t.Errorf("runs are not skipped before WordPress is installed:\n%s", svc)
	}
}

func TestCertbotReloadHookIsInstalledForLetsEncryptSites(t *testing.T) {
	c := testCtx(t)
	c.State.Upsert(state.Site{Slug: "s", Domain: "s.example", Root: c.DocumentRoot("s"),
		TLS: true, CertSource: "letsencrypt", CertPath: "/etc/letsencrypt/live/s.example/fullchain.pem", Enabled: true})
	if err := c.ApplyNginx(); err != nil {
		t.Fatal(err)
	}
	body := read(t, c, CertbotReloadHook)
	if !strings.HasPrefix(body, "#!/bin/sh\n") || !strings.Contains(body, "systemctl reload nginx") {
		t.Errorf("reload hook content:\n%s", body)
	}
	if info, err := os.Stat(c.Path(CertbotReloadHook)); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("reload hook is not executable: %v %v", info, err)
	}
}

func TestClaimAliasesRefusesNamesAnotherSiteServes(t *testing.T) {
	c := testCtx(t)
	c.State.Upsert(state.Site{Slug: "one", Domain: "one.example", Aliases: []string{"www.one.example", "shop.example"}})

	if _, err := c.claimAliases("two.example", []string{"shop.example"}, ""); err == nil {
		t.Error("an explicitly requested alias another site serves was accepted")
	}

	// The automatic www. alias is dropped, not fatal, when it is taken.
	c.State.Upsert(state.Site{Slug: "three", Domain: "three.example", Aliases: []string{"www.four.example"}})
	got, err := c.claimAliases("four.example", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if a == "www.four.example" {
			t.Error("the automatic www. alias was kept although another site serves it")
		}
	}

	got, err = c.claimAliases("five.example", []string{"cdn.five.example"}, "")
	if err != nil || len(got) != 2 {
		t.Errorf("free aliases = %v, %v; want www.five.example and cdn.five.example", got, err)
	}
}

// A wp-config.php left in the document root by an earlier install would be
// kept (it is written only if absent) while a new, empty database was
// provisioned — the site and the registry would then disagree on which
// database is the site's. AddSite must refuse instead.
func TestAddSiteRefusesALeftoverWPConfig(t *testing.T) {
	c := testCtx(t)
	root := c.Path(c.DocumentRoot("example-com"))
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wp-config.php"), []byte("<?php // old"), 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := c.AddSite(SiteRequest{Domain: "example.com", WordPress: true})
	if err == nil || !strings.Contains(err.Error(), "wp-config.php") {
		t.Fatalf("AddSite over a leftover wp-config.php = %v, want a refusal naming it", err)
	}
	if c.State.Count() != 0 {
		t.Error("the refused site was registered anyway")
	}
}

func TestMigratedWPConfigKeepsTheSourcesCustomDefines(t *testing.T) {
	c := testCtx(t)
	rec := &state.Site{Slug: "blog", Domain: "blog.example", User: "web-blog", Root: c.DocumentRoot("blog"),
		DBName: "wp_blog_ab12", DBUser: "wp_blog_ab12", WordPress: true}
	if err := os.MkdirAll(c.Path(rec.Root), 0o755); err != nil {
		t.Fatal(err)
	}
	info := migrate.WPConfigInfo{TablePrefix: "wp_", ExtraDefines: []string{
		"define( 'WP_CACHE', true );", "define( 'MULTISITE', true );",
	}}
	if err := c.MigrateFinishSite(rec, info, "a-long-generated-password", MigrateSiteRequest{Domain: rec.Domain}); err != nil {
		t.Fatal(err)
	}
	cfg := read(t, c, filepath.Join(rec.Root, "wp-config.php"))
	for _, d := range info.ExtraDefines {
		if !strings.Contains(cfg, d) {
			t.Errorf("migrated wp-config.php lost %q", d)
		}
	}
	if strings.Index(cfg, "MULTISITE") > strings.Index(cfg, "wp-settings.php") {
		t.Error("carried-over defines must come before wp-settings.php is loaded")
	}
}
