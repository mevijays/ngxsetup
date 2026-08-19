package stats

import (
	"bufio"
	"io"
	"os"
)

// Tailer reads only the lines appended to a file since the last call,
// tracking a byte offset per path. Built for access logs specifically:
// nginx keeps the file open across writes and logrotate replaces it
// (create+rename or copytruncate) periodically, both of which this handles.
type Tailer struct {
	offsets map[string]int64
}

// NewTailer returns a Tailer with no prior state; the first call for any path
// starts from wherever the file currently ends, not from byte zero — a
// dashboard that just started must not replay a site's entire access log
// history as if it all happened in the first tick.
func NewTailer() *Tailer {
	return &Tailer{offsets: make(map[string]int64)}
}

// Lines returns the complete lines appended to path since the previous call
// for that same path. A partial final line — nginx mid-write — is left
// unconsumed so it is read whole on the next call instead of being split.
func (t *Tailer) Lines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// A site whose log has not been created yet (no requests since
			// setup) is not an error; it simply has nothing new to report.
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()

	offset, seen := t.offsets[path]
	switch {
	case !seen:
		// First observation of this file: start at the end. Historical
		// lines are not "new" activity for a live dashboard.
		t.offsets[path] = size
		return nil, nil
	case size < offset:
		// The file is shorter than where we left off: logrotate truncated
		// or replaced it. Start over from the current end rather than
		// seeking to a byte offset that may now belong to unrelated
		// content, or erroring out and leaving this site's row stuck.
		offset = 0
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}

	var lines []string
	r := bufio.NewReader(f)
	consumed := offset
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			lines = append(lines, line[:len(line)-1])
			consumed += int64(len(line))
		}
		if err != nil {
			break // EOF, or a partial trailing line — either way, stop here
		}
	}
	t.offsets[path] = consumed
	return lines, nil
}

// Reset drops tracked state for a path, so the next Lines call treats it as
// newly observed (starts at end-of-file rather than replaying history). Used
// when a site is removed and re-added under the same slug.
func (t *Tailer) Reset(path string) { delete(t.offsets, path) }
