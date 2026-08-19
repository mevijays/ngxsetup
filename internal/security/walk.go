package security

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// maxScannedFileSize skips anything larger — a legitimate PHP source file is
// essentially never this big, and reading multi-megabyte binaries into
// memory to regex-scan them is wasted work that only slows a scan down
// without ever finding anything.
const maxScannedFileSize = 4 << 20 // 4 MB

// scannableExt is what ScanContent's rules are meaningful against. Anything
// else (images, CSS, fonts) is skipped for the pattern-matching layer, though
// the uploads and double-extension checks below apply regardless of content.
var scannableExt = map[string]bool{
	".php": true, ".phtml": true, ".php3": true, ".php4": true,
	".php5": true, ".php7": true, ".phar": true, ".inc": true,
}

// doubleExtension catches the classic upload-filter bypass: naming a PHP
// payload so it looks like an image to a naive extension check
// ("avatar.php.jpg") while still ending in something a misconfigured server
// would execute, or being executable regardless of the decoy extension on
// servers that pattern-match rather than check the final extension.
var doubleExtension = regexp.MustCompile(`(?i)\.(?:jpg|jpeg|png|gif|pdf|zip|txt|ico)\.(?:php\d?|phtml|phar)$`)

// isUnderUploads reports whether a path (relative to the site's document
// root) sits inside wp-content/uploads — a directory WordPress itself never
// writes PHP into, which makes any PHP file there inherently suspicious
// regardless of its content.
func isUnderUploads(relPath string) bool {
	rel := filepath.ToSlash(relPath)
	return strings.Contains(rel, "wp-content/uploads/")
}

// WalkAndScan applies the built-in heuristics (and the path-based checks
// above) to every scannable file under root, which is expected to be one
// site's document root.
func WalkAndScan(root string) ([]Finding, error) {
	var findings []Finding

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable file must not abort the whole scan
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		ext := strings.ToLower(filepath.Ext(path))

		if doubleExtension.MatchString(path) {
			findings = append(findings, Finding{
				Severity: Critical, Category: CategoryHeuristic,
				Title:  "double file extension disguising an executable script",
				Detail: "named to look like a non-executable file (image, archive, document) while ending in an extension PHP will still execute",
				Path:   rel,
				Fix:    "if you did not upload this file, remove it and treat the site as compromised; then audit how it got there (an upload handler that trusts the client-supplied extension is the usual cause)",
			})
		}

		if (ext == ".php" || ext == ".phtml" || ext == ".phar") && isUnderUploads(rel) {
			findings = append(findings, Finding{
				Severity: Critical, Category: CategoryHeuristic,
				Title:  "PHP file inside wp-content/uploads",
				Detail: "WordPress never places executable PHP in the uploads directory itself; a PHP file here almost always means an upload filter was bypassed",
				Path:   rel,
				Fix:    "remove the file, then confirm the nginx hardening snippet that blocks PHP execution under uploads/ is in place (ngxsetup writes this by default — check for local edits if it is missing)",
			})
		}

		if !scannableExt[ext] {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxScannedFileSize || info.Size() == 0 {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, f := range ScanContent(rel, content) {
			findings = append(findings, f)
		}
		return nil
	})
	return findings, err
}
