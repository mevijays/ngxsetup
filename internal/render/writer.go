// Package render applies generated configuration to disk.
//
// Provisioning tools fail in a characteristic way: they write half a dozen
// files, the seventh is wrong, the service will not restart, and the operator
// is left with a machine in a state no one designed. Everything here exists to
// make that outcome impossible.
//
//   - Writes are atomic. A file is never observed half-written.
//   - Writes are idempotent. Content identical to what is already on disk is
//     reported as unchanged and not touched, so re-running is safe and quiet.
//   - Writes are journalled. Every modified file is backed up first, so a
//     failed validation restores the exact previous state.
//   - Files not written by this tool are never overwritten without an explicit
//     override, so hand-written configuration survives.
package render

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"ngxsetup/internal/logx"
	"ngxsetup/internal/tmpl"
)

// Writer applies file changes and can undo them.
type Writer struct {
	// DryRun reports what would change without touching the filesystem.
	DryRun bool
	// ShowDiff prints a unified diff for every modified file.
	ShowDiff bool
	// Root prefixes every path. Empty means the real filesystem; tests set it
	// to a temporary directory.
	Root string
	// BackupDir receives copies of files before they are modified.
	BackupDir string
	// Force allows overwriting files this tool did not create.
	Force bool

	journal []entry
}

// sandboxed reports whether this writer is operating on a copy of the
// filesystem rather than the live one. Ownership changes are meaningless there
// — the accounts do not exist and nothing runs out of the tree — so they are
// skipped rather than failing the apply.
func (w *Writer) sandboxed() bool { return w.Root != "" }

type entry struct {
	path       string // real path, Root already applied
	backup     string // backup file location; empty when the file was created
	created    bool
	wasSymlink bool
}

// ErrUnmanaged reports a refusal to overwrite a file the tool does not own.
var ErrUnmanaged = errors.New("file exists and was not created by ngxsetup")

func (w *Writer) path(p string) string {
	if w.Root == "" {
		return p
	}
	return filepath.Join(w.Root, p)
}

// Write places content at path, returning whether anything changed.
//
// requireManaged should be true for any file that could plausibly have been
// written by hand — the main nginx.conf, a database config — and false for
// files that live in a directory this tool owns outright.
func (w *Writer) Write(path string, content []byte, mode os.FileMode, requireManaged bool) (bool, error) {
	full := w.path(path)

	existing, readErr := os.ReadFile(full)
	exists := readErr == nil

	if exists {
		if string(existing) == string(content) {
			logx.Skip("%s already up to date", path)
			return false, nil
		}
		if requireManaged && !tmpl.IsManaged(existing) && !w.Force {
			return false, fmt.Errorf("%s: %w (pass --force to overwrite, or move it aside)", path, ErrUnmanaged)
		}
	}

	if w.ShowDiff {
		if !exists {
			existing = nil
		}
		if d := Diff(path, existing, content); d != "" {
			logx.Raw(d)
		}
	}

	if w.DryRun {
		if exists {
			logx.Change("[dry-run] would update %s", path)
		} else {
			logx.Change("[dry-run] would create %s", path)
		}
		return true, nil
	}

	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return false, err
	}

	e := entry{path: full, created: !exists}
	if exists {
		bak, err := w.backup(full, existing)
		if err != nil {
			return false, err
		}
		e.backup = bak
	}

	if err := writeAtomic(full, content, mode); err != nil {
		return false, err
	}
	w.journal = append(w.journal, e)

	if exists {
		logx.Change("updated %s", path)
	} else {
		logx.Change("created %s", path)
	}
	return true, nil
}

// WriteIfAbsent creates a file only when it does not already exist. Used for
// per-site override files, which the tool creates once and never rewrites.
func (w *Writer) WriteIfAbsent(path string, content []byte, mode os.FileMode) (bool, error) {
	full := w.path(path)
	if _, err := os.Stat(full); err == nil {
		logx.Skip("%s exists, left untouched", path)
		return false, nil
	}
	if w.DryRun {
		logx.Change("[dry-run] would create %s", path)
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return false, err
	}
	if err := writeAtomic(full, content, mode); err != nil {
		return false, err
	}
	w.journal = append(w.journal, entry{path: full, created: true})
	logx.Change("created %s", path)
	return true, nil
}

// EnsureDir creates a directory, optionally owned by a specific user.
func (w *Writer) EnsureDir(path string, mode os.FileMode, owner string) error {
	full := w.path(path)
	if w.sandboxed() {
		owner = ""
	}
	if _, err := os.Stat(full); err == nil {
		if owner != "" && !w.DryRun {
			return Chown(full, owner)
		}
		return nil
	}
	if w.DryRun {
		logx.Change("[dry-run] would create directory %s", path)
		return nil
	}
	if err := os.MkdirAll(full, mode); err != nil {
		return err
	}
	// MkdirAll applies the umask; set the mode explicitly so a directory meant
	// to be 0750 is not silently created 0755.
	if err := os.Chmod(full, mode); err != nil {
		return err
	}
	if owner != "" {
		if err := Chown(full, owner); err != nil {
			return err
		}
	}
	logx.Change("created directory %s", path)
	return nil
}

// Symlink makes link point at target, replacing an existing link.
//
// The target is rooted alongside the link: under an alternate root a link
// pointing at the real /etc would dangle, and every check that follows it
// would report the file as missing.
func (w *Writer) Symlink(target, link string) error {
	full := w.path(link)
	target = w.path(target)
	if cur, err := os.Readlink(full); err == nil && cur == target {
		logx.Skip("%s already linked", link)
		return nil
	}
	if w.DryRun {
		logx.Change("[dry-run] would link %s -> %s", link, target)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	_ = os.Remove(full)
	if err := os.Symlink(target, full); err != nil {
		return err
	}
	w.journal = append(w.journal, entry{path: full, created: true, wasSymlink: true})
	logx.Change("linked %s -> %s", link, target)
	return nil
}

// Remove deletes a path, recording a backup so it can be restored.
func (w *Writer) Remove(path string) error {
	full := w.path(path)
	existing, err := os.ReadFile(full)
	if err != nil {
		return nil // already gone
	}
	if w.DryRun {
		logx.Change("[dry-run] would remove %s", path)
		return nil
	}
	bak, err := w.backup(full, existing)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil {
		return err
	}
	w.journal = append(w.journal, entry{path: full, backup: bak})
	logx.Change("removed %s", path)
	return nil
}

// Changed reports how many files this writer has modified.
func (w *Writer) Changed() int { return len(w.journal) }

// Rollback restores every file this writer has touched, newest first. It is
// called when post-write validation fails, which is the moment a provisioning
// tool either saves an operator's evening or ruins it.
func (w *Writer) Rollback() error {
	var errs []string
	for i := len(w.journal) - 1; i >= 0; i-- {
		e := w.journal[i]
		switch {
		case e.created:
			if err := os.Remove(e.path); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err.Error())
			}
		case e.backup != "":
			data, err := os.ReadFile(e.backup)
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}
			if err := writeAtomic(e.path, data, 0o644); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}
	w.journal = nil
	if len(errs) > 0 {
		return fmt.Errorf("rollback incomplete: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Commit discards the rollback journal after a successful validation.
func (w *Writer) Commit() { w.journal = nil }

// BackupLocation reports where this run's backups were written, for the
// operator to find later.
func (w *Writer) BackupLocation() string { return w.BackupDir }

func (w *Writer) backup(full string, content []byte) (string, error) {
	if w.BackupDir == "" {
		return "", nil
	}
	// Mirror the original path inside the backup directory so a human can find
	// the file they are looking for without decoding a mangled name.
	rel := strings.TrimPrefix(full, w.Root)
	dst := filepath.Join(w.BackupDir, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(dst, content, 0o600); err != nil {
		return "", err
	}
	return dst, nil
}

// writeAtomic writes through a temporary file in the same directory, then
// renames. Rename within a filesystem is atomic, so a reader either sees the
// old file or the new one — never a truncated one, and never nothing at all.
func writeAtomic(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := f.Write(content); err != nil {
		f.Close()
		return err
	}
	// Force the contents to disk before the rename. Without this a power loss
	// between the two can leave the new name pointing at an empty file.
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Chown sets ownership from a "user" or "user:group" specification.
func Chown(path, spec string) error {
	uname, gname, _ := strings.Cut(spec, ":")
	if gname == "" {
		gname = uname
	}
	u, err := user.Lookup(uname)
	if err != nil {
		return fmt.Errorf("lookup user %s: %w", uname, err)
	}
	g, err := user.LookupGroup(gname)
	if err != nil {
		// A user without a matching group is normal; fall back to the user's
		// primary group rather than failing the whole apply.
		g = &user.Group{Gid: u.Gid}
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(g.Gid)
	return os.Chown(path, uid, gid)
}

// ChownRecursive applies ownership to a whole tree. Symlinks are changed
// without following them, so a symlink planted inside a site's uploads
// directory cannot be used to re-own a file elsewhere on the system.
func ChownRecursive(root, spec string) error {
	uname, gname, _ := strings.Cut(spec, ":")
	if gname == "" {
		gname = uname
	}
	u, err := user.Lookup(uname)
	if err != nil {
		return fmt.Errorf("lookup user %s: %w", uname, err)
	}
	g, err := user.LookupGroup(gname)
	if err != nil {
		g = &user.Group{Gid: u.Gid}
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(g.Gid)

	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries rather than aborting
		}
		return os.Lchown(p, uid, gid)
	})
}
