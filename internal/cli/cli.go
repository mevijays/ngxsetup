// Package cli is the command surface.
//
// It is built on the standard library's flag package rather than a framework,
// which keeps the binary dependency-free and auditable — a tool that runs as
// root on other people's servers should be readable end to end.
//
// Every command that changes the machine accepts --dry-run and --diff, and
// every one of them is safe to run twice.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"ngxsetup/internal/build"
	"ngxsetup/internal/logx"
	"ngxsetup/internal/provision"
)

// Version re-exports internal/build.Version so existing callers of
// cli.Version keep working; internal/build is where the value actually
// lives and is set at release build time — see that package's doc comment
// for why it isn't just defined here.
var Version = build.Version

// globalOpts are accepted by every command.
type globalOpts struct {
	dryRun  bool
	diff    bool
	force   bool
	verbose bool
	quiet   bool
	root    string
	profile string
}

func (g *globalOpts) register(fs *flag.FlagSet) {
	fs.BoolVar(&g.dryRun, "dry-run", false, "show what would change without changing it")
	fs.BoolVar(&g.diff, "diff", false, "print a diff for every file that would change")
	fs.BoolVar(&g.force, "force", false, "overwrite configuration files ngxsetup did not create")
	fs.BoolVar(&g.verbose, "verbose", false, "log every command executed")
	fs.BoolVar(&g.quiet, "quiet", false, "print warnings and errors only")
	fs.StringVar(&g.root, "root", "", "prefix all paths with this directory (for testing)")
	fs.StringVar(&g.profile, "profile", "", "tuning profile: balanced, cache, density, database")
}

func (g globalOpts) provisionOptions() provision.Options {
	return provision.Options{
		DryRun:   g.dryRun,
		ShowDiff: g.diff,
		Force:    g.force,
		Verbose:  g.verbose,
		Root:     g.root,
		Profile:  g.profile,
	}
}

func (g globalOpts) applyLogging() {
	switch {
	case g.verbose:
		logx.SetLevel(logx.LevelDebug)
	case g.quiet:
		logx.SetLevel(logx.LevelWarn)
	}
}

// Run dispatches a command and returns a process exit code.
func Run(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// argv[0] dispatch keeps the commands the previous shell tooling exposed
	// working, so existing runbooks and muscle memory survive the rewrite.
	if alias := legacyAlias(args); alias != nil {
		args = alias
	}

	if len(args) < 2 {
		fmt.Print(usage())
		return 2
	}

	cmd := args[1]
	rest := args[2:]

	var err error
	switch cmd {
	case "setup":
		err = cmdSetup(ctx, rest)
	case "site", "vhost":
		err = cmdSite(ctx, rest)
	case "tune":
		err = cmdTune(ctx, rest)
	case "doctor":
		err = cmdDoctor(ctx, rest)
	case "status":
		err = cmdStatus(ctx, rest)
	case "top":
		err = cmdTop(ctx, rest)
	case "web":
		err = cmdWeb(ctx, rest)
	case "cache":
		err = cmdCache(ctx, rest)
	case "ssl":
		err = cmdSSL(ctx, rest)
	case "secure":
		err = cmdSecure(ctx, rest)
	case "config":
		err = cmdConfig(ctx, rest)
	case "security":
		err = cmdSecurity(ctx, rest)
	case "db":
		err = cmdDB(ctx, rest)
	case "borg":
		err = cmdBorg(ctx, rest)
	case "migrate":
		err = cmdMigrate(ctx, rest)
	case "uninstall":
		err = cmdUninstall(ctx, rest)
	case "version", "--version", "-v":
		fmt.Printf("ngxsetup %s\n", build.Version)
		fmt.Printf("Maintainer:  %s\n", build.Maintainer)
		fmt.Printf("Repository:  %s\n", build.RepoURL)
		return 0
	case "help", "--help", "-h":
		fmt.Print(usage())
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage())
		return 2
	}

	logx.Summary()

	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 2
		}
		logx.Error("%v", err)
		return 1
	}
	return 0
}

// legacyAlias rewrites an invocation made through one of the compatibility
// symlinks into the equivalent modern command.
func legacyAlias(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	switch filepath.Base(args[0]) {
	case "vhostsetup":
		// The old command was a menu-driven prompt with no arguments.
		return append([]string{"ngxsetup", "site", "add"}, args[1:]...)
	case "fixperm":
		return append([]string{"ngxsetup", "site", "fix-perms", "--all"}, args[1:]...)
	case "mysqltune":
		return append([]string{"ngxsetup", "tune", "--apply"}, args[1:]...)
	case "loadcheck":
		return append([]string{"ngxsetup", "status"}, args[1:]...)
	}
	return nil
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// parseArgs parses flags that may appear after positional arguments.
//
// The standard flag package stops at the first non-flag argument, so
// `site add example.com --wordpress` would silently discard --wordpress and
// create a site with none of the options the operator asked for. Parsing in a
// loop — take a positional, parse what follows, repeat — accepts flags and
// arguments in any order, which is what every other command-line tool does.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// arg returns the nth positional argument, or "" when absent.
func arg(positional []string, n int) string {
	if n < len(positional) {
		return positional[n]
	}
	return ""
}

func usage() string {
	return `ngxsetup — a tuned, hardened WordPress server in a single binary

USAGE
  ngxsetup <command> [flags]

SETUP
  setup                    install and configure nginx, PHP-FPM and MariaDB/MySQL
  tune                     recompute and apply tuning for this machine's resources
  secure                   apply firewall, fail2ban and update hardening

SITES  ('vhost' works identically to 'site' throughout — use whichever reads better)
  site add <domain>        create a virtual host, optionally with WordPress and TLS
  site list                list configured sites
  site info <domain>       show one site's paths, database and certificate
  site remove <domain>     remove a site
  site enable|disable      take a site in or out of service
  site fix-perms           restore correct ownership and modes

MIGRATION
  migrate discover --host <h> --user <u> --key <path>
                            list a remote Linux server's WordPress vhosts
  migrate run --host <h> --user <u> --key <path> <domain>... | --all
                            import selected sites' database and files onto
                            this machine (asks for confirmation — see --help)

OPERATIONS
  status                   show load, resources and service health
  top                      live per-site resource dashboard (CPU, memory, req/s, cache hit rate)
  web                      browser control panel — every command above, no SSH required
  doctor                   diagnose configuration, performance and security problems
  cache purge [<domain>]   drop cached responses
  ssl issue|renew          obtain or renew certificates
  config get|set|show      read and change persisted settings

SECURITY
  security scan [<domain>]   check core/plugin/theme integrity and scan for malware
  security patch [<domain>]  update outdated WordPress core, plugins and themes

BACKUP
  db backup [<domain>]       dump each site's database to a timestamped .sql file
  db restore <domain> <file> load a .sql file into a site's database, overwriting it
                              (destructive; asks for confirmation — see --help)

  borg setup --repo <repo>   configure and initialise an off-box borg repository
  borg status                show whether borg is configured and reachable
  borg backup [<domain>]     archive a site's (or every site's) files and database
  borg list                  list archives in the repository
  borg restore <domain> <archive> [--database] [--files]
                              restore from a borg archive (destructive — see --help)
  borg delete <archive>       permanently remove one archive from the repository
                              (destructive — see --help)
  borg schedule <hourly|daily|weekly|expr> | --disable
                              one-click scheduled backups via a systemd timer

UNINSTALL
  uninstall                  remove ngxsetup's configuration and revert the stack
                              (destructive; asks for confirmation — see --help)

COMMON FLAGS
  --dry-run                show what would change, change nothing
  --diff                   print a diff for every file that would change
  --profile <name>         balanced (default), cache, density, database
  --verbose                log every command executed

Run 'ngxsetup <command> --help' for the flags of a specific command.

Every command is safe to run more than once: files that already have the right
contents are left untouched, and a change that fails validation is rolled back.

` + fmt.Sprintf("ngxsetup %s — maintained by %s\n%s", build.Version, build.Maintainer, build.RepoURL)
}

// confirm asks for interactive confirmation of a destructive action.
func confirm(prompt string) bool {
	fmt.Printf("%s [y/N]: ", prompt)
	var answer string
	_, _ = fmt.Scanln(&answer)
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}
