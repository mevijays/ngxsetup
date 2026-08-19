// Package security scans a WordPress site for signs of compromise: tampered
// core or plugin files, malware matching known signatures, suspicious code
// patterns, and administrative accounts that should not be there.
//
// Detection is layered, each layer independent so the absence of one degrades
// rather than disables the scan:
//
//  1. wp-cli checksum verification — authoritative for anything hosted on
//     wordpress.org: an exact byte comparison against the checksums
//     wordpress.org itself publishes for that exact version. No signature
//     database, no false positives, but only covers .org-hosted code.
//  2. ClamAV, if installed — a real, actively maintained open-source
//     signature database (millions of signatures, updated via freshclam),
//     the same engine widely used to scan uploads and mail attachments.
//  3. YARA, if installed — pattern-based detection against a bundled
//     ruleset targeting common PHP webshell and obfuscation techniques, with
//     support for pointing at a larger externally maintained ruleset.
//  4. Built-in heuristics — regex patterns for the well-known shapes of
//     obfuscated PHP malware (eval+base64 chains, backdoor markers). Always
//     runs; the fallback when nothing else is installed.
//
// Every layer is optional except the last, so a scan run on a machine with
// none of the external tools installed still does something rather than
// nothing — and says plainly which layers it could not run, rather than
// silently reporting a clean bill of health from a fraction of the checks a
// fully-equipped scan would have made.
package security

import "fmt"

// Severity ranks how urgently a finding needs attention.
type Severity int

const (
	Info Severity = iota
	Warning
	Critical
)

func (s Severity) String() string {
	switch s {
	case Critical:
		return "critical"
	case Warning:
		return "warning"
	default:
		return "info"
	}
}

// Category groups findings by what kind of problem they represent, so a
// report can be summarized ("3 tampered files, 1 suspicious admin account")
// without an operator reading every line.
type Category string

const (
	CategoryIntegrity  Category = "integrity"  // a file differs from what it should be
	CategoryMalware    Category = "malware"    // matched a signature database
	CategoryHeuristic  Category = "heuristic"  // matched a built-in suspicious-pattern rule
	CategoryOutdated   Category = "outdated"   // known-old version, not necessarily compromised
	CategoryAccount    Category = "account"    // an administrative account worth reviewing
	CategoryPermission Category = "permission" // a file or directory mode that should not exist
)

// Finding is one thing the scan noticed.
type Finding struct {
	Severity Severity
	Category Category
	Title    string
	Detail   string
	// Path is the file this finding is about, relative to the site's
	// document root when known. Empty for findings that are not about one
	// specific file (an admin account, a missing scan layer).
	Path string
	// Fix is what to do about it, in the same spirit as doctor's findings —
	// a scanner that says "problem found" without saying what to do about it
	// has done half the job.
	Fix string
}

func (f Finding) String() string {
	if f.Path != "" {
		return fmt.Sprintf("[%s] %s: %s (%s)", f.Severity, f.Title, f.Detail, f.Path)
	}
	return fmt.Sprintf("[%s] %s: %s", f.Severity, f.Title, f.Detail)
}

// Report is the complete result of scanning one site.
type Report struct {
	Domain string
	// LayersRun records which detection layers actually executed, and
	// LayersSkipped why the others did not — a scan is not the same
	// statement of confidence with four layers running as with one, and the
	// report says so explicitly rather than looking identical either way.
	LayersRun     []string
	LayersSkipped map[string]string // layer name -> reason
	Findings      []Finding
}

// Add appends a finding. A small helper, but every call site constructing a
// Finding by hand is a call site that can forget a field; scanners route
// through this instead.
func (r *Report) Add(sev Severity, cat Category, title, detail, path, fix string) {
	r.Findings = append(r.Findings, Finding{
		Severity: sev, Category: cat, Title: title, Detail: detail, Path: path, Fix: fix,
	})
}

// CountBySeverity summarizes the report for a one-line status.
func (r Report) CountBySeverity() (critical, warning, info int) {
	for _, f := range r.Findings {
		switch f.Severity {
		case Critical:
			critical++
		case Warning:
			warning++
		default:
			info++
		}
	}
	return
}

// Clean reports whether the scan found nothing above informational severity.
func (r Report) Clean() bool {
	c, w, _ := r.CountBySeverity()
	return c == 0 && w == 0
}
