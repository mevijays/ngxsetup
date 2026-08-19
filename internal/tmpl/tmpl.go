// Package tmpl embeds and renders every configuration file the tool writes.
//
// Rendering is deliberately free of timestamps, hostnames and random values.
// Identical inputs must produce byte-identical output, because that is what
// makes "has anything actually changed?" answerable — and therefore what makes
// diffing, idempotent re-runs and rollback meaningful.
package tmpl

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"

	"ngxsetup/internal/tuning"
)

//go:embed all:files
var files embed.FS

// ManagedHeader marks a file as owned by this tool. Anything carrying it may be
// overwritten; anything not carrying it is left alone, which is how the tool
// avoids destroying configuration a human wrote.
const ManagedHeader = "# Managed by ngxsetup. Local edits will be overwritten."

// Ident is the comment marker used to recognise our files. Kept separate from
// ManagedHeader so the check survives wording changes to the header.
const Ident = "Managed by ngxsetup"

var funcs = template.FuncMap{
	// mem renders megabytes the way nginx, PHP and MySQL all parse.
	"mem": tuning.MemString,
	"add": func(a, b int) int { return a + b },
	"mul": func(a, b int) int { return a * b },
	// mulk renders a kilobyte quantity multiplied by n, e.g. 32 × 2 -> "64k".
	"mulk": func(kb, n int) string { return fmt.Sprintf("%dk", kb*n) },
	// mulSeconds converts days to seconds for directives that want seconds.
	"mulSeconds": func(days int) int { return days * 86400 },
	"join":       strings.Join,
	"lower":      strings.ToLower,
	"quote":      func(s string) string { return `"` + s + `"` },
}

// Render executes the named embedded template against data.
//
// The comment style of the rendered file decides how the managed header is
// written: `;` for PHP ini and pool files, `#` everywhere else.
func Render(name string, data any) ([]byte, error) {
	raw, err := files.ReadFile("files/" + name)
	if err != nil {
		return nil, fmt.Errorf("embedded template %s: %w", name, err)
	}

	t, err := template.New(name).
		Funcs(funcs).
		// Fail loudly on a missing field rather than writing "<no value>" into
		// a config file, where it becomes a service that will not start.
		Option("missingkey=error").
		Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}

	var buf bytes.Buffer
	if h := headerFor(name); h != "" {
		buf.WriteString(h)
		buf.WriteByte('\n')
	}
	if err := t.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("render %s: %w", name, err)
	}
	return normalise(buf.Bytes()), nil
}

// RenderRaw returns an embedded file that needs no substitution.
func RenderRaw(name string) ([]byte, error) {
	raw, err := files.ReadFile("files/" + name)
	if err != nil {
		return nil, fmt.Errorf("embedded file %s: %w", name, err)
	}
	var buf bytes.Buffer
	buf.WriteString(headerFor(name))
	buf.WriteByte('\n')
	buf.Write(raw)
	return normalise(buf.Bytes()), nil
}

func headerFor(name string) string {
	switch {
	// A PHP file must open with `<?php`; anything before it is emitted to the
	// browser and breaks header output. These templates carry their own marker
	// inside a docblock instead.
	case strings.HasSuffix(name, ".php.tmpl"):
		return ""

	// PHP's INI parser only accepts `;` as a comment leader — a `#`-prefixed
	// line is read as a malformed key=value entry and php-fpm refuses to
	// start on it.
	//
	// The path test is a prefix, not Contains("/php/"): template names are
	// given relative to the embed root ("php/fpm-global.conf.tmpl"), with no
	// leading slash, so the old substring form never matched anything under
	// php/ at all. It only appeared to work because pool.conf.tmpl was also
	// caught by the suffix case below — which meant the next PHP template
	// added here silently got an nginx-style header and a service that would
	// not start. Confirmed live, twice, on exactly that failure.
	case strings.HasPrefix(name, "php/"), strings.HasSuffix(name, ".ini.tmpl"),
		strings.HasSuffix(name, "pool.conf.tmpl"):
		return ";; " + ManagedHeader[2:]

	default:
		return ManagedHeader
	}
}

// normalise collapses the blank-line runs that conditional template blocks
// leave behind and guarantees a single trailing newline. Without it, toggling
// an option produces a diff full of whitespace noise.
func normalise(b []byte) []byte {
	lines := strings.Split(string(b), "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if l == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	s := strings.TrimRight(strings.Join(out, "\n"), "\n")
	return []byte(s + "\n")
}

// IsManaged reports whether content carries our header and may be overwritten.
func IsManaged(content []byte) bool {
	// Only inspect the opening lines: a site's own override file could
	// legitimately mention the tool further down.
	head := content
	if len(head) > 512 {
		head = head[:512]
	}
	return bytes.Contains(head, []byte(Ident))
}
