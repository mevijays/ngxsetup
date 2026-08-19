package security

import "regexp"

// Rule is one pattern-based detector. The patterns below encode widely
// documented indicators of PHP malware — the kind of thing security writeups
// have described for over a decade — written as original regular
// expressions for this project rather than copied from any specific
// scanner's rule text.
//
// This layer exists for the gap no signature database fully closes: novel or
// hand-modified malware that does not match a known hash or byte sequence,
// but still has to get code execution into the request path somehow, and
// those mechanisms are a much smaller, much more stable set than the malware
// built on top of them.
type Rule struct {
	ID       string
	Severity Severity
	Pattern  *regexp.Regexp
	Title    string
	Detail   string
}

// builtinRules is deliberately not exhaustive. Precision matters more than
// recall here: a false positive on a site's real code teaches an operator to
// ignore the scanner, which is worse than one more missed sample. Patterns
// specific enough to name a mechanism (eval of decoded input, function-name
// string concatenation) rather than a bare function name (which would flag
// enormous amounts of legitimate WordPress and plugin code).
var builtinRules = []Rule{
	{
		ID: "eval-decoded-input", Severity: Critical,
		Pattern: regexp.MustCompile(`(?i)eval\s*\(\s*(?:base64_decode|gzinflate|gzuncompress|str_rot13|gzdecode)\s*\(`),
		Title:   "eval() of decoded/decompressed input",
		Detail:  "the single most common shape of obfuscated PHP malware: content is decoded then executed, so the actual payload never appears as plain text in the file",
	},
	{
		ID: "eval-request-input", Severity: Critical,
		Pattern: regexp.MustCompile(`(?i)(?:eval|assert)\s*\(\s*\$_(?:POST|GET|REQUEST|COOKIE)\b`),
		Title:   "eval() or assert() of a raw HTTP parameter",
		Detail:  "executes attacker-controlled request input directly — a backdoor allowing arbitrary code execution from any request",
	},
	{
		ID: "exec-request-input", Severity: Critical,
		Pattern: regexp.MustCompile(`(?i)\b(?:system|exec|shell_exec|passthru|popen|proc_open)\s*\(\s*\$_(?:POST|GET|REQUEST|COOKIE)\b`),
		Title:   "shell command built directly from a raw HTTP parameter",
		Detail:  "passes request input straight to a shell — a remote command execution backdoor, not something any WordPress core or plugin code legitimately does",
	},
	{
		ID: "preg_replace_e_modifier", Severity: Critical,
		Pattern: regexp.MustCompile(`preg_replace\s*\(\s*['"][^'"]*/e['"]`),
		Title:   "preg_replace() with the /e modifier",
		Detail:  "the /e modifier evaluates its replacement as PHP code; combined with request input this is remote code execution, and the modifier itself was removed in PHP 7 specifically because of this — its presence at all is a strong signal",
	},
	{
		ID: "obfuscated-function-call", Severity: Warning,
		Pattern: regexp.MustCompile(`\$\w+\s*=\s*(?:['"][a-z_]{1,10}['"]\s*\.\s*){2,}['"][a-z_]{0,10}['"]\s*;\s*\$\w+\s*\(`),
		Title:   "function name assembled from concatenated strings",
		Detail:  "building a function name at runtime from string fragments (e.g. 'sy'.'stem') is a common technique for hiding a dangerous call from simple text search, including from a scanner grepping for the function name directly",
	},
	{
		ID: "reverse-shell-indicator", Severity: Critical,
		Pattern: regexp.MustCompile(`(?i)fsockopen\s*\([^)]*\)[\s\S]{0,200}(?:shell_exec|system|exec|popen)\s*\(`),
		Title:   "socket connection combined with a shell command",
		Detail:  "the classic shape of a reverse shell: open a network connection out, then pipe a command interpreter through it",
	},
	{
		ID: "webshell-marker", Severity: Critical,
		Pattern: regexp.MustCompile(`(?i)\b(?:c99shell|r57shell|WSO\s*[\d.]*\s*Shell|b374k|IndoXploit|FilesMan|Mini\s*Shell|Web\s*Shell\s+by)\b`),
		Title:   "well-known webshell self-identification string",
		Detail:  "matches the name a widely circulated webshell uses to identify itself in its own source or output — legitimate code has no reason to contain this",
	},
	{
		ID: "long-base64-blob", Severity: Warning,
		Pattern: regexp.MustCompile(`['"][A-Za-z0-9+/]{800,}={0,2}['"]`),
		Title:   "very long base64-like string literal",
		Detail:  "a common way to hide a payload in plain sight; also produced by some legitimate icon/font embedding, so this alone is a reason to look, not a confirmed finding",
	},
}

// ScanContent applies every built-in rule to one file's content and returns
// what matched. Pure and allocation-light by design — this runs once per PHP
// file on a site that can have several thousand of them.
func ScanContent(path string, content []byte) []Finding {
	var findings []Finding
	for _, rule := range builtinRules {
		if rule.Pattern.Match(content) {
			findings = append(findings, Finding{
				Severity: rule.Severity,
				Category: CategoryHeuristic,
				Title:    rule.Title,
				Detail:   rule.Detail,
				Path:     path,
				Fix:      "inspect the matched code; if it is not something you added, treat the site as compromised — see MIGRATING.md-style incident guidance",
			})
		}
	}
	return findings
}
