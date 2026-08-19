package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ngxsetup/internal/logx"
	"ngxsetup/internal/tmpl"
)

func newWriter(t *testing.T) *Writer {
	t.Helper()
	logx.SetOutput(&strings.Builder{}, &strings.Builder{})
	root := t.TempDir()
	return &Writer{Root: root, BackupDir: filepath.Join(t.TempDir(), "backup")}
}

func managed(body string) []byte { return []byte(tmpl.ManagedHeader + "\n" + body) }

func TestWriteCreatesAndReportsChange(t *testing.T) {
	w := newWriter(t)
	changed, err := w.Write("/etc/demo.conf", managed("a=1\n"), 0o644, true)
	if err != nil || !changed {
		t.Fatalf("first write: changed=%v err=%v", changed, err)
	}
	got, err := os.ReadFile(filepath.Join(w.Root, "etc/demo.conf"))
	if err != nil || !strings.Contains(string(got), "a=1") {
		t.Fatalf("content not written: %q %v", got, err)
	}
}

// Re-running setup must be quiet and must not touch files. This is the
// property that makes the tool safe to run on a live server.
func TestWriteIsIdempotent(t *testing.T) {
	w := newWriter(t)
	content := managed("a=1\n")
	if _, err := w.Write("/etc/demo.conf", content, 0o644, true); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(w.Root, "etc/demo.conf")
	before, _ := os.Stat(full)

	changed, err := w.Write("/etc/demo.conf", content, 0o644, true)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("identical content should report no change")
	}
	after, _ := os.Stat(full)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("unchanged file was rewritten")
	}
}

// Configuration a human wrote must survive. Silently overwriting it is the
// behaviour that makes operators distrust provisioning tools.
func TestRefusesToOverwriteUnmanagedFile(t *testing.T) {
	w := newWriter(t)
	full := filepath.Join(w.Root, "etc/handwritten.conf")
	os.MkdirAll(filepath.Dir(full), 0o755)
	os.WriteFile(full, []byte("# carefully tuned by a human\n"), 0o644)

	_, err := w.Write("/etc/handwritten.conf", managed("generated\n"), 0o644, true)
	if err == nil {
		t.Fatal("expected refusal to overwrite an unmanaged file")
	}
	body, _ := os.ReadFile(full)
	if !strings.Contains(string(body), "human") {
		t.Error("unmanaged file was modified anyway")
	}

	w.Force = true
	if _, err := w.Write("/etc/handwritten.conf", managed("generated\n"), 0o644, true); err != nil {
		t.Fatalf("--force should permit the overwrite: %v", err)
	}
}

func TestUnmanagedCheckSkippedForOwnedPaths(t *testing.T) {
	w := newWriter(t)
	full := filepath.Join(w.Root, "etc/nginx/sites-available/x.conf")
	os.MkdirAll(filepath.Dir(full), 0o755)
	os.WriteFile(full, []byte("old generated content\n"), 0o644)

	if _, err := w.Write("/etc/nginx/sites-available/x.conf", managed("new\n"), 0o644, false); err != nil {
		t.Fatalf("paths the tool owns should not need the managed marker: %v", err)
	}
}

// A failed validation must leave the machine exactly as it was found.
func TestRollbackRestoresPreviousState(t *testing.T) {
	w := newWriter(t)
	existing := filepath.Join(w.Root, "etc/existing.conf")
	os.MkdirAll(filepath.Dir(existing), 0o755)
	os.WriteFile(existing, managed("original\n"), 0o644)

	if _, err := w.Write("/etc/existing.conf", managed("modified\n"), 0o644, true); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write("/etc/brand-new.conf", managed("new\n"), 0o644, true); err != nil {
		t.Fatal(err)
	}

	if err := w.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	body, err := os.ReadFile(existing)
	if err != nil || !strings.Contains(string(body), "original") {
		t.Errorf("modified file not restored: %q %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(w.Root, "etc/brand-new.conf")); !os.IsNotExist(err) {
		t.Error("newly created file should be removed by rollback")
	}
}

func TestCommitDiscardsJournal(t *testing.T) {
	w := newWriter(t)
	w.Write("/etc/a.conf", managed("x\n"), 0o644, true)
	w.Commit()
	if err := w.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(w.Root, "etc/a.conf")); err != nil {
		t.Error("rollback after commit must not undo anything")
	}
}

func TestBackupPreservesOriginalPath(t *testing.T) {
	w := newWriter(t)
	target := filepath.Join(w.Root, "etc/nginx/nginx.conf")
	os.MkdirAll(filepath.Dir(target), 0o755)
	os.WriteFile(target, managed("before\n"), 0o644)

	w.Write("/etc/nginx/nginx.conf", managed("after\n"), 0o644, true)

	bak := filepath.Join(w.BackupDir, "etc/nginx/nginx.conf")
	body, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("backup not found at the mirrored path: %v", err)
	}
	if !strings.Contains(string(body), "before") {
		t.Errorf("backup holds the wrong content: %q", body)
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	w := newWriter(t)
	w.DryRun = true
	changed, err := w.Write("/etc/demo.conf", managed("a\n"), 0o644, true)
	if err != nil || !changed {
		t.Fatalf("dry run should report the change: %v %v", changed, err)
	}
	if _, err := os.Stat(filepath.Join(w.Root, "etc/demo.conf")); !os.IsNotExist(err) {
		t.Error("dry run wrote a file")
	}
}

func TestWriteIfAbsentLeavesExistingAlone(t *testing.T) {
	w := newWriter(t)
	full := filepath.Join(w.Root, "etc/override.conf")
	os.MkdirAll(filepath.Dir(full), 0o755)
	os.WriteFile(full, []byte("operator's own rules\n"), 0o644)

	changed, err := w.WriteIfAbsent("/etc/override.conf", []byte("template\n"), 0o644)
	if err != nil || changed {
		t.Fatalf("existing file should not be rewritten: changed=%v err=%v", changed, err)
	}
	body, _ := os.ReadFile(full)
	if !strings.Contains(string(body), "operator") {
		t.Error("existing override was clobbered")
	}
}

func TestEnsureDirAppliesExactMode(t *testing.T) {
	w := newWriter(t)
	if err := w.EnsureDir("/var/www/site", 0o750, ""); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(w.Root, "var/www/site"))
	if err != nil {
		t.Fatal(err)
	}
	// MkdirAll subtracts the umask; the mode must be set explicitly afterwards
	// or a directory meant to be private is created world-readable.
	if st.Mode().Perm() != 0o750 {
		t.Errorf("mode = %o, want 750", st.Mode().Perm())
	}
}

func TestSymlinkIsIdempotent(t *testing.T) {
	w := newWriter(t)
	if err := w.Symlink("/etc/nginx/sites-available/a.conf", "/etc/nginx/sites-enabled/a.conf"); err != nil {
		t.Fatal(err)
	}
	before := w.Changed()
	if err := w.Symlink("/etc/nginx/sites-available/a.conf", "/etc/nginx/sites-enabled/a.conf"); err != nil {
		t.Fatal(err)
	}
	if w.Changed() != before {
		t.Error("re-linking an identical symlink should be a no-op")
	}
}

func TestAtomicWriteLeavesNoTempFiles(t *testing.T) {
	w := newWriter(t)
	w.Write("/etc/demo.conf", managed("a\n"), 0o644, true)

	entries, _ := os.ReadDir(filepath.Join(w.Root, "etc"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") && strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestDiff(t *testing.T) {
	old := []byte("line1\nline2\nline3\n")
	new := []byte("line1\nCHANGED\nline3\n")

	d := Diff("x.conf", old, new)
	if !strings.Contains(d, "-line2") || !strings.Contains(d, "+CHANGED") {
		t.Errorf("diff missing the change:\n%s", d)
	}
	if !strings.Contains(d, " line1") {
		t.Errorf("diff missing context:\n%s", d)
	}
	if Diff("x.conf", old, old) != "" {
		t.Error("identical content should produce no diff")
	}
}

func TestDiffHandlesEmptySides(t *testing.T) {
	if d := Diff("x", nil, []byte("a\nb\n")); !strings.Contains(d, "+a") {
		t.Errorf("new file diff wrong:\n%s", d)
	}
	if d := Diff("x", []byte("a\n"), nil); !strings.Contains(d, "-a") {
		t.Errorf("deleted content diff wrong:\n%s", d)
	}
}
