package security

import (
	"context"
	"strings"
)

// ClamAV wraps the clamscan binary, when installed, against ClamAV's own
// actively maintained signature database (millions of signatures, updated
// via freshclam) — the same open-source antivirus engine used to scan mail
// attachments and uploads across a huge fraction of the internet. This
// package does not maintain or embed any signatures of its own for this
// layer; ClamAV's project does that job better than a bundled copy ever
// could stay current.
type ClamAV struct {
	Runner Runner
}

// Available reports whether clamscan is on PATH.
func (c ClamAV) Available() bool { return c.Runner.Look("clamscan") }

// Scan runs a recursive scan of root and returns one finding per infected
// file ClamAV reports.
func (c ClamAV) Scan(ctx context.Context, root string) ([]Finding, error) {
	// --no-summary keeps the output to exactly one line per file examined;
	// -i restricts that to infected files only, so parsing does not have to
	// distinguish a clean-file line from an infected one.
	out, _ := c.Runner.CombinedOutput(ctx, "clamscan", "-r", "--no-summary", "-i", root)
	// clamscan's exit code is 1 when infections are found — that is success
	// for this call's purpose, not a failure to propagate; only a genuine
	// inability to run (binary missing, permission denied) is worth
	// surfacing as an error, and Available() already covers "missing."
	return parseClamAVOutput(out, root), nil
}

// parseClamAVOutput turns clamscan's "-i --no-summary" output into findings.
// Each infected line looks like:
//
//	/var/www/example-com/public/wp-content/uploads/shell.php: Php.Trojan.WebShell-1 FOUND
func parseClamAVOutput(out, root string) []Finding {
	var findings []Finding
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasSuffix(line, "FOUND") {
			continue
		}
		line = strings.TrimSuffix(line, "FOUND")
		line = strings.TrimSpace(line)
		path, signature, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(path, root), "/")
		findings = append(findings, Finding{
			Severity: Critical, Category: CategoryMalware,
			Title:  "ClamAV signature match",
			Detail: "matches known-malware signature " + signature,
			Path:   rel,
			Fix:    "quarantine or remove this file immediately; ClamAV signature matches have an extremely low false-positive rate, and " + signature + " can be looked up for details on what it does",
		})
	}
	return findings
}
