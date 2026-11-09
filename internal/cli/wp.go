package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"ngxsetup/internal/logx"
	"ngxsetup/internal/provision"
	"ngxsetup/internal/system"
)

const wpUsage = `usage: ngxsetup wp [--dry-run] <domain> [--] <wp-cli arguments...>

Runs wp-cli for one site, as that site's own system user and inside the same
sandbox as its PHP-FPM service. Everything after the domain goes to wp-cli
unchanged, and wp-cli's exit status becomes this command's.

  sudo ngxsetup wp example.com plugin list
  sudo ngxsetup wp example.com plugin install query-monitor --activate
  sudo ngxsetup wp example.com core update
  sudo ngxsetup wp example.com search-replace 'http://old' 'https://new' --dry-run
  sudo ngxsetup wp example.com user update admin --prompt=user_pass
  sudo ngxsetup wp example.com shell            # interactive PHP shell
  sudo ngxsetup wp example.com plugin list --format=json | jq .
`

// exitCodeError carries a child process's exit status out of a command, so
// `ngxsetup wp` can be scripted exactly like wp-cli itself (`wp core
// is-installed` answers through its exit status alone).
type exitCodeError int

func (e exitCodeError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func cmdWP(ctx context.Context, args []string) error {
	dryRun := false
	i := 0
loop:
	for ; i < len(args); i++ {
		switch args[i] {
		case "--dry-run":
			dryRun = true
		case "--verbose":
			logx.SetLevel(logx.LevelDebug)
		case "-h", "--help", "help":
			fmt.Print(wpUsage)
			return nil
		default:
			break loop
		}
	}
	if i >= len(args) {
		return fmt.Errorf("%s", strings.TrimSpace(wpUsage))
	}
	domain, wpArgs := args[i], args[i+1:]
	if len(wpArgs) > 0 && wpArgs[0] == "--" {
		wpArgs = wpArgs[1:]
	}
	if len(wpArgs) == 0 {
		return fmt.Errorf("no wp-cli command given\n\n%s", strings.TrimSpace(wpUsage))
	}
	if !dryRun {
		if err := system.RequireRoot(); err != nil {
			return err
		}
	}

	c, err := provision.New(ctx, provision.Options{DryRun: dryRun})
	if err != nil {
		return err
	}
	rec, err := c.WPSite(domain)
	if err != nil {
		return err
	}

	interactive := isTerminal(os.Stdin) && isTerminal(os.Stdout)
	name, argv := c.WPCommand(*rec, interactive, wpArgs)
	if dryRun {
		logx.Change("[dry-run] would run: %s %s", name, strings.Join(argv, " "))
		return nil
	}

	// No context: Ctrl+C reaches wp-cli directly through the terminal's
	// process group (an interactive `wp shell` handles it itself), while
	// this process — which ignores it via the signal context Run sets up —
	// waits for the real exit status rather than abandoning a half-finished
	// search-replace.
	cmd := exec.Command(name, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
			return exitCodeError(exitErr.ExitCode())
		}
		return fmt.Errorf("running wp-cli for %s: %w", rec.Domain, err)
	}
	return nil
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
