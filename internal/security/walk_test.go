package security

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func hasFinding(findings []Finding, title string) bool {
	for _, f := range findings {
		if f.Title == title {
			return true
		}
	}
	return false
}

func TestWalkAndScanFindsUploadsPHP(t *testing.T) {
	root := writeTree(t, map[string]string{
		"wp-content/uploads/2026/08/avatar.php":  "<?php echo 'hi'; ?>",
		"wp-content/plugins/example/example.php": "<?php // legitimate plugin code\n",
	})
	findings, err := WalkAndScan(root)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinding(findings, "PHP file inside wp-content/uploads") {
		t.Errorf("expected an uploads-PHP finding, got %v", findings)
	}
	for _, f := range findings {
		if f.Path == "wp-content/plugins/example/example.php" {
			t.Errorf("legitimate plugin file should not be flagged just for existing: %v", f)
		}
	}
}

func TestWalkAndScanFindsDoubleExtension(t *testing.T) {
	root := writeTree(t, map[string]string{
		"wp-content/uploads/photo.jpg.php": "<?php ?>",
	})
	findings, err := WalkAndScan(root)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinding(findings, "double file extension disguising an executable script") {
		t.Errorf("expected a double-extension finding, got %v", findings)
	}
}

func TestWalkAndScanAppliesContentRulesToPHPFiles(t *testing.T) {
	root := writeTree(t, map[string]string{
		"wp-content/themes/mytheme/functions.php": `<?php eval($_POST['x']); ?>`,
	})
	findings, err := WalkAndScan(root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range findings {
		if f.Path == "wp-content/themes/mytheme/functions.php" && f.Category == CategoryHeuristic {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the eval() backdoor to be found in functions.php, got %v", findings)
	}
}

// Non-PHP files must not be content-scanned (nothing useful would match, and
// it wastes time on what can be a large media library), but a PHP file
// disguised with a non-PHP extension must still be caught by the
// double-extension check, which runs regardless of content.
func TestWalkAndScanSkipsNonScannableContentButNotPathChecks(t *testing.T) {
	root := writeTree(t, map[string]string{
		"wp-content/uploads/2026/photo.jpg": string(make([]byte, 1024)), // binary-ish, should not be content-scanned
	})
	findings, err := WalkAndScan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Errorf("a plain image should produce no findings, got %v", findings)
	}
}

func TestWalkAndScanSkipsOversizedFiles(t *testing.T) {
	root := t.TempDir()
	big := filepath.Join(root, "huge.php")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxScannedFileSize + 1024); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Should not hang or error trying to read a huge file into memory.
	if _, err := WalkAndScan(root); err != nil {
		t.Fatal(err)
	}
}

func TestWalkAndScanHandlesUnreadableEntriesGracefully(t *testing.T) {
	root := writeTree(t, map[string]string{
		"ok.php": `<?php eval($_POST['x']); ?>`,
	})
	// A broken symlink must not abort the whole walk.
	os.Symlink(filepath.Join(root, "does-not-exist"), filepath.Join(root, "broken-link.php"))

	findings, err := WalkAndScan(root)
	if err != nil {
		t.Fatalf("a broken symlink should not fail the whole scan: %v", err)
	}
	if !hasFinding(findings, "eval() or assert() of a raw HTTP parameter") {
		t.Error("the real finding in ok.php should still be reported despite the broken symlink elsewhere")
	}
}

func TestWalkAndScanEmptyDirectory(t *testing.T) {
	root := t.TempDir()
	findings, err := WalkAndScan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Errorf("empty directory should produce no findings, got %v", findings)
	}
}
