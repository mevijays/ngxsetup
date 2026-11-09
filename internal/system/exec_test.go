package system

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// RunStdinFile is how a database restore reaches the client, streaming a
// dump that can run to hundreds of megabytes. `cat` stands in for the real
// client here: what matters is that the file's contents arrive on the
// child's stdin unchanged, not which binary receives them.
func TestRunStdinFileStreamsFileContents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "input.txt")
	want := "line one\nline two\n"
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}

	r := Runner{}
	out, err := r.RunStdinFile(context.Background(), path, "cat")
	if err != nil {
		t.Fatalf("RunStdinFile: %v", err)
	}
	if out != want {
		t.Errorf("RunStdinFile output = %q, want %q", out, want)
	}
}

func TestRunStdinFileRejectsMissingFile(t *testing.T) {
	r := Runner{}
	if _, err := r.RunStdinFile(context.Background(), "/no/such/file-really-not-there.sql", "cat"); err == nil {
		t.Error("RunStdinFile accepted a nonexistent file")
	}
}

// DryRun must not touch the file at all, matching every other mutating
// Runner method — a preview that can fail on a missing file is not a preview.
func TestRunStdinFileDryRunDoesNotOpenFile(t *testing.T) {
	r := Runner{DryRun: true}
	out, err := r.RunStdinFile(context.Background(), "/no/such/file-really-not-there.sql", "cat")
	if err != nil {
		t.Fatalf("dry-run RunStdinFile returned an error: %v", err)
	}
	if out != "" {
		t.Errorf("dry-run RunStdinFile returned output %q, want empty", out)
	}
}

// RunToFile is how a database dump reaches disk: streamed, and renamed into
// place only on success.
func TestRunToFileStreamsOutputAndRenamesOnSuccess(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "dump.sql")
	if err := (Runner{}).RunToFile(context.Background(), dest, 0o600, "sh", "-c", "printf 'a\\nb\\n'"); err != nil {
		t.Fatalf("RunToFile: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "a\nb\n" {
		t.Fatalf("dest = %q, %v; want the command's output", got, err)
	}
	if info, _ := os.Stat(dest); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}

// A failed command must leave neither the real file nor its partial temp
// file behind — a truncated dump under the real name is worse than none.
func TestRunToFileLeavesNothingOnFailure(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "dump.sql")
	err := (Runner{}).RunToFile(context.Background(), dest, 0o600, "sh", "-c", "echo partial; echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("RunToFile reported success for a failing command")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("files left behind after a failure: %v", entries)
	}
}
