package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrateFixHardcodedPathsRewritesPHPFiles is the scenario the feature
// exists for: WP Super Cache's advanced-cache.php drop-in hardcodes the
// site's old absolute path in a WPCACHEHOME-style constant, because that
// file loads before WordPress's normal plugin autoloading can resolve its
// own location any other way.
func TestMigrateFixHardcodedPathsRewritesPHPFiles(t *testing.T) {
	c := testCtx(t)
	docRoot := "/var/www/site/public"
	oldRoot := "/var/www/old-site/public"
	newRoot := docRoot

	writeTestFile(t, c, filepath.Join(docRoot, "wp-content", "advanced-cache.php"),
		"<?php\ndefine('WPCACHEHOME', '"+oldRoot+"/wp-content/plugins/wp-super-cache/');\n", 0o644)
	writeTestFile(t, c, filepath.Join(docRoot, "wp-content", "plugins", "example", "example.php"),
		"<?php\n// nothing site-specific here\necho 'hello';\n", 0o644)

	n, err := c.MigrateFixHardcodedPaths(docRoot, oldRoot, newRoot)
	if err != nil {
		t.Fatalf("MigrateFixHardcodedPaths: %v", err)
	}
	if n != 1 {
		t.Errorf("fixed %d file(s), want exactly 1 (only advanced-cache.php referenced the old path)", n)
	}

	got := read(t, c, filepath.Join(docRoot, "wp-content", "advanced-cache.php"))
	if strings.Contains(got, oldRoot) {
		t.Errorf("old path still present after fixup: %q", got)
	}
	if !strings.Contains(got, newRoot+"/wp-content/plugins/wp-super-cache/") {
		t.Errorf("new path not correctly substituted: %q", got)
	}

	untouched := read(t, c, filepath.Join(docRoot, "wp-content", "plugins", "example", "example.php"))
	if !strings.Contains(untouched, "echo 'hello';") {
		t.Errorf("a file with no old-path reference was modified: %q", untouched)
	}
}

func TestMigrateFixHardcodedPathsSkipsNonPHPFiles(t *testing.T) {
	c := testCtx(t)
	docRoot := "/var/www/site/public"
	oldRoot := "/var/www/old-site/public"

	writeTestFile(t, c, filepath.Join(docRoot, "notes.txt"), "see "+oldRoot+" for details", 0o644)

	n, err := c.MigrateFixHardcodedPaths(docRoot, oldRoot, docRoot)
	if err != nil {
		t.Fatalf("MigrateFixHardcodedPaths: %v", err)
	}
	if n != 0 {
		t.Errorf("fixed %d file(s), want 0 — a .txt file must not be scanned or rewritten", n)
	}
	if got := read(t, c, filepath.Join(docRoot, "notes.txt")); !strings.Contains(got, oldRoot) {
		t.Error("a non-PHP file was modified even though it should have been skipped entirely")
	}
}

func TestMigrateFixHardcodedPathsSkipsOversizedFiles(t *testing.T) {
	c := testCtx(t)
	docRoot := "/var/www/site/public"
	oldRoot := "/var/www/old-site/public"

	big := "<?php\n// " + oldRoot + "\n" + strings.Repeat("x", maxPathFixFileSize+1)
	writeTestFile(t, c, filepath.Join(docRoot, "huge.php"), big, 0o644)

	n, err := c.MigrateFixHardcodedPaths(docRoot, oldRoot, docRoot)
	if err != nil {
		t.Fatalf("MigrateFixHardcodedPaths: %v", err)
	}
	if n != 0 {
		t.Errorf("fixed %d file(s), want 0 — a file over the size cap must be skipped", n)
	}
}

func TestMigrateFixHardcodedPathsNoOpWhenRootsMatchOrEmpty(t *testing.T) {
	c := testCtx(t)
	docRoot := "/var/www/site/public"
	writeTestFile(t, c, filepath.Join(docRoot, "a.php"), "<?php // "+docRoot, 0o644)

	cases := []struct{ old, new string }{
		{docRoot, docRoot}, // identical — nothing could have changed
		{"", docRoot},
		{docRoot, ""},
	}
	for _, tc := range cases {
		if n, err := c.MigrateFixHardcodedPaths(docRoot, tc.old, tc.new); err != nil || n != 0 {
			t.Errorf("MigrateFixHardcodedPaths(%q, %q) = (%d, %v), want (0, nil)", tc.old, tc.new, n, err)
		}
	}
}

func TestMigrateFixHardcodedPathsPreservesFileMode(t *testing.T) {
	c := testCtx(t)
	docRoot := "/var/www/site/public"
	oldRoot := "/var/www/old-site/public"
	path := filepath.Join(docRoot, "advanced-cache.php")
	writeTestFile(t, c, path, "<?php // "+oldRoot, 0o640)

	if _, err := c.MigrateFixHardcodedPaths(docRoot, oldRoot, docRoot); err != nil {
		t.Fatalf("MigrateFixHardcodedPaths: %v", err)
	}
	info, err := os.Stat(c.Path(path))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("file mode after rewrite = %o, want 0640 (unchanged)", info.Mode().Perm())
	}
}

func TestMigrateFixHardcodedPathsRespectsDryRun(t *testing.T) {
	c := testCtx(t)
	c.Writer.DryRun = true
	docRoot := "/var/www/site/public"
	oldRoot := "/var/www/old-site/public"
	path := filepath.Join(docRoot, "advanced-cache.php")
	writeTestFile(t, c, path, "<?php // "+oldRoot, 0o644)

	n, err := c.MigrateFixHardcodedPaths(docRoot, oldRoot, docRoot)
	if err != nil || n != 0 {
		t.Fatalf("MigrateFixHardcodedPaths under DryRun = (%d, %v), want (0, nil)", n, err)
	}
	if got := read(t, c, path); !strings.Contains(got, oldRoot) {
		t.Error("DryRun still modified a file on disk")
	}
}

// writeTestFile creates a file (and its parent directories) under the test
// Ctx's rooted filesystem, using a logical path the same way the rest of
// this package's own methods (and callers) address one.
func writeTestFile(t *testing.T, c *Ctx, logicalPath, content string, mode os.FileMode) {
	t.Helper()
	full := c.Path(logicalPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
