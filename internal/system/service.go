package system

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"strings"

	"ngxsetup/internal/logx"
)

// ---- systemd ---------------------------------------------------------------

// UnitExists reports whether systemd knows about a unit.
//
// Template instances (foo@bar.service) need separate handling: list-unit-files
// enumerates only the template itself (foo@.service), never its instances, so
// the plain lookup returns false for a service that is installed and actively
// running. Confirmed live, where every per-site FPM instance was reported as
// missing while all of them were up. For an instance, the question that
// actually matters is whether systemd can load it, which `systemctl cat`
// answers — it resolves the template and any instance drop-ins.
func UnitExists(ctx context.Context, r Runner, unit string) bool {
	if isTemplateInstance(unit) {
		_, err := r.Output(ctx, "systemctl", "cat", unit)
		return err == nil
	}
	out, err := r.Output(ctx, "systemctl", "list-unit-files", "--no-legend", unit)
	return err == nil && strings.Contains(out, unit)
}

// isTemplateInstance reports whether a unit name is an instantiated template
// (has an "@" with something after it, e.g. "svc@web.service" but not the
// bare template "svc@.service").
func isTemplateInstance(unit string) bool {
	at := strings.Index(unit, "@")
	if at < 0 {
		return false
	}
	rest := unit[at+1:]
	if i := strings.LastIndex(rest, "."); i >= 0 {
		rest = rest[:i]
	}
	return rest != ""
}

// IsActive reports whether a unit is currently running.
func IsActive(ctx context.Context, r Runner, unit string) bool {
	out, _ := r.Output(ctx, "systemctl", "is-active", unit)
	return strings.TrimSpace(out) == "active"
}

// IsEnabled reports whether a unit starts at boot.
func IsEnabled(ctx context.Context, r Runner, unit string) bool {
	out, _ := r.Output(ctx, "systemctl", "is-enabled", unit)
	return strings.TrimSpace(out) == "enabled"
}

// DaemonReload makes systemd re-read unit files after a drop-in changes.
func DaemonReload(ctx context.Context, r Runner) error {
	return r.Run(ctx, "systemctl", "daemon-reload")
}

// EnableNow enables a unit and starts it if it is not already running.
func EnableNow(ctx context.Context, r Runner, unit string) error {
	if !UnitExists(ctx, r, unit) {
		return fmt.Errorf("unit %s does not exist", unit)
	}
	if !IsEnabled(ctx, r, unit) {
		if err := r.Run(ctx, "systemctl", "enable", unit); err != nil {
			return err
		}
	}
	if IsActive(ctx, r, unit) {
		return nil
	}
	return r.Run(ctx, "systemctl", "start", unit)
}

// Reload asks a unit to re-read its configuration without dropping traffic.
// Falls back to a restart for units that cannot reload.
func Reload(ctx context.Context, r Runner, unit string) error {
	if !UnitExists(ctx, r, unit) {
		return nil
	}
	if err := r.Run(ctx, "systemctl", "reload-or-restart", unit); err != nil {
		return fmt.Errorf("reloading %s: %w", unit, err)
	}
	logx.Change("reloaded %s", unit)
	return nil
}

// Restart bounces a unit. Used only where a reload genuinely cannot apply the
// change — the database, and PHP-FPM when a pool is added or removed.
func Restart(ctx context.Context, r Runner, unit string) error {
	if err := r.Run(ctx, "systemctl", "restart", unit); err != nil {
		return fmt.Errorf("restarting %s: %w", unit, err)
	}
	logx.Change("restarted %s", unit)
	return nil
}

// JournalTail returns the last n log lines for a unit, for error reporting.
func JournalTail(ctx context.Context, r Runner, unit string, n int) string {
	out, err := r.Output(ctx, "journalctl", "-u", unit, "-n", fmt.Sprint(n), "--no-pager", "--output=cat")
	if err != nil {
		return ""
	}
	return out
}

// ---- users -----------------------------------------------------------------

// UserExists reports whether a system account is present.
func UserExists(name string) bool {
	_, err := user.Lookup(name)
	return err == nil
}

// EnsureSystemUser creates a locked, shell-less system account owning one
// site's files.
//
// This is what makes site isolation real: each site's PHP-FPM pool runs as its
// own user, so a compromised plugin on one site cannot read another site's
// wp-config.php and therefore cannot reach another site's database.
func EnsureSystemUser(ctx context.Context, r Runner, name, home string) error {
	if UserExists(name) {
		logx.Skip("system user %s exists", name)
		return nil
	}
	args := []string{
		"--system",
		"--home-dir", home,
		"--no-create-home",
		// A dedicated group per user; the site's files are group-readable by
		// nginx through an explicit ACL, not by making them world-readable.
		"--user-group",
		"--shell", "/usr/sbin/nologin",
		"--comment", "ngxsetup site account",
		name,
	}
	if err := r.Run(ctx, "useradd", args...); err != nil {
		return fmt.Errorf("creating user %s: %w", name, err)
	}
	// No password hash at all, so the account cannot be authenticated against
	// by any means.
	r.TryRun(ctx, "passwd", "--lock", name)
	logx.Change("created system user %s", name)
	return nil
}

// AddUserToGroup makes a user a member of a supplementary group.
func AddUserToGroup(ctx context.Context, r Runner, username, group string) error {
	return r.Run(ctx, "usermod", "-aG", group, username)
}

// DeleteSystemUser removes an account created by EnsureSystemUser.
func DeleteSystemUser(ctx context.Context, r Runner, name string) error {
	if !UserExists(name) {
		return nil
	}
	// Home directory removal is deliberately not requested: site data is
	// removed explicitly and visibly, never as a side effect of deleting an
	// account.
	if err := r.Run(ctx, "userdel", name); err != nil {
		return err
	}
	logx.Change("removed system user %s", name)
	return nil
}

// ---- privilege -------------------------------------------------------------

// RequireRoot returns an error unless the process is running as root.
func RequireRoot() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("this command changes system configuration and must run as root (try: sudo ngxsetup ...)")
	}
	return nil
}

// IsRoot reports whether the process has root privileges, for commands that
// work either way but report more when privileged.
func IsRoot() bool { return os.Geteuid() == 0 }
