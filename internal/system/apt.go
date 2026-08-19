package system

import (
	"context"
	"fmt"
	"strings"

	"ngxsetup/internal/logx"
)

// aptOptions keep unattended installs genuinely unattended.
//
// force-confold is the important one: when a package ships a new version of a
// config file this tool manages, dpkg would otherwise stop and wait at an
// interactive prompt that nobody is there to answer. The default answer keeps
// the existing file, which is the one we wrote.
var aptOptions = []string{
	"-y",
	"-o", "Dpkg::Options::=--force-confold",
	"-o", "Dpkg::Options::=--force-confdef",
	"--no-install-recommends",
}

// AptUpdate refreshes the package index.
func AptUpdate(ctx context.Context, r Runner) error {
	logx.Step("refreshing package index")
	return r.Run(ctx, "apt-get", "update", "-qq")
}

// PackageInstalled reports whether a package is installed and configured.
func PackageInstalled(ctx context.Context, r Runner, pkg string) bool {
	out, err := r.Output(ctx, "dpkg-query", "-W", "-f=${db:Status-Status}", pkg)
	return err == nil && strings.TrimSpace(out) == "installed"
}

// PackageAvailable reports whether a package exists in the configured
// repositories. Used before attempting optional components, so a missing
// module degrades to a warning rather than a failed provision.
func PackageAvailable(ctx context.Context, r Runner, pkg string) bool {
	out, err := r.Output(ctx, "apt-cache", "policy", pkg)
	if err != nil || strings.TrimSpace(out) == "" {
		return false
	}
	// A package with no candidate version is known to apt but not installable.
	return !strings.Contains(out, "Candidate: (none)")
}

// AptInstall installs packages, skipping any already present.
//
// Filtering first is what makes re-running setup fast and quiet: apt is not
// invoked at all when everything is already installed.
func AptInstall(ctx context.Context, r Runner, pkgs ...string) error {
	var missing []string
	for _, p := range pkgs {
		if PackageInstalled(ctx, r, p) {
			continue
		}
		missing = append(missing, p)
	}
	if len(missing) == 0 {
		logx.Skip("packages already installed: %s", strings.Join(pkgs, " "))
		return nil
	}

	logx.Step("installing %s", strings.Join(missing, " "))
	args := append([]string{"install"}, aptOptions...)
	args = append(args, missing...)
	if err := r.Run(ctx, "apt-get", args...); err != nil {
		return fmt.Errorf("installing %s: %w", strings.Join(missing, " "), err)
	}
	logx.Change("installed %s", strings.Join(missing, " "))
	return nil
}

// AptInstallOptional installs packages that improve the stack but are not
// required, reporting rather than failing when they are unavailable.
func AptInstallOptional(ctx context.Context, r Runner, pkgs ...string) []string {
	var installed []string
	for _, p := range pkgs {
		if PackageInstalled(ctx, r, p) {
			installed = append(installed, p)
			continue
		}
		if !PackageAvailable(ctx, r, p) {
			logx.Warn("optional package %s is not available in the configured repositories", p)
			continue
		}
		if err := AptInstall(ctx, r, p); err != nil {
			logx.Warn("optional package %s failed to install: %v", p, err)
			continue
		}
		installed = append(installed, p)
	}
	return installed
}

// AptRemove removes packages that are present.
func AptRemove(ctx context.Context, r Runner, pkgs ...string) error {
	var present []string
	for _, p := range pkgs {
		if PackageInstalled(ctx, r, p) {
			present = append(present, p)
		}
	}
	if len(present) == 0 {
		return nil
	}
	args := append([]string{"remove", "--purge"}, aptOptions...)
	args = append(args, present...)
	if err := r.Run(ctx, "apt-get", args...); err != nil {
		return err
	}
	logx.Change("removed %s", strings.Join(present, " "))
	return nil
}

// AptAutoremove clears orphaned dependencies.
func AptAutoremove(ctx context.Context, r Runner) {
	r.TryRun(ctx, "apt-get", "autoremove", "-y", "--purge")
}

// HoldingLock reports whether another package manager is mid-transaction.
// Racing an unattended-upgrades run produces a confusing lock error partway
// through provisioning, so it is worth checking up front.
//
// This is a read-only inspection, not a mutation, so it must use Output
// rather than Run: Run() is a no-op under --dry-run and always returns a nil
// error, which fuser's success-means-locked convention would then read as
// "always locked" — reporting a phantom lock on every dry run regardless of
// the machine's actual state.
func HoldingLock(ctx context.Context, r Runner) bool {
	if !r.Look("fuser") {
		return false
	}
	for _, lock := range []string{"/var/lib/dpkg/lock-frontend", "/var/lib/apt/lists/lock"} {
		if _, err := r.Output(ctx, "fuser", "-s", lock); err == nil {
			return true
		}
	}
	return false
}
