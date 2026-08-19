package security

import (
	"context"
	"strings"
	"testing"
)

// fakeWPRunner lets WPCLI's logic be tested without a real wp-cli install.
// Responses are keyed by the joined argument list so a test can distinguish
// between the different wp subcommands WPCLI issues.
type fakeWPRunner struct {
	installed bool
	responses map[string]fakeResponse
}

type fakeResponse struct {
	out string
	err error
}

func (f fakeWPRunner) Look(name string) bool { return f.installed }

func (f fakeWPRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	return f.lookup(args)
}
func (f fakeWPRunner) CombinedOutput(ctx context.Context, name string, args ...string) (string, error) {
	return f.lookup(args)
}
func (f fakeWPRunner) lookup(args []string) (string, error) {
	key := strings.Join(args, " ")
	for k, resp := range f.responses {
		if strings.Contains(key, k) {
			return resp.out, resp.err
		}
	}
	return "", nil
}

type sentinelErr struct{ msg string }

func (e sentinelErr) Error() string { return e.msg }

func TestParseChecksumFailures(t *testing.T) {
	out := `Warning: File doesn't verify against checksum: wp-includes/version.php
Warning: File should not exist: wp-admin/shell.php
Error: WordPress installation doesn't verify against checksums.`
	findings := parseChecksumFailures(out)
	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2", len(findings))
	}
	if findings[0].Path != "wp-includes/version.php" {
		t.Errorf("findings[0].Path = %q", findings[0].Path)
	}
	if findings[0].Title != "core file modified" {
		t.Errorf("findings[0].Title = %q", findings[0].Title)
	}
	if findings[1].Path != "wp-admin/shell.php" {
		t.Errorf("findings[1].Path = %q", findings[1].Path)
	}
	if findings[1].Title != "unexpected file in WordPress core" {
		t.Errorf("findings[1].Title = %q, want to distinguish \"should not exist\" from a modified file", findings[1].Title)
	}
	for _, f := range findings {
		if f.Severity != Critical {
			t.Errorf("checksum mismatch should be Critical, got %v", f.Severity)
		}
	}
}

func TestParseChecksumFailuresCleanOutput(t *testing.T) {
	if findings := parseChecksumFailures(""); len(findings) != 0 {
		t.Errorf("empty output should produce no findings, got %v", findings)
	}
}

func TestVerifyCoreChecksumsCleanWhenNoError(t *testing.T) {
	w := WPCLI{Runner: fakeWPRunner{installed: true}, User: "web-example-com", Path: "/var/www/example-com/public"}
	findings, err := w.VerifyCoreChecksums(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Errorf("a clean exit should produce no findings, got %v", findings)
	}
}

func TestVerifyCoreChecksumsReportsFailures(t *testing.T) {
	w := WPCLI{
		Runner: fakeWPRunner{installed: true, responses: map[string]fakeResponse{
			"core verify-checksums": {
				out: "Warning: File doesn't verify against checksum: wp-includes/version.php\n",
				err: sentinelErr{"exit status 1"},
			},
		}},
		User: "web-example-com", Path: "/x",
	}
	findings, err := w.VerifyCoreChecksums(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Path != "wp-includes/version.php" {
		t.Errorf("findings = %v, want one finding for wp-includes/version.php", findings)
	}
}

func TestParseWPItemsOutdatedPlugins(t *testing.T) {
	out := `[{"name":"akismet","title":"Akismet","status":"active","update":"available","version":"5.3","update_version":"5.4"}]`
	items, err := parseWPItems(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "akismet" {
		t.Errorf("items = %+v", items)
	}
	// update_version — the version an update would move to, not just "an
	// update exists" — is what lets an operator see "5.3 -> 5.4" rather
	// than a bare yes/no before choosing what to patch.
	if items[0].UpdateVersion != "5.4" {
		t.Errorf("UpdateVersion = %q, want 5.4", items[0].UpdateVersion)
	}
	if items[0].Title != "Akismet" {
		t.Errorf("Title = %q, want Akismet", items[0].Title)
	}
}

func TestWPCLICoreVersion(t *testing.T) {
	w := WPCLI{Runner: fakeWPRunner{installed: true, responses: map[string]fakeResponse{
		"core version": {out: "6.7.1\n"},
	}}, User: "u", Path: "/x"}
	v, err := w.CoreVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != "6.7.1" {
		t.Errorf("CoreVersion = %q, want 6.7.1 (trimmed)", v)
	}
}

func TestParseWPItemsEmptyArray(t *testing.T) {
	items, err := parseWPItems("[]")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("expected no items when everything is current, got %v", items)
	}
}

func TestParseWPItemsEmptyString(t *testing.T) {
	items, err := parseWPItems("")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("expected no items for empty output, got %v", items)
	}
}

func TestParseWPItemsMalformedJSON(t *testing.T) {
	if _, err := parseWPItems("not json"); err == nil {
		t.Error("malformed JSON should produce an error, not a silent empty result")
	}
}

func TestCoreUpdateAvailable(t *testing.T) {
	w := WPCLI{
		Runner: fakeWPRunner{installed: true, responses: map[string]fakeResponse{
			"core check-update": {out: `[{"version":"6.7.1","update_type":"major"}]`},
		}},
		User: "u", Path: "/x",
	}
	version, err := w.CoreUpdateAvailable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != "6.7.1" {
		t.Errorf("version = %q, want 6.7.1", version)
	}
}

func TestCoreUpdateAvailableWhenCurrent(t *testing.T) {
	w := WPCLI{
		Runner: fakeWPRunner{installed: true, responses: map[string]fakeResponse{
			"core check-update": {out: `[]`},
		}},
		User: "u", Path: "/x",
	}
	version, err := w.CoreUpdateAvailable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != "" {
		t.Errorf("version = %q, want empty when core is already current", version)
	}
}

func TestAdminUsersParsesCSVAndSkipsHeader(t *testing.T) {
	w := WPCLI{
		Runner: fakeWPRunner{installed: true, responses: map[string]fakeResponse{
			"user list": {out: "user_login\nadmin\nsiteowner\n"},
		}},
		User: "u", Path: "/x",
	}
	users, err := w.AdminUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0] != "admin" || users[1] != "siteowner" {
		t.Errorf("users = %v, want [admin siteowner]", users)
	}
}

func TestWPCLIRunsAsSiteUserNotRoot(t *testing.T) {
	// The command construction itself has to name the site's own user, or
	// running wp-cli would inherit whatever privilege the caller has —
	// exactly the cross-site access this whole isolation model exists to
	// prevent.
	var captured []string
	runner := capturingRunner{fn: func(args []string) { captured = args }}
	w := WPCLI{Runner: runner, User: "web-example-com", Path: "/var/www/example-com/public"}
	w.run(context.Background(), "core", "verify-checksums")

	if len(captured) < 2 || captured[0] != "-u" || captured[1] != "web-example-com" {
		t.Errorf("command args = %v, want to start with -u web-example-com", captured)
	}
	joined := strings.Join(captured, " ")
	if !strings.Contains(joined, "--path=/var/www/example-com/public") {
		t.Errorf("command args = %v, missing --path for the site's own document root", captured)
	}
}

type capturingRunner struct{ fn func([]string) }

func (c capturingRunner) Look(string) bool { return true }
func (c capturingRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	c.fn(args)
	return "", nil
}
func (c capturingRunner) CombinedOutput(ctx context.Context, name string, args ...string) (string, error) {
	c.fn(args)
	return "", nil
}
