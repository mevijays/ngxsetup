package security

import (
	"context"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed rules/*.yar
var embeddedRules embed.FS

// YARA wraps the yara binary, when installed, against the bundled ruleset —
// and optionally an operator-supplied directory of additional rules, so a
// larger, separately maintained ruleset (a local checkout of an open-source
// project, kept current on its own update schedule) can supplement the
// built-in set without this package having to vendor and keep it in sync
// itself.
type YARA struct {
	Runner Runner
	// ExtraRulesDir, if set, is scanned for *.yar / *.yara files to compile
	// alongside the bundled ruleset.
	ExtraRulesDir string
}

// Available reports whether the yara binary is on PATH.
func (y YARA) Available() bool { return y.Runner.Look("yara") }

// Scan compiles the ruleset (bundled plus ExtraRulesDir, if any) into a
// temporary directory and runs a recursive match against root.
func (y YARA) Scan(ctx context.Context, root string) ([]Finding, error) {
	ruleDir, cleanup, err := y.materializeRules()
	if err != nil {
		return nil, fmt.Errorf("preparing YARA rules: %w", err)
	}
	defer cleanup()

	var findings []Finding
	err = filepath.WalkDir(ruleDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".yar") && !strings.HasSuffix(path, ".yara") {
			return nil
		}
		// yara's own exit code is 1 on a rule error, which combined with a
		// scan that also legitimately finds nothing would be
		// indistinguishable from success; -w treats warnings (like the
		// "may slow down scanning" hint on the base64 rule) as non-fatal
		// rather than aborting the file's rules entirely.
		out, _ := y.Runner.CombinedOutput(ctx, "yara", "-r", "-w", path, root)
		findings = append(findings, parseYARAOutput(out, root)...)
		return nil
	})
	return findings, err
}

// materializeRules writes the embedded ruleset to a temporary directory —
// yara needs real files on disk, not an in-memory FS — and returns that
// directory (which includes ExtraRulesDir's contents too, if set) along with
// a cleanup function.
func (y YARA) materializeRules() (dir string, cleanup func(), err error) {
	tmp, err := os.MkdirTemp("", "ngxsetup-yara-*")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { os.RemoveAll(tmp) }

	entries, err := embeddedRules.ReadDir("rules")
	if err != nil {
		cleanup()
		return "", nil, err
	}
	for _, e := range entries {
		data, err := embeddedRules.ReadFile("rules/" + e.Name())
		if err != nil {
			cleanup()
			return "", nil, err
		}
		if err := os.WriteFile(filepath.Join(tmp, e.Name()), data, 0o644); err != nil {
			cleanup()
			return "", nil, err
		}
	}

	if y.ExtraRulesDir != "" {
		extra, err := os.ReadDir(y.ExtraRulesDir)
		if err == nil { // a missing/unreadable extra dir degrades to "just the bundled rules," not an error
			for _, e := range extra {
				if e.IsDir() {
					continue
				}
				name := e.Name()
				if !strings.HasSuffix(name, ".yar") && !strings.HasSuffix(name, ".yara") {
					continue
				}
				data, err := os.ReadFile(filepath.Join(y.ExtraRulesDir, name))
				if err != nil {
					continue
				}
				// Prefixed so an externally supplied file cannot silently
				// shadow (and thereby disable) a bundled rule of the same
				// name.
				os.WriteFile(filepath.Join(tmp, "extra-"+name), data, 0o644)
			}
		}
	}
	return tmp, cleanup, nil
}

// parseYARAOutput parses yara -r's default output format, one match per
// line: "<rule name> <file path>". Kept separate from Scan so the parsing
// itself is testable against fixed sample output without running yara.
func parseYARAOutput(out, root string) []Finding {
	var findings []Finding
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "warning:") || strings.HasPrefix(line, "error:") {
			continue
		}
		ruleName, path, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(path, root), "/")
		desc, severity := yaraRuleMeta(ruleName)
		findings = append(findings, Finding{
			Severity: severity, Category: CategoryMalware,
			Title:  "YARA match: " + ruleName,
			Detail: desc,
			Path:   rel,
			Fix:    "inspect the matched file; if it is not something you added, treat the site as compromised",
		})
	}
	return findings
}

// yaraRuleMeta gives a human description and severity for a rule name,
// mirroring the meta.description / meta.severity fields the .yar files
// themselves declare. yara's plain-text output does not include rule meta
// without --print-meta and a stricter parse, so this keeps the mapping
// alongside the rules it describes rather than parsing yara's own
// meta-printing format.
func yaraRuleMeta(rule string) (description string, severity Severity) {
	switch rule {
	case "eval_of_decoded_input":
		return "eval() applied to decoded or decompressed input", Critical
	case "eval_or_assert_of_request_input":
		return "eval() or assert() executed directly on HTTP request input", Critical
	case "shell_exec_of_request_input":
		return "a shell command built directly from HTTP request input", Critical
	case "preg_replace_e_modifier":
		return "preg_replace() with the deprecated /e (eval) modifier", Critical
	case "self_obfuscating_eval_marker":
		return "base64-encoded \"eval(\" marker, a common obfuscation trick", Warning
	case "known_webshell_identifier":
		return "self-identification string used by a widely circulated public webshell", Critical
	case "wp_fake_plugin_header_backdoor":
		return "a file with a WordPress plugin header but a request-driven eval/exec body", Critical
	case "uploader_backdoor":
		return "an unauthenticated file-upload handler, often a first-stage payload", Critical
	case "long_base64_blob":
		return "a very long base64-like string literal, commonly used to hide a payload", Warning
	default:
		return "matched an externally supplied YARA rule (" + rule + ")", Warning
	}
}
