package security

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Runner is the subset of command execution this package needs — narrowed to
// keep it independent of internal/system, matching the pattern the rest of
// this codebase uses (facts.Runner, db's dbSizeQuerier) so each package
// depends on exactly the capability it needs and nothing more.
type Runner interface {
	Output(ctx context.Context, name string, args ...string) (string, error)
	CombinedOutput(ctx context.Context, name string, args ...string) (string, error)
	Look(name string) bool
}

// WPCLI runs wp-cli as a specific site's own system user — the same pattern
// site.go already uses for wp core install, so a compromised plugin's
// damage stays confined to that site's account even while being audited.
type WPCLI struct {
	Runner Runner
	// User is the site's system account (e.g. "web-example-com"). Required:
	// running wp-cli as root would give it access to every other site.
	User string
	Path string // document root
}

// run captures stdout and stderr merged. wp-cli's own convention for which
// stream a given warning lands on is not something to guess at twice in one
// project — combining both is what actually makes the parsers below reliable
// regardless.
func (w WPCLI) run(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"-u", w.User, "--", "wp"}, args...)
	full = append(full, "--path="+w.Path, "--skip-plugins", "--skip-themes")
	return w.Runner.CombinedOutput(ctx, "runuser", full...)
}

// runQuiet is for commands whose output is meant to be parsed as clean JSON
// or CSV — mixing in stderr there would corrupt the parse, so these use
// Output instead of the combined form run() uses for human-readable reports.
func (w WPCLI) runQuiet(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"-u", w.User, "--", "wp"}, args...)
	full = append(full, "--path="+w.Path, "--skip-plugins", "--skip-themes")
	return w.Runner.Output(ctx, "runuser", full...)
}

// Available reports whether wp-cli is installed at all — every method below
// degrades to a clearly labelled skipped layer without it, rather than the
// whole scan failing.
func (w WPCLI) Available() bool { return w.Runner.Look("wp") }

// VerifyCoreChecksums compares every WordPress core file against the
// checksums wordpress.org publishes for the exact version installed — an
// exact match test, not a heuristic, and the single most authoritative
// signal this scanner has for "core files were modified."
func (w WPCLI) VerifyCoreChecksums(ctx context.Context) ([]Finding, error) {
	out, err := w.run(ctx, "core", "verify-checksums", "--format=json")
	if err == nil {
		return nil, nil // exit 0: every checksum matched
	}
	// wp-cli reports mismatches on stderr/stdout and a non-zero exit; the
	// output — not the error — is what actually lists which files differ.
	return parseChecksumFailures(out), nil
}

// parseChecksumFailures turns wp-cli's verify-checksums output into
// findings. Kept separate from the command invocation so the parsing itself
// is testable against fixed sample output.
func parseChecksumFailures(out string) []Finding {
	var findings []Finding
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		// wp-cli's plain-text failure lines look like:
		//   Warning: File doesn't verify against checksum: wp-includes/x.php
		//   Warning: File should not exist: wp-admin/backdoor.php
		const marker = "File "
		i := strings.Index(line, marker)
		if i < 0 {
			continue
		}
		rest := line[i+len(marker):]
		path := rest
		if idx := strings.LastIndex(rest, ": "); idx >= 0 {
			path = strings.TrimSpace(rest[idx+2:])
		}
		if path == "" {
			continue
		}
		title := "core file modified"
		if strings.Contains(rest, "should not exist") {
			title = "unexpected file in WordPress core"
		}
		findings = append(findings, Finding{
			Severity: Critical, Category: CategoryIntegrity,
			Title:  title,
			Detail: "does not match the checksum wordpress.org publishes for this exact core version",
			Path:   path,
			Fix:    "compare against a fresh download of the same WordPress version; if you did not intentionally modify this file, treat the site as compromised",
		})
	}
	return findings
}

// VerifyPluginChecksums checks every plugin that is installed from
// wordpress.org (premium and custom plugins are not published there and have
// no checksums to compare against, so they are silently skipped here — not
// silently declared clean, which is why the scanner's summary separately
// reports how many plugins could not be checked this way).
func (w WPCLI) VerifyPluginChecksums(ctx context.Context) (findings []Finding, uncheckable int, err error) {
	out, err := w.run(ctx, "plugin", "verify-checksums", "--all", "--format=json")
	if err != nil && out == "" {
		return nil, 0, fmt.Errorf("wp plugin verify-checksums: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "is not installed from wordpress.org") || strings.Contains(line, "could not be found") {
			uncheckable++
			continue
		}
		const marker = "File "
		i := strings.Index(line, marker)
		if i < 0 {
			continue
		}
		rest := line[i+len(marker):]
		path := rest
		if idx := strings.LastIndex(rest, ": "); idx >= 0 {
			path = strings.TrimSpace(rest[idx+2:])
		}
		if path == "" {
			continue
		}
		findings = append(findings, Finding{
			Severity: Critical, Category: CategoryIntegrity,
			Title:  "plugin file modified",
			Detail: "does not match the checksum wordpress.org publishes for this exact plugin version",
			Path:   path,
			Fix:    "compare against a fresh copy of the plugin; if you did not modify this file, treat the site as compromised",
		})
	}
	return findings, uncheckable, nil
}

// wpItem is the shape wp-cli's --format=json gives for core/plugin/theme
// version and update-availability queries. update_version — the version an
// update would move to — is not one of wp-cli's default columns for `plugin
// list`/`theme list`; it has to be asked for explicitly via --fields, which
// OutdatedPlugins/OutdatedThemes below do, specifically so an operator
// choosing what to patch can see "7.1 -> 7.2," not just "an update exists."
type wpItem struct {
	Name          string `json:"name"`
	Title         string `json:"title"`
	Status        string `json:"status"`
	Update        string `json:"update"`
	Version       string `json:"version"`
	UpdateVersion string `json:"update_version"`
}

// wpItemFields is the explicit --fields list every plugin/theme query below
// uses, confirmed live against a real site's wp-cli output.
const wpItemFields = "name,title,status,update,version,update_version"

// OutdatedPlugins returns every installed plugin wp-cli reports an update
// for. An outdated plugin is not itself a compromise, but it is the single
// most common way one happens — most real-world WordPress compromises trace
// back to a known, already-patched vulnerability in something that was never
// updated.
func (w WPCLI) OutdatedPlugins(ctx context.Context) ([]wpItem, error) {
	out, err := w.runQuiet(ctx, "plugin", "list", "--update=available", "--fields="+wpItemFields, "--format=json")
	if err != nil {
		return nil, err
	}
	return parseWPItems(out)
}

// OutdatedThemes mirrors OutdatedPlugins for themes.
func (w WPCLI) OutdatedThemes(ctx context.Context) ([]wpItem, error) {
	out, err := w.runQuiet(ctx, "theme", "list", "--update=available", "--fields="+wpItemFields, "--format=json")
	if err != nil {
		return nil, err
	}
	return parseWPItems(out)
}

// CoreUpdateAvailable reports the version WordPress core would update to, or
// "" if core is already current.
func (w WPCLI) CoreUpdateAvailable(ctx context.Context) (string, error) {
	out, err := w.runQuiet(ctx, "core", "check-update", "--format=json")
	if err != nil {
		return "", err
	}
	var items []struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		return "", nil // no update available renders as "[]", not an error
	}
	if len(items) == 0 {
		return "", nil
	}
	return items[0].Version, nil
}

// CoreVersion reports the currently installed WordPress version — plain
// text, not JSON; `wp core version` has no --format flag because its
// output is already exactly one line.
func (w WPCLI) CoreVersion(ctx context.Context) (string, error) {
	out, err := w.runQuiet(ctx, "core", "version")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func parseWPItems(out string) ([]wpItem, error) {
	out = strings.TrimSpace(out)
	if out == "" || out == "[]" {
		return nil, nil
	}
	var items []wpItem
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		return nil, fmt.Errorf("parsing wp-cli output: %w", err)
	}
	return items, nil
}

// AdminUsers lists every account with the administrator role, for an
// operator to eyeball against who they actually granted access to — an
// unfamiliar admin account is one of the clearest signs of a compromise that
// checksum verification cannot see, since planting a new admin user touches
// the database, not a file.
func (w WPCLI) AdminUsers(ctx context.Context) ([]string, error) {
	out, err := w.runQuiet(ctx, "user", "list", "--role=administrator", "--field=user_login", "--format=csv")
	if err != nil {
		return nil, err
	}
	var users []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && line != "user_login" {
			users = append(users, line)
		}
	}
	return users, nil
}
