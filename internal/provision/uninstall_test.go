package provision

import (
	"strings"
	"testing"

	"ngxsetup/internal/state"
)

// The default plan must disconnect sites from nginx/PHP without listing them
// as data to be deleted — this is the property the whole "nothing destroyed
// without being asked" design rests on, so it gets its own explicit test
// rather than just being implied by the other cases.
func TestPlanUninstallDefaultKeepsSiteData(t *testing.T) {
	c := testCtx(t)
	c.State.Upsert(state.Site{Slug: "example-com", Domain: "example.com", DBName: "example_db"})

	plan := c.PlanUninstall(UninstallOptions{})
	if len(plan.SitesPurged) != 0 {
		t.Errorf("SitesPurged = %v, want empty when PurgeSites is not set", plan.SitesPurged)
	}
	if len(plan.SitesDisconnected) != 1 || plan.SitesDisconnected[0] != "example.com" {
		t.Errorf("SitesDisconnected = %v, want [example.com]", plan.SitesDisconnected)
	}
	if len(plan.PackagesRemoved) != 0 {
		t.Errorf("PackagesRemoved = %v, want empty when PurgePackages is not set", plan.PackagesRemoved)
	}
}

func TestPlanUninstallPurgeSites(t *testing.T) {
	c := testCtx(t)
	c.State.Upsert(state.Site{Slug: "a-com", Domain: "a.com"})
	c.State.Upsert(state.Site{Slug: "b-com", Domain: "b.com"})

	plan := c.PlanUninstall(UninstallOptions{PurgeSites: true})
	if len(plan.SitesDisconnected) != 0 {
		t.Errorf("SitesDisconnected = %v, want empty when every site is purged instead", plan.SitesDisconnected)
	}
	if len(plan.SitesPurged) != 2 {
		t.Errorf("SitesPurged = %v, want both sites", plan.SitesPurged)
	}
}

func TestPlanUninstallPurgePackagesListsTheStack(t *testing.T) {
	c := testCtx(t)
	plan := c.PlanUninstall(UninstallOptions{PurgePackages: true})
	if len(plan.PackagesRemoved) == 0 {
		t.Fatal("expected a package list when PurgePackages is set")
	}
	for _, want := range []string{"nginx", "mariadb-server", "fail2ban"} {
		found := false
		for _, p := range plan.PackagesRemoved {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("PackagesRemoved = %v, missing %q", plan.PackagesRemoved, want)
		}
	}
}

// The config path list must actually name real, specific paths — not just
// be non-empty — so a Describe()'d confirmation prompt tells an operator
// something concrete before they approve it.
func TestPlanUninstallConfigPathsAreSpecific(t *testing.T) {
	c := testCtx(t)
	plan := c.PlanUninstall(UninstallOptions{})
	if len(plan.ConfigPaths) < 10 {
		t.Errorf("only %d config paths listed; expected the full managed-file set", len(plan.ConfigPaths))
	}
	joined := strings.Join(plan.ConfigPaths, "\n")
	for _, want := range []string{"nginx.service.d", "fail2ban", ConfigDir, StateDir} {
		if !strings.Contains(joined, want) {
			t.Errorf("ConfigPaths missing something referencing %q:\n%s", want, joined)
		}
	}
}

// This is the property a destructive command's confirmation prompt lives or
// dies on: a plain "disconnect" must read differently from a "PERMANENTLY
// DELETE" so an operator skimming the list cannot mistake one for the other.
func TestUninstallPlanDescribeDistinguishesDisconnectFromPurge(t *testing.T) {
	c := testCtx(t)
	c.State.Upsert(state.Site{Slug: "example-com", Domain: "example.com"})

	disconnected := c.PlanUninstall(UninstallOptions{}).Describe()
	purged := c.PlanUninstall(UninstallOptions{PurgeSites: true}).Describe()

	joinedDisc := strings.Join(disconnected, "\n")
	joinedPurge := strings.Join(purged, "\n")

	if !strings.Contains(joinedDisc, "KEPT") {
		t.Errorf("default plan description should say data is kept:\n%s", joinedDisc)
	}
	if strings.Contains(joinedDisc, "DELETE") {
		t.Errorf("default plan description should not threaten deletion:\n%s", joinedDisc)
	}
	if !strings.Contains(joinedPurge, "DELETE") {
		t.Errorf("--purge-sites plan description should say so plainly:\n%s", joinedPurge)
	}
}

func TestPlanUninstallRestoresOverwrittenPackages(t *testing.T) {
	c := testCtx(t)
	plan := c.PlanUninstall(UninstallOptions{})
	if len(plan.RestoredFiles) != 2 {
		t.Fatalf("RestoredFiles = %v, want exactly the nginx.conf and php-fpm www.conf entries ngxsetup overwrote", plan.RestoredFiles)
	}
	for _, f := range plan.RestoredFiles {
		if f.Package == "" || f.PathInPackage == "" || f.LivePath == "" {
			t.Errorf("incomplete RestoredFile: %+v", f)
		}
		if !strings.HasPrefix(f.PathInPackage, "./") {
			t.Errorf("PathInPackage %q must be relative to the archive root (dpkg-deb --fsys-tarfile's own convention)", f.PathInPackage)
		}
	}
}

// shellQuote feeds directly into a shell script; a package name or path
// containing a single quote must not be able to break out of the quoted
// string and inject additional shell commands.
func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"nginx-common":       `'nginx-common'`,
		"":                   `''`,
		"it's-a-test":        `'it'\''s-a-test'`,
		"a'; rm -rf /; echo": `'a'\''; rm -rf /; echo'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
