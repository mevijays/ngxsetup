package stats

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

// The first observation of a file must not replay its entire history — a
// dashboard that just started should show "0 req/s," not years of old
// traffic compressed into the first tick.
func TestTailerStartsAtEndOfFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	writeFile(t, path, "line one\nline two\n")

	tailer := NewTailer()
	lines, err := tailer.Lines(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Errorf("first call returned %d lines, want 0 (pre-existing content is not \"new\")", len(lines))
	}
}

func TestTailerReturnsOnlyNewLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	writeFile(t, path, "existing\n")

	tailer := NewTailer()
	tailer.Lines(path) // establish baseline at current end

	appendFile(t, path, "new line 1\nnew line 2\n")
	lines, err := tailer.Lines(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0] != "new line 1" || lines[1] != "new line 2" {
		t.Errorf("Lines = %v, want [new line 1, new line 2]", lines)
	}

	// A second call with nothing appended must return nothing, not the same
	// lines again.
	lines, err = tailer.Lines(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Errorf("second call with no new writes returned %d lines, want 0", len(lines))
	}
}

// nginx may still be mid-write on the last line of a batch; it must be left
// for the next call rather than returned truncated.
func TestTailerLeavesPartialLineForNextCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	writeFile(t, path, "")
	tailer := NewTailer()
	tailer.Lines(path)

	appendFile(t, path, "complete line\npartial line no newline yet")
	lines, err := tailer.Lines(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "complete line" {
		t.Errorf("Lines = %v, want exactly [complete line]", lines)
	}

	appendFile(t, path, " -- now complete\n")
	lines, err = tailer.Lines(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "partial line no newline yet -- now complete" {
		t.Errorf("Lines = %v, want the completed partial line joined correctly", lines)
	}
}

// logrotate's create+truncate must not wedge the tailer against a byte
// offset that no longer means anything in the new file.
func TestTailerHandlesRotationByTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	writeFile(t, path, "a very long line of pre-rotation content\n")
	tailer := NewTailer()
	tailer.Lines(path)
	appendFile(t, path, "one more line before rotation\n")
	tailer.Lines(path)

	// logrotate copytruncate: file shrinks to (near) zero, then new lines
	// arrive.
	writeFile(t, path, "first line after rotation\n")
	lines, err := tailer.Lines(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "first line after rotation" {
		t.Errorf("Lines after rotation = %v, want [first line after rotation]", lines)
	}
}

func TestTailerMissingFileIsNotAnError(t *testing.T) {
	tailer := NewTailer()
	lines, err := tailer.Lines(filepath.Join(t.TempDir(), "does-not-exist.log"))
	if err != nil {
		t.Errorf("a site with no access log yet should not error, got %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("expected no lines, got %v", lines)
	}
}

func TestTailerReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	writeFile(t, path, "line one\n")
	tailer := NewTailer()
	tailer.Lines(path) // baseline at end

	tailer.Reset(path)
	appendFile(t, path, "line two\n")
	lines, err := tailer.Lines(path)
	if err != nil {
		t.Fatal(err)
	}
	// After Reset, the path is "newly observed" again: baseline resets to
	// end-of-file on this call, so line two (already on disk before this
	// call) is not returned — only content appended after this call would be.
	if len(lines) != 0 {
		t.Errorf("Lines after Reset = %v, want 0 (Reset re-baselines rather than replaying)", lines)
	}
}
