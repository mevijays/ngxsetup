package webui

import (
	"os"
	"path/filepath"
	"testing"

	"ngxsetup/internal/provision"
	"ngxsetup/internal/render"
)

// A crafted path must never resolve to anything outside the backup
// directory — resolveBackupPath is what both download and delete rely on
// for that, so it is tested directly against the exact attack shapes a
// client could send, independent of which HTTP verb ends up calling it.
func TestResolveBackupPathRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	backupDir := filepath.Join(root, provision.DefaultBackupDir)
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(backupDir, "site-20260101-000000.sql")
	if err := os.WriteFile(good, []byte("-- dump\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A file that genuinely exists outside the backup directory, so a
	// traversal attempt that reaches it can be told apart from one that
	// simply resolves to a nonexistent path.
	secret := filepath.Join(root, "secret.sql")
	if err := os.WriteFile(secret, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &provision.Ctx{Writer: &render.Writer{Root: root}}

	if _, _, err := resolveBackupPath(c, ""); err == nil {
		t.Error("empty path was accepted")
	}

	bad := []string{
		"../secret.sql",
		"../../" + filepath.Base(secret),
		// A logical absolute path outside DefaultBackupDir — the form a
		// real client would actually send, e.g. reusing ListBackups'
		// reporting convention to ask for something it never reported.
		"/etc/passwd",
		filepath.Join(provision.DefaultBackupDir, "..", "secret.sql"),
	}
	for _, p := range bad {
		if full, _, err := resolveBackupPath(c, p); err == nil {
			t.Errorf("resolveBackupPath(%q) = %q, nil — want an error", p, full)
		}
	}

	// The legitimate file, addressed both ways ListBackups' own Path field
	// can appear: a bare filename, and the logical absolute path
	// ListBackups actually reports (DefaultBackupDir + name — deliberately
	// *not* run through c.Path() itself, the same way ListBackups builds
	// it, since resolveBackupPath is the one responsible for applying the
	// --root test prefix, not its caller).
	logicalAbs := filepath.Join(provision.DefaultBackupDir, "site-20260101-000000.sql")
	for _, p := range []string{"site-20260101-000000.sql", logicalAbs} {
		full, info, err := resolveBackupPath(c, p)
		if err != nil {
			t.Errorf("resolveBackupPath(%q) unexpectedly failed: %v", p, err)
			continue
		}
		if full != good {
			t.Errorf("resolveBackupPath(%q) = %q, want %q", p, full, good)
		}
		if info == nil || info.IsDir() {
			t.Errorf("resolveBackupPath(%q) returned a bad FileInfo", p)
		}
	}

	// Right directory, wrong extension — never a real backup, no matter
	// what ListBackups would ever actually put there.
	notSQL := filepath.Join(backupDir, "not-a-backup.txt")
	if err := os.WriteFile(notSQL, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveBackupPath(c, notSQL); err == nil {
		t.Error("a non-.sql file inside the backup dir was accepted")
	}
}
