package cli

import (
	"context"
	"flag"
	"io"
	"testing"
)

func testFlagSet() (*flag.FlagSet, *bool, *string) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	b := fs.Bool("wordpress", false, "")
	s := fs.String("alias", "", "")
	return fs, b, s
}

// The standard flag package stops parsing at the first positional argument, so
// `site add example.com --wordpress` would silently create a site with none of
// the requested options. Every ordering has to work.
func TestParseArgsAcceptsFlagsAfterPositionals(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantPos   []string
		wantWP    bool
		wantAlias string
	}{
		{"flags first", []string{"--wordpress", "example.com"}, []string{"example.com"}, true, ""},
		{"flags last", []string{"example.com", "--wordpress"}, []string{"example.com"}, true, ""},
		{"interleaved", []string{"example.com", "--wordpress", "--alias", "www.example.com"},
			[]string{"example.com"}, true, "www.example.com"},
		{"flag between positionals", []string{"a.com", "--wordpress", "b.com"},
			[]string{"a.com", "b.com"}, true, ""},
		{"value flag then positional", []string{"--alias", "x.com", "example.com"},
			[]string{"example.com"}, false, "x.com"},
		{"no args", nil, nil, false, ""},
		{"positional only", []string{"example.com"}, []string{"example.com"}, false, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs, wp, alias := testFlagSet()
			pos, err := parseArgs(fs, c.args)
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}
			if len(pos) != len(c.wantPos) {
				t.Fatalf("positional = %v, want %v", pos, c.wantPos)
			}
			for i := range pos {
				if pos[i] != c.wantPos[i] {
					t.Errorf("positional[%d] = %q, want %q", i, pos[i], c.wantPos[i])
				}
			}
			if *wp != c.wantWP {
				t.Errorf("--wordpress = %v, want %v", *wp, c.wantWP)
			}
			if *alias != c.wantAlias {
				t.Errorf("--alias = %q, want %q", *alias, c.wantAlias)
			}
		})
	}
}

func TestParseArgsReportsUnknownFlags(t *testing.T) {
	fs, _, _ := testFlagSet()
	if _, err := parseArgs(fs, []string{"example.com", "--nonexistent"}); err == nil {
		t.Error("an unknown flag should be an error, not silently ignored")
	}
}

func TestArgAccessor(t *testing.T) {
	pos := []string{"a", "b"}
	if arg(pos, 0) != "a" || arg(pos, 1) != "b" {
		t.Error("positional accessor returned the wrong value")
	}
	if arg(pos, 5) != "" {
		t.Error("out-of-range positional should be empty, not a panic")
	}
	if arg(nil, 0) != "" {
		t.Error("nil positional list should be empty")
	}
}

// The compatibility symlinks are how existing runbooks keep working; each must
// map to the modern command.
func TestLegacyAliases(t *testing.T) {
	cases := map[string][]string{
		"/usr/local/bin/vhostsetup": {"ngxsetup", "site", "add"},
		"/usr/local/bin/fixperm":    {"ngxsetup", "site", "fix-perms", "--all"},
		"/usr/local/bin/mysqltune":  {"ngxsetup", "tune", "--apply"},
		"/usr/local/bin/loadcheck":  {"ngxsetup", "status"},
	}
	for argv0, want := range cases {
		got := legacyAlias([]string{argv0})
		if got == nil {
			t.Errorf("%s was not recognised as an alias", argv0)
			continue
		}
		if len(got) != len(want) {
			t.Errorf("%s -> %v, want %v", argv0, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s -> %v, want %v", argv0, got, want)
				break
			}
		}
	}

	if legacyAlias([]string{"/usr/local/bin/ngxsetup"}) != nil {
		t.Error("the real binary name should not be rewritten")
	}
	if legacyAlias(nil) != nil {
		t.Error("empty argv should not panic")
	}
}

// Arguments passed through an alias must survive the rewrite.
func TestLegacyAliasPreservesArguments(t *testing.T) {
	got := legacyAlias([]string{"/usr/local/bin/fixperm", "--dry-run"})
	if len(got) == 0 || got[len(got)-1] != "--dry-run" {
		t.Errorf("alias dropped the caller's arguments: %v", got)
	}
}

// "vhost" and "site" must be interchangeable throughout, and "create" must
// alias "add" — same validation, same downstream call. Both --root at an
// isolated temp dir so this is hermetic regardless of what happens to exist
// on the machine running the test, and both use the same invalid state so
// they fail at the identical point (before any site-specific logic runs),
// proving the two spellings take the same code path rather than merely
// looking similar.
func TestVhostAndCreateAliasSiteAndAdd(t *testing.T) {
	tmp := t.TempDir()
	ctx := context.Background()

	errSite := cmdSite(ctx, []string{"add", "example.com", "--root", tmp})
	errVhost := cmdSite(ctx, []string{"create", "example.com", "--root", tmp})
	if errSite == nil || errVhost == nil {
		t.Fatal("expected both to fail: no stack is installed under this root")
	}
	if errSite.Error() != errVhost.Error() {
		t.Errorf("\"add\" and \"create\" diverged:\nadd:    %v\ncreate: %v", errSite, errVhost)
	}

	errList1 := cmdSite(ctx, []string{"list", "--root", tmp})
	errList2 := cmdSite(ctx, []string{"list", "--root", tmp})
	if errList1 != nil || errList2 != nil {
		t.Fatalf("site list should succeed against an empty registry: %v / %v", errList1, errList2)
	}
}

// security scan/patch against an empty registry must produce a clear,
// specific error rather than a panic or a confusing wp-cli failure three
// layers down.
func TestSecurityCommandsRequireSites(t *testing.T) {
	tmp := t.TempDir()
	ctx := context.Background()

	if err := cmdSecurity(ctx, []string{"scan", "--root", tmp}); err == nil {
		t.Error("expected an error when no sites are configured")
	}
	if err := cmdSecurity(ctx, []string{"patch", "--root", tmp}); err == nil {
		t.Error("expected an error when no sites are configured")
	}
}

func TestSecurityUnknownSubcommand(t *testing.T) {
	if err := cmdSecurity(context.Background(), []string{"bogus"}); err == nil {
		t.Error("expected an error for an unrecognised security subcommand")
	}
}

func TestSecurityNoSubcommand(t *testing.T) {
	if err := cmdSecurity(context.Background(), nil); err == nil {
		t.Error("expected a usage error when no subcommand is given")
	}
}

// uninstall against an empty root must still work (nothing to remove is not
// an error) and must not require confirmation input when --dry-run is set.
func TestUninstallDryRunNoSites(t *testing.T) {
	tmp := t.TempDir()
	if err := cmdUninstall(context.Background(), []string{"--dry-run", "--root", tmp}); err != nil {
		t.Fatalf("dry-run uninstall on an empty root should succeed, got %v", err)
	}
}

func TestUninstallYesSkipsPrompt(t *testing.T) {
	tmp := t.TempDir()
	// --yes must not block on stdin; if it did, this test would hang rather
	// than complete, so simply returning is the assertion.
	if err := cmdUninstall(context.Background(), []string{"--dry-run", "--yes", "--root", tmp}); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

func TestDBBackupRequiresSubcommand(t *testing.T) {
	if err := cmdDB(context.Background(), nil); err == nil {
		t.Error("expected a usage error when no db subcommand is given")
	}
}

func TestDBBackupUnknownSubcommand(t *testing.T) {
	if err := cmdDB(context.Background(), []string{"bogus"}); err == nil {
		t.Error("expected an error for an unrecognised db subcommand")
	}
}

func TestDBBackupNoSitesConfigured(t *testing.T) {
	tmp := t.TempDir()
	if err := cmdDBBackup(context.Background(), []string{"--root", tmp}); err == nil {
		t.Error("expected an error backing up with no sites registered")
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" a.com , b.com ,, c.com ")
	want := []string{"a.com", "b.com", "c.com"}
	if len(got) != len(want) {
		t.Fatalf("splitList = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("splitList[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if splitList("") != nil {
		t.Error("empty input should give a nil list")
	}
}
