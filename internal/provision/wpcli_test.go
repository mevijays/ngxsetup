package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ngxsetup/internal/state"
)

func wpTestSite(c *Ctx) state.Site {
	return state.Site{Slug: "blog", Domain: "blog.example", User: "web-blog", Root: c.DocumentRoot("blog"), WordPress: true}
}

// wp-cli runs the site's own PHP, so it must get the site's own account and
// the same confinement as its PHP-FPM service — never root, never a view of
// another site's files.
func TestWPCommandRunsAsTheSiteInsideItsSandbox(t *testing.T) {
	c := testCtx(t)
	rec := wpTestSite(c)
	name, argv := c.WPCommand(rec, false, []string{"plugin", "list", "--format=json"})
	if name != "systemd-run" {
		t.Fatalf("command = %s, want systemd-run", name)
	}
	joined := strings.Join(argv, "\n")
	for _, want := range []string{
		"--uid=web-blog", "--gid=web-blog", "--pipe", "--wait",
		"--property=TemporaryFileSystem=" + WebRoot + ":ro",
		"--property=BindPaths=" + c.SiteRoot("blog"),
		"--property=ProtectSystem=strict",
		"--property=ReadWritePaths=" + c.SiteRoot("blog"),
		"--property=NoNewPrivileges=yes",
		"--setenv=HOME=" + c.SiteRoot("blog"),
	} {
		if !strings.Contains(joined, want+"\n") && !strings.HasSuffix(joined, want) {
			t.Errorf("argv is missing %q:\n%s", want, joined)
		}
	}
	// wp-cli and its arguments come last, after the separator, unchanged.
	sep := -1
	for i, a := range argv {
		if a == "--" {
			sep = i
		}
	}
	tail := argv[sep+1:]
	want := []string{wpCLIPath, "--path=" + rec.Root, "plugin", "list", "--format=json"}
	if strings.Join(tail, " ") != strings.Join(want, " ") {
		t.Errorf("command after -- = %v, want %v", tail, want)
	}
}

// systemd expands "${VAR}" in a service's command line and drops an argument
// that starts with "$" when no such variable is set — found running
// `wp eval '$u = wp_upload_dir(); …'`, which reached wp-cli with no code.
// Every "$" must be passed as systemd's "$$" escape.
func TestWPCommandEscapesDollarSigns(t *testing.T) {
	c := testCtx(t)
	_, argv := c.WPCommand(wpTestSite(c), false, []string{"eval", `$u = 1; echo "${u}";`, "100%"})
	tail := argv[len(argv)-2:]
	if tail[0] != `$$u = 1; echo "$${u}";` || tail[1] != "100%" {
		t.Errorf("escaped args = %q", tail)
	}
}

func TestWPCommandUsesAPTYOnlyWhenInteractive(t *testing.T) {
	c := testCtx(t)
	_, argv := c.WPCommand(wpTestSite(c), true, []string{"shell"})
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--pty") || strings.Contains(joined, "--pipe") {
		t.Errorf("interactive argv = %s, want --pty and no --pipe", joined)
	}
}

func TestWPSiteRequiresWordPressAndWPCLI(t *testing.T) {
	c := testCtx(t)
	c.State.Upsert(wpTestSite(c))
	c.State.Upsert(state.Site{Slug: "plain", Domain: "plain.example", Root: c.DocumentRoot("plain")})

	if _, err := c.WPSite("blog.example"); err == nil || !strings.Contains(err.Error(), "wp-cli is not installed") {
		t.Errorf("WPSite without wp-cli = %v, want a not-installed error", err)
	}
	if err := os.MkdirAll(filepath.Dir(c.Path(wpCLIPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.Path(wpCLIPath), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WPSite("blog.example"); err != nil {
		t.Errorf("WPSite for a WordPress site = %v", err)
	}
	if _, err := c.WPSite("plain.example"); err == nil {
		t.Error("WPSite accepted a plain vhost")
	}
	if _, err := c.WPSite("missing.example"); err == nil {
		t.Error("WPSite accepted an unknown site")
	}
}
