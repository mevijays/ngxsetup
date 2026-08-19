package provision

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxArchiveEntry bounds a single extracted file. WordPress ships nothing
// close to this; the cap exists so a hostile or corrupt archive cannot fill
// the disk during extraction.
const maxArchiveEntry = 256 << 20

// extractTarGz unpacks an archive beneath dest, optionally stripping the first
// path component (WordPress tarballs wrap everything in a `wordpress/` prefix).
//
// Every entry is checked against path traversal. An archive member named
// ../../etc/cron.d/x is the classic way to turn "download and unpack" into
// arbitrary file write as root, and Go's archive/tar does nothing to prevent
// it — the caller must.
func extractTarGz(src, dest string, stripComponents int) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s is not a gzip archive: %w", src, err)
	}
	defer gz.Close()

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	// Resolve symlinks in the destination once, so the containment check below
	// compares against the real path.
	destReal, err := filepath.EvalSymlinks(dest)
	if err != nil {
		destReal = dest
	}

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		name := hdr.Name
		if stripComponents > 0 {
			parts := strings.Split(filepath.ToSlash(name), "/")
			if len(parts) <= stripComponents {
				continue
			}
			name = strings.Join(parts[stripComponents:], "/")
		}
		if name == "" {
			continue
		}

		target := filepath.Join(destReal, filepath.Clean("/"+name))
		// Join with a rooted Clean already neutralises "..", but verify the
		// result explicitly rather than trusting that reasoning.
		if target != destReal && !strings.HasPrefix(target, destReal+string(os.PathSeparator)) {
			return fmt.Errorf("archive entry %q escapes the destination directory", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if hdr.Size > maxArchiveEntry {
				return fmt.Errorf("archive entry %q is %d bytes, over the limit", hdr.Name, hdr.Size)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, io.LimitReader(tr, maxArchiveEntry))
			closeErr := out.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink, tar.TypeLink:
			// Links are skipped rather than recreated. WordPress contains
			// none, and a link pointing outside the tree is the other half of
			// the traversal attack the check above defends against.
			continue
		}
	}
}
