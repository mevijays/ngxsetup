// Package provision composes everything else into the operations the CLI
// exposes: bringing up a stack, applying tuning, and managing sites.
//
// The unit of work is a transaction. Files are written, the affected services
// are asked to validate their new configuration, and only then is the change
// committed. A validation failure restores every file this command touched, so
// a bad apply leaves a running server rather than a broken one.
package provision

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"ngxsetup/internal/config"
	"ngxsetup/internal/facts"
	"ngxsetup/internal/logx"
	"ngxsetup/internal/render"
	"ngxsetup/internal/state"
	"ngxsetup/internal/system"
	"ngxsetup/internal/tuning"
)

// Filesystem locations owned by this tool.
const (
	ConfigDir  = "/etc/ngxsetup"
	StateDir   = "/var/lib/ngxsetup"
	BackupRoot = "/var/lib/ngxsetup/backups"
	WebRoot    = "/var/www"
	ACMERoot   = "/var/www/_acme"
	CacheDir   = "/var/cache/nginx/fastcgi"
	OpcacheDir = "/var/cache/php/opcache"
	PHPLogDir  = "/var/log/php"
	// DBLogDir must match the path literals db/server.cnf.tmpl hardcodes for
	// slow_query_log_file and log_bin — Ubuntu's mariadb-server package does
	// not create this directory itself (it defaults to journald logging), so
	// ApplyDB creates it before the server ever starts against a config that
	// references it.
	DBLogDir       = "/var/log/mysql"
	PHPSocketDir   = "/run/php"
	SnippetDir     = "/etc/nginx/snippets/ngxsetup"
	SitesAvailable = "/etc/nginx/sites-available"
	SitesEnabled   = "/etc/nginx/sites-enabled"
	NginxConfD     = "/etc/nginx/conf.d"
	CredentialsDir = "/root/ngxsetup-sites"
)

// Ctx carries everything an operation needs. Constructed once per command.
type Ctx struct {
	Context context.Context
	Facts   facts.Facts
	Plan    tuning.Plan
	Config  *config.Config
	State   *state.State
	Runner  system.Runner
	Writer  *render.Writer

	// PHPVersion is the version whose FPM instance the pools belong to.
	PHPVersion string
	// FPMUnit is the systemd unit for that version.
	FPMUnit string
	// DBUnit is the database service unit, which differs between MariaDB and
	// MySQL packaging.
	DBUnit string

	// Credentials produced during this command, held only long enough to write
	// them to the mode-0600 credentials file and print them once.
	pendingAdmin         string
	pendingAdminPassword string
}

// Options configure how a command runs.
type Options struct {
	DryRun   bool
	ShowDiff bool
	Force    bool
	Verbose  bool

	ConfigPath string
	StatePath  string
	// Root relocates every path, so the whole apply can be exercised against a
	// temporary directory in tests.
	Root string

	// Tuning overrides supplied on the command line.
	Profile        string
	AvgPHPWorkerMB int
	ReserveMB      int
	MemoryMB       int
}

// New builds a Ctx: reads configuration and state, detects the machine, and
// computes the plan.
func New(ctx context.Context, o Options) (*Ctx, error) {
	cfg, err := config.Load(withRoot(o.Root, orDefault(o.ConfigPath, config.DefaultPath)))
	if err != nil {
		return nil, err
	}
	st, err := state.Load(withRoot(o.Root, orDefault(o.StatePath, state.DefaultPath)))
	if err != nil {
		return nil, err
	}

	runner := system.Runner{DryRun: o.DryRun}

	src := facts.Source(facts.OSSource{})
	f := facts.Detect(src)
	f.Storage = facts.DetectStorage(src, storageProbePath(o.Root))
	f.DetectSoftware(ctx, runner)

	if o.MemoryMB > 0 {
		f.MemTotalMB = o.MemoryMB
	}

	profile := cfg.Profile
	if o.Profile != "" {
		profile = o.Profile
	}
	prof, err := tuning.ParseProfile(profile)
	if err != nil {
		return nil, err
	}

	plan := tuning.Compute(f, tuning.Options{
		Profile:           prof,
		Sites:             st.Count(),
		AvgPHPWorkerMB:    pick(o.AvgPHPWorkerMB, cfg.AvgPHPWorkerMB),
		ReserveMB:         pick(o.ReserveMB, cfg.ReserveMB),
		UploadMaxMB:       cfg.UploadMaxMB,
		EnableBinlog:      cfg.EnableBinlog,
		AggressiveOpcache: cfg.AggressiveOpcache,
	})

	c := &Ctx{
		Context: ctx,
		Facts:   f,
		Plan:    plan,
		Config:  cfg,
		State:   st,
		Runner:  runner,
		Writer: &render.Writer{
			DryRun:    o.DryRun,
			ShowDiff:  o.ShowDiff,
			Force:     o.Force,
			Root:      o.Root,
			BackupDir: withRoot(o.Root, filepath.Join(BackupRoot, time.Now().UTC().Format("20060102-150405"))),
		},
		PHPVersion: f.PHPVersion,
	}
	c.FPMUnit = fpmUnit(f.PHPVersion)
	c.DBUnit = dbUnit(f.DBFlavor)
	return c, nil
}

// Transaction runs a set of file changes and validates the result, rolling
// everything back if validation fails.
//
// This is the safety property that separates this tool from a shell script:
// there is no state in which half the configuration has been applied.
func (c *Ctx) Transaction(name string, apply func() error, validate func() error) error {
	logx.Section("%s", name)

	if err := apply(); err != nil {
		if rbErr := c.Writer.Rollback(); rbErr != nil {
			logx.Error("rollback after failure was incomplete: %v", rbErr)
			logx.Error("previous file versions are in %s", c.Writer.BackupLocation())
		} else if c.Writer.Changed() > 0 {
			logx.Info("rolled back; no changes were left on disk")
		}
		return err
	}

	if c.Writer.DryRun {
		c.Writer.Commit()
		return nil
	}

	if validate != nil {
		if err := validate(); err != nil {
			logx.Error("validation failed: %v", err)
			logx.Step("restoring the previous configuration")
			if rbErr := c.Writer.Rollback(); rbErr != nil {
				return fmt.Errorf("%w (and rollback was incomplete: %v; backups are in %s)",
					err, rbErr, c.Writer.BackupLocation())
			}
			logx.Info("previous configuration restored; the server is unchanged")
			return err
		}
	}

	c.Writer.Commit()
	return nil
}

// Path applies the context root, so every filesystem reference in this package
// can be written as an absolute system path.
func (c *Ctx) Path(p string) string { return withRoot(c.Writer.Root, p) }

// SocketPath returns the FPM socket for a site.
func (c *Ctx) SocketPath(slug string) string {
	return filepath.Join(PHPSocketDir, slug+".sock")
}

// SiteRoot returns a site's base directory. The document root is a
// subdirectory of it, so that logs, sessions and temporary files can live
// beside the public tree without being reachable over HTTP — a layout the
// previous stack did not have, which is why it served the site straight out of
// /var/www/<name>.
func (c *Ctx) SiteRoot(slug string) string { return filepath.Join(WebRoot, slug) }

// DocumentRoot returns the directory nginx serves for a site.
func (c *Ctx) DocumentRoot(slug string) string { return filepath.Join(c.SiteRoot(slug), "public") }

func (c *Ctx) siteTmpDir(slug string) string     { return filepath.Join(c.SiteRoot(slug), "tmp") }
func (c *Ctx) siteSessionDir(slug string) string { return filepath.Join(c.SiteRoot(slug), "sessions") }

// Resolvers returns the resolver list for OCSP stapling: the local stub when
// systemd-resolved is present, with public resolvers as a fallback.
func (c *Ctx) Resolvers() string {
	if system.IsActive(c.Context, c.Runner, "systemd-resolved") {
		return "127.0.0.53"
	}
	return "1.1.1.1 8.8.8.8"
}

// HTTP2Style reports how this nginx expects HTTP/2 to be requested. It became
// a standalone directive in 1.25.1; before that it was a listen parameter, and
// emitting the wrong form fails the config test.
func (c *Ctx) HTTP2Style() string {
	if versionAtLeast(c.Facts.NginxVersion, 1, 25) {
		return "directive"
	}
	return "listen"
}

// SupportsRejectHandshake reports whether ssl_reject_handshake is available
// (nginx 1.19.4+), which is the clean way to refuse unknown TLS SNI.
func (c *Ctx) SupportsRejectHandshake() bool {
	return versionAtLeast(c.Facts.NginxVersion, 1, 20)
}

// ---- helpers ---------------------------------------------------------------

func withRoot(root, p string) string {
	if root == "" {
		return p
	}
	return filepath.Join(root, p)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func pick(override, fallback int) int {
	if override > 0 {
		return override
	}
	return fallback
}

// storageProbePath picks a directory that exists, so storage detection works
// on a machine where /var/www has not been created yet.
func storageProbePath(root string) string {
	if root != "" {
		return root
	}
	return "/var"
}

func fpmUnit(phpVersion string) string {
	if phpVersion == "" {
		return ""
	}
	return "php" + phpVersion + "-fpm.service"
}

func dbUnit(flavor facts.DBFlavor) string {
	if flavor == facts.DBMariaDB {
		return "mariadb.service"
	}
	return "mysql.service"
}

func versionAtLeast(v string, major, minor int) bool {
	if v == "" {
		return false
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return false
	}
	ma, mi := atoi(parts[0]), atoi(parts[1])
	if ma != major {
		return ma > major
	}
	return mi >= minor
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}
