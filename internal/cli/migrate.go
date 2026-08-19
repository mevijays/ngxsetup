package cli

import (
	"context"
	"fmt"
	"os"

	"ngxsetup/internal/logx"
	"ngxsetup/internal/migrate"
	"ngxsetup/internal/provision"
)

func cmdMigrate(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ngxsetup migrate <discover|run> ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "discover":
		return cmdMigrateDiscover(ctx, rest)
	case "run":
		return cmdMigrateRun(ctx, rest)
	default:
		return fmt.Errorf("unknown migrate subcommand %q", sub)
	}
}

// migrateConnFlags registers the connection flags discover and run share.
func migrateConnFlags(fs interface {
	StringVar(*string, string, string, string)
}) (*string, *string, *string) {
	host := new(string)
	user := new(string)
	key := new(string)
	fs.StringVar(host, "host", "", "remote host, IP or hostname")
	fs.StringVar(user, "user", "root", "remote SSH user (root, or an account with passwordless sudo)")
	fs.StringVar(key, "key", "", "path to a private key file")
	return host, user, key
}

func cmdMigrateDiscover(ctx context.Context, args []string) error {
	fs := newFlagSet("migrate discover")
	host, user, key := migrateConnFlags(fs)
	port := fs.Int("port", 22, "remote SSH port")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	client, cleanup, err := buildMigrateClient(ctx, *host, *port, *user, *key)
	if err != nil {
		return err
	}
	defer cleanup()

	sites, err := discoverSites(ctx, client)
	if err != nil {
		return err
	}
	if len(sites) == 0 {
		logx.Info("no vhosts found on %s", *host)
		return nil
	}
	fmt.Printf("%-32s  %-8s  %-40s  %s\n", "DOMAIN", "WP?", "ROOT", "NOTE")
	for _, s := range sites {
		wp := "no"
		if s.wordpress {
			wp = "yes"
		}
		fmt.Printf("%-32s  %-8s  %-40s  %s\n", s.site.Domain, wp, s.site.Root, s.note)
	}
	return nil
}

func cmdMigrateRun(ctx context.Context, args []string) error {
	fs := newFlagSet("migrate run")
	host, user, key := migrateConnFlags(fs)
	port := fs.Int("port", 22, "remote SSH port")
	all := fs.Bool("all", false, "migrate every migratable site discovery finds, instead of naming them")
	tls := fs.Bool("tls", false, "request a Let's Encrypt certificate for each migrated site")
	selfSigned := fs.Bool("self-signed", false, "issue a self-signed certificate for each migrated site")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if !*all && len(pos) == 0 {
		return fmt.Errorf("usage: ngxsetup migrate run --host <host> --user <user> --key <path> <domain> [<domain>...] | --all")
	}
	if *tls && *selfSigned {
		return fmt.Errorf("--tls and --self-signed are mutually exclusive")
	}

	client, cleanup, err := buildMigrateClient(ctx, *host, *port, *user, *key)
	if err != nil {
		return err
	}
	defer cleanup()

	discovered, err := discoverSites(ctx, client)
	if err != nil {
		return err
	}
	byDomain := map[string]migratableSite{}
	for _, s := range discovered {
		byDomain[s.site.Domain] = s
	}

	var selected []migratableSite
	if *all {
		for _, s := range discovered {
			if s.wordpress {
				selected = append(selected, s)
			}
		}
	} else {
		for _, d := range pos {
			s, ok := byDomain[d]
			if !ok {
				return fmt.Errorf("%s was not found among the remote host's vhosts", d)
			}
			if !s.wordpress {
				return fmt.Errorf("%s has no readable wp-config.php and cannot be migrated: %s", d, s.note)
			}
			selected = append(selected, s)
		}
	}
	if len(selected) == 0 {
		return fmt.Errorf("no migratable sites selected")
	}

	logx.Section("About to migrate")
	for _, s := range selected {
		logx.Info("  %s  (%s)", s.site.Domain, s.site.Root)
	}
	if !*yes {
		if !confirm(fmt.Sprintf("Migrate %d site(s) from %s onto this machine?", len(selected), *host)) {
			logx.Info("cancelled")
			return nil
		}
	}

	pc, err := provision.New(ctx, provision.Options{})
	if err != nil {
		return err
	}
	stagingDir, err := os.MkdirTemp("", "ngxsetup-migrate-staging-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stagingDir)

	failed := 0
	for _, s := range selected {
		logx.Section("Migrating %s", s.site.Domain)
		remoteSite := provision.DiscoveredSite{Domain: s.site.Domain, Aliases: s.site.Aliases, Root: s.site.Root, DBInfo: s.dbInfo}
		req := provision.MigrateSiteRequest{Domain: s.site.Domain, Aliases: s.site.Aliases, TLS: *tls, SelfSigned: *selfSigned}
		if err := pc.MigrateRunOne(ctx, client.Cfg, remoteSite, req, stagingDir, cliMigrateProgress{}); err != nil {
			logx.Error("%s: %v", s.site.Domain, err)
			failed++
			continue
		}
		logx.Change("%s migrated successfully", s.site.Domain)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d site(s) failed to migrate — see above", failed, len(selected))
	}
	return nil
}

// migratableSite pairs one discovered vhost with the WordPress database
// information (if any) read from its wp-config.php, and a note on why it
// cannot be migrated when that information could not be read.
type migratableSite struct {
	site      migrate.NginxVHost
	dbInfo    migrate.WPConfigInfo
	wordpress bool
	note      string
}

func discoverSites(ctx context.Context, client *migrate.Client) ([]migratableSite, error) {
	logx.Step("connecting to %s@%s", client.Cfg.User, client.Cfg.Host)
	if err := client.TestConnection(ctx); err != nil {
		return nil, err
	}
	if err := client.EnsureRemoteTool(ctx, "rsync", "rsync"); err != nil {
		return nil, err
	}
	logx.Step("reading /etc/nginx/sites-enabled")
	vhosts, err := client.DiscoverVHosts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]migratableSite, 0, len(vhosts))
	for _, v := range vhosts {
		m := migratableSite{site: v}
		if v.Root == "" {
			m.note = "no root directive found for this vhost"
			out = append(out, m)
			continue
		}
		info, ok := client.ReadWPConfig(ctx, v.Root)
		if !ok {
			m.note = "wp-config.php not found (or not readable) at " + v.Root
			out = append(out, m)
			continue
		}
		m.dbInfo = info
		m.wordpress = true
		out = append(out, m)
	}
	return out, nil
}

// buildMigrateClient validates the connection flags and prepares an
// ephemeral known_hosts file for this invocation, cleaned up when the
// returned function is called.
func buildMigrateClient(ctx context.Context, host string, port int, user, keyPath string) (*migrate.Client, func(), error) {
	if host == "" {
		return nil, nil, fmt.Errorf("--host is required")
	}
	if keyPath == "" {
		return nil, nil, fmt.Errorf("--key is required (path to a private key file)")
	}
	if _, err := os.Stat(keyPath); err != nil {
		return nil, nil, fmt.Errorf("reading key file: %w", err)
	}
	tmpDir, err := os.MkdirTemp("", "ngxsetup-migrate-cli-*")
	if err != nil {
		return nil, nil, err
	}
	knownHosts := tmpDir + "/known_hosts"
	if err := os.WriteFile(knownHosts, nil, 0o600); err != nil {
		os.RemoveAll(tmpDir)
		return nil, nil, err
	}
	cfg := migrate.RemoteConfig{
		Host: host, Port: port, User: user,
		KeyPath: keyPath, KnownHostsPath: knownHosts,
		Sudo: user != "root",
	}
	client := &migrate.Client{Cfg: cfg, Log: func(s string) { logx.Info("  %s", s) }}
	return client, func() { os.RemoveAll(tmpDir) }, nil
}

// cliMigrateProgress implements provision.MigrateProgress with direct,
// blocking logx calls — the terminal is the "live status" for a CLI run,
// the same way it already is for every other long-running command.
type cliMigrateProgress struct{}

func (cliMigrateProgress) Step(step string) { logx.Step("%s", step) }
func (cliMigrateProgress) Percent(pct int)  { logx.Info("  %d%%", pct) }
func (cliMigrateProgress) Log(line string)  { logx.Info("  %s", line) }
