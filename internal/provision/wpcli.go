package provision

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ngxsetup/internal/state"
	"ngxsetup/internal/system"
)

// Running wp-cli for a site.
//
// wp-cli executes the site's own PHP — every plugin and theme it has — so it
// gets exactly the confinement the site's PHP-FPM service has: it runs as the
// site's own account, in a transient systemd unit with the same sandbox as
// ngxsetup-fpm@.service (other sites' directories hidden, the rest of the
// filesystem read-only, no privilege escalation). Running it as root, or
// even as the site user without the sandbox, would let one compromised
// plugin reach every other site on the box through a routine `wp plugin
// list`.
//
// One difference from the PHP-FPM service is deliberate: the whole site
// directory is writable, because installing and updating plugins, themes and
// core is precisely what wp-cli is for here (the generated wp-config.php
// sets DISALLOW_FILE_MODS, so wp-admin cannot).

// WPSite resolves a site wp-cli can be run against.
func (c *Ctx) WPSite(nameOrSlug string) (*state.Site, error) {
	rec, err := c.State.Find(nameOrSlug)
	if err != nil {
		return nil, err
	}
	if !rec.WordPress {
		return nil, fmt.Errorf("%s is a plain vhost, not a WordPress site", rec.Domain)
	}
	if _, err := os.Stat(c.Path(wpCLIPath)); err != nil {
		return nil, fmt.Errorf("wp-cli is not installed at %s; run `ngxsetup setup` to install it", wpCLIPath)
	}
	return rec, nil
}

// wpSandboxProperties mirrors ngxsetup-fpm@.service's confinement.
func (c *Ctx) wpSandboxProperties(rec state.Site) []string {
	base := c.SiteRoot(rec.Slug)
	return []string{
		"TemporaryFileSystem=" + WebRoot + ":ro",
		"BindPaths=" + base,
		"ProtectSystem=strict",
		"ReadWritePaths=" + base,
		"WorkingDirectory=" + rec.Root,
		// Files wp-cli creates are not world-readable and, inside the setgid
		// site directories, get the www-data group nginx reads by. WordPress's
		// own upgrader (plugin/theme/core install) chmods what it unpacks to
		// its FS_CHMOD_DIR/FS_CHMOD_FILE, 0755/0644 here — still readable by
		// nginx, and the site directory itself keeps other accounts out;
		// `site fix-perms` restores the canonical 2750/0640.
		"UMask=0027",
		"PrivateTmp=yes",
		"PrivateDevices=yes",
		"ProtectHome=yes",
		"ProtectProc=invisible",
		"ProtectKernelTunables=yes",
		"ProtectKernelModules=yes",
		"ProtectKernelLogs=yes",
		"ProtectControlGroups=yes",
		"ProtectHostname=yes",
		"ProtectClock=yes",
		"NoNewPrivileges=yes",
		"RestrictSUIDSGID=yes",
		"RestrictRealtime=yes",
		"RestrictNamespaces=yes",
		"RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
		"LockPersonality=yes",
		"SystemCallArchitectures=native",
		"SystemCallFilter=@system-service",
		"SystemCallErrorNumber=EPERM",
	}
}

// WPCommand returns the command line that runs wp-cli with args for a site.
// interactive allocates a pseudo-terminal (for `wp shell`, `wp db cli`, or
// anything that prompts); otherwise stdin, stdout and stderr are passed
// through as pipes, which is what scripting and captured output need.
func (c *Ctx) WPCommand(rec state.Site, interactive bool, args []string) (name string, argv []string) {
	base := c.SiteRoot(rec.Slug)
	argv = []string{"--quiet", "--collect", "--wait"}
	if interactive {
		argv = append(argv, "--pty")
	} else {
		argv = append(argv, "--pipe")
	}
	argv = append(argv,
		"--uid="+rec.User,
		"--gid="+rec.User,
		"--setenv=HOME="+base,
		"--setenv=WP_CLI_CACHE_DIR="+filepath.Join(base, ".wp-cli", "cache"),
	)
	if interactive {
		if term := os.Getenv("TERM"); term != "" {
			argv = append(argv, "--setenv=TERM="+term)
		}
	} else {
		// wp-cli pages long help through less when it thinks a person is
		// reading; with no terminal that would hang waiting for input.
		argv = append(argv, "--setenv=PAGER=cat")
	}
	for _, p := range c.wpSandboxProperties(rec) {
		argv = append(argv, "--property="+p)
	}
	argv = append(argv, "--", wpCLIPath, "--path="+rec.Root)
	for _, a := range args {
		argv = append(argv, escapeUnitEnv(a))
	}
	return "systemd-run", argv
}

// escapeUnitEnv protects one argument from the environment-variable
// expansion systemd applies to a service's command line, as it does to
// ExecStart=: "${VAR}" anywhere is substituted, and an argument starting with
// "$" is taken as a "$VAR" word and dropped when that variable is unset. Left
// alone, `wp eval '$x = 1;'` would reach wp-cli with no code at all and
// `search-replace '${old}' new` would search for an empty string. "$$" is
// systemd's escape for a literal "$", on every version.
func escapeUnitEnv(s string) string { return strings.ReplaceAll(s, "$", "$$") }

// RunWP runs wp-cli non-interactively and returns its stdout and stderr
// merged — the transcript a person reads. stdin is fed to the command, which
// is how a secret reaches it (`--prompt=user_pass`) without ever appearing
// in a process listing.
func (c *Ctx) RunWP(ctx context.Context, rec state.Site, stdin string, args ...string) (string, error) {
	name, argv := c.WPCommand(rec, false, args)
	return c.Runner.RunStdinCombined(ctx, stdin, name, argv...)
}

// QueryWP runs a read-only wp-cli command and returns stdout alone, for
// output that is parsed (--format=json) and must not have warnings mixed in.
// Unlike RunWP it executes under --dry-run too, the same as every other
// read-only inspection this tool performs.
func (c *Ctx) QueryWP(ctx context.Context, rec state.Site, args ...string) (string, error) {
	name, argv := c.WPCommand(rec, false, args)
	r := c.Runner
	r.DryRun = false
	return r.Output(ctx, name, argv...)
}

// GenerateWPPassword returns a new random password for a WordPress account.
func GenerateWPPassword() (string, error) { return system.Password(24) }
