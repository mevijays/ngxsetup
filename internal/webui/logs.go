package webui

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ngxsetup/internal/provision"
)

// logSource is one file or systemd-journal unit the log viewer can show.
// Kept as data rather than a hard-coded switch in the handler, so the
// per-site sources fall naturally out of whatever sites are registered right
// now instead of needing their own branch.
type logSource struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Category string `json:"category"` // "system" or the site's domain
	Kind     string `json:"kind"`     // "file" or "journal"
	Path     string `json:"path,omitempty"`
	Unit     string `json:"unit,omitempty"`
	Exists   bool   `json:"exists"`
}

// logSources builds the list of everything the viewer can currently show.
// File sources are checked for existence right now — a fresh install has no
// FastCGI cache entries yet and possibly no fail2ban log line ever written,
// and the picker should say so rather than offer a source that 404s the
// moment it's selected.
func logSources(c *provision.Ctx) []logSource {
	var out []logSource
	add := func(s logSource) {
		if s.Kind == "file" {
			s.Exists = fileExists(s.Path)
		} else {
			s.Exists = true // journal units are assumed present; reading reports the real error
		}
		out = append(out, s)
	}

	add(logSource{Key: "fail2ban", Label: "fail2ban", Category: "system", Kind: "file", Path: "/var/log/fail2ban.log"})
	add(logSource{Key: "auth", Label: "Authentication (SSH, sudo)", Category: "system", Kind: "file", Path: "/var/log/auth.log"})
	add(logSource{Key: "nginx-default", Label: "nginx — unmatched requests", Category: "system", Kind: "file", Path: "/var/log/nginx/default.access.log"})
	add(logSource{Key: "web-ui", Label: "ngxsetup web — audit log", Category: "system", Kind: "file", Path: auditLogPath})
	if c.DBUnit != "" {
		add(logSource{Key: "database", Label: "Database (" + c.DBUnit + ")", Category: "system", Kind: "journal", Unit: c.DBUnit})
	}
	if fileExists(filepath.Join(provision.DBLogDir, "slow.log")) {
		add(logSource{Key: "database-slow", Label: "Database — slow query log", Category: "system", Kind: "file", Path: filepath.Join(provision.DBLogDir, "slow.log")})
	}

	for _, site := range c.State.Sites {
		add(logSource{Key: "site-access-" + site.Slug, Label: "Access log", Category: site.Domain, Kind: "file",
			Path: fmt.Sprintf("/var/log/nginx/%s.access.log", site.Slug)})
		add(logSource{Key: "site-error-" + site.Slug, Label: "Error log", Category: site.Domain, Kind: "file",
			Path: fmt.Sprintf("/var/log/nginx/%s.error.log", site.Slug)})
		add(logSource{Key: "site-phpfpm-" + site.Slug, Label: "PHP-FPM log", Category: site.Domain, Kind: "file",
			Path: filepath.Join(provision.PHPLogDir, site.Slug+"-fpm.log")})
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			// "system" sorts first; everything else (domains) alphabetically after.
			if out[i].Category == "system" {
				return true
			}
			if out[j].Category == "system" {
				return false
			}
			return out[i].Category < out[j].Category
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func findLogSource(c *provision.Ctx, key string) (logSource, bool) {
	for _, s := range logSources(c) {
		if s.Key == key {
			return s, true
		}
	}
	return logSource{}, false
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// maxTailWindow bounds how far back a snapshot read looks, and how much a
// single live-tail poll returns — the "make it light" constraint made
// concrete: this endpoint never reads more than this many bytes off disk
// per request no matter how large the underlying log file has grown.
const maxTailWindow = 256 * 1024

// snapshotTail returns roughly the last n lines of a file without reading
// the whole thing — it seeks to a bounded window before the end and splits
// what it finds there, the same trade-off `tail` itself makes on a file
// with no line-index.
func snapshotTail(path string, n int) ([]string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := info.Size()

	start := int64(0)
	if size > maxTailWindow {
		start = size - maxTailWindow
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, 0, err
	}

	lines := strings.Split(string(data), "\n")
	if start > 0 && len(lines) > 0 {
		lines = lines[1:] // the read window likely starts mid-line; drop the partial fragment
	}
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // trailing newline produces one empty element
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, size, nil
}

// tailFrom returns every complete line written to path since offset, and the
// new offset to poll from next time. A line is only returned once its
// trailing newline has actually been written — the same reason real `tail
// -f` does not print a line until it is finished — so offset only ever
// advances to the last newline seen, never mid-line.
//
// If offset is beyond the file's current size, the file is treated as
// rotated or truncated and reading resumes from the start — the practical
// consequence of a byte offset being the only cursor this endpoint keeps,
// rather than an inode-tracking watch the way logrotate-aware tools do it.
func tailFrom(path string, offset int64) (lines []string, newOffset int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}
	size := info.Size()
	if offset > size {
		offset = 0
	}
	if offset == size {
		return nil, size, nil
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	toRead := size - offset
	if toRead > maxTailWindow {
		toRead = maxTailWindow
	}
	buf := make([]byte, toRead)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, offset, err
	}
	data := buf[:n]

	idx := bytes.LastIndexByte(data, '\n')
	if idx < 0 {
		// No complete line in this window yet; wait for more to be written.
		return nil, offset, nil
	}
	newOffset = offset + int64(idx) + 1
	complete := data[:idx]
	if len(complete) == 0 {
		return nil, newOffset, nil
	}
	lines = strings.Split(string(complete), "\n")
	return lines, newOffset, nil
}
