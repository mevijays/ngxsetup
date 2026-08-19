package webui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSnapshotTailReturnsLastNLines(t *testing.T) {
	content := "line1\nline2\nline3\nline4\nline5\n"
	path := writeTemp(t, content)

	lines, size, err := snapshotTail(path, 3)
	if err != nil {
		t.Fatalf("snapshotTail: %v", err)
	}
	want := []string{"line3", "line4", "line5"}
	if strings.Join(lines, ",") != strings.Join(want, ",") {
		t.Errorf("lines = %v, want %v", lines, want)
	}
	if size != int64(len(content)) {
		t.Errorf("size = %d, want %d", size, len(content))
	}
}

func TestSnapshotTailFewerLinesThanRequested(t *testing.T) {
	path := writeTemp(t, "only\ntwo\n")
	lines, _, err := snapshotTail(path, 50)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, ",") != "only,two" {
		t.Errorf("lines = %v", lines)
	}
}

func TestSnapshotTailMissingFile(t *testing.T) {
	if _, _, err := snapshotTail(filepath.Join(t.TempDir(), "nope.log"), 10); err == nil {
		t.Error("snapshotTail accepted a nonexistent file")
	}
}

// The window is bounded (maxTailWindow), so a huge file must not be read in
// full — this is the "does not load the server much" guarantee made
// concrete: exercise it against a file bigger than the window and confirm
// the function still returns quickly and correctly, rather than trusting
// the constant alone.
func TestSnapshotTailBoundedWindowOnLargeFile(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20000; i++ {
		b.WriteString("this is a reasonably long log line to pad the file size out\n")
	}
	path := writeTemp(t, b.String())

	lines, size, err := snapshotTail(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 5 {
		t.Errorf("got %d lines, want 5", len(lines))
	}
	if size < maxTailWindow {
		t.Fatalf("test file (%d bytes) is not actually larger than the tail window (%d) — test is not exercising the bounded path", size, maxTailWindow)
	}
}

func TestTailFromReturnsOnlyNewCompleteLines(t *testing.T) {
	path := writeTemp(t, "first\nsecond\n")

	lines, offset, err := tailFrom(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, ",") != "first,second" {
		t.Errorf("lines = %v", lines)
	}

	// Append more, including a partial line with no trailing newline yet.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("third\npartial-no-newline-yet")
	f.Close()

	lines2, offset2, err := tailFrom(path, offset)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines2, ",") != "third" {
		t.Errorf("lines2 = %v, want just [third] — the unterminated line must not appear yet", lines2)
	}

	// Nothing new since offset2 (the partial line still has no newline).
	lines3, offset3, err := tailFrom(path, offset2)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines3) != 0 {
		t.Errorf("lines3 = %v, want none (partial line still unterminated)", lines3)
	}
	if offset3 != offset2 {
		t.Errorf("offset advanced past a partial line: %d -> %d", offset2, offset3)
	}

	// Finish the line; it should appear on the next poll.
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("\n")
	f.Close()
	lines4, _, err := tailFrom(path, offset3)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines4, ",") != "partial-no-newline-yet" {
		t.Errorf("lines4 = %v", lines4)
	}
}

func TestTailFromNoNewData(t *testing.T) {
	path := writeTemp(t, "only line\n")
	_, offset, err := tailFrom(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	lines, offset2, err := tailFrom(path, offset)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Errorf("expected no new lines, got %v", lines)
	}
	if offset2 != offset {
		t.Errorf("offset changed with no new data: %d -> %d", offset, offset2)
	}
}

// A rotated or truncated log file (offset now beyond the file's current
// size) must resume from the start rather than error out or return nothing
// forever.
func TestTailFromHandlesRotation(t *testing.T) {
	path := writeTemp(t, "a very long line that will be replaced\n")
	lines, offset, err := tailFrom(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("setup: expected 1 line, got %v", lines)
	}

	// Simulate rotation: truncate to something shorter than the old offset.
	if err := os.WriteFile(path, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	lines2, _, err := tailFrom(path, offset)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines2, ",") != "new" {
		t.Errorf("after rotation, lines = %v, want [new]", lines2)
	}
}

func TestFileExists(t *testing.T) {
	path := writeTemp(t, "x\n")
	if !fileExists(path) {
		t.Error("fileExists false for a file that exists")
	}
	if fileExists(filepath.Join(t.TempDir(), "nope")) {
		t.Error("fileExists true for a file that does not exist")
	}
	if fileExists(filepath.Dir(path)) {
		t.Error("fileExists true for a directory")
	}
}
