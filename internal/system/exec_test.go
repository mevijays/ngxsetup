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
