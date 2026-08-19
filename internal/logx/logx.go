// Package logx is the single output path for everything the CLI prints.
//
// Server provisioning is a long, mostly-unattended operation, so the output has
// two jobs: keep an operator oriented while it runs, and leave a transcript that
// explains what changed after it finishes. Every mutation the tool performs goes
// through Step/Change so the transcript is complete by construction.
package logx

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

var (
	mu       sync.Mutex
	out      io.Writer = os.Stdout
	errOut   io.Writer = os.Stderr
	level              = LevelInfo
	useColor           = isTTY(os.Stdout)
	indent   int
	warnings []string
)

// ANSI codes, blanked out when stdout is not a terminal.
const (
	cReset  = "\033[0m"
	cDim    = "\033[2m"
	cBold   = "\033[1m"
	cRed    = "\033[31m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cBlue   = "\033[34m"
	cCyan   = "\033[36m"
)

func isTTY(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// SetLevel raises or lowers verbosity. Debug shows every command executed.
func SetLevel(l Level) { mu.Lock(); level = l; mu.Unlock() }

// SetOutput redirects output, used by tests.
func SetOutput(w, e io.Writer) { mu.Lock(); out, errOut, useColor = w, e, false; mu.Unlock() }

func colorize(code, s string) string {
	if !useColor {
		return s
	}
	return code + s + cReset
}

func emit(w io.Writer, s string) {
	fmt.Fprint(w, strings.Repeat("  ", indent), s, "\n")
}

// Section prints a top-level heading. Sections do not nest.
func Section(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	indent = 0
	emit(out, "")
	emit(out, colorize(cBold+cBlue, "▸ "+fmt.Sprintf(format, a...)))
	indent = 1
}

// Step reports work about to happen inside a section.
func Step(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	if level > LevelInfo {
		return
	}
	emit(out, colorize(cCyan, "· ")+fmt.Sprintf(format, a...))
}

// Change records a mutation that actually landed on the system. Anything that
// writes a file, installs a package or restarts a service reports through here,
// which is what makes the final transcript trustworthy.
func Change(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	if level > LevelInfo {
		return
	}
	emit(out, colorize(cGreen, "✓ ")+fmt.Sprintf(format, a...))
}

// Skip records a no-op: the desired state was already in place. Distinguishing
// this from Change is what makes re-running setup readable.
func Skip(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	if level > LevelInfo {
		return
	}
	emit(out, colorize(cDim, "= "+fmt.Sprintf(format, a...)))
}

// Info prints plain informational output.
func Info(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	if level > LevelInfo {
		return
	}
	emit(out, fmt.Sprintf(format, a...))
}

// Debug prints only under --verbose.
func Debug(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	if level > LevelDebug {
		return
	}
	emit(out, colorize(cDim, "  "+fmt.Sprintf(format, a...)))
}

// Warn records a non-fatal problem. Warnings are also buffered so they can be
// replayed at the end of a long run, where they would otherwise scroll away.
func Warn(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	msg := fmt.Sprintf(format, a...)
	warnings = append(warnings, msg)
	if level > LevelWarn {
		return
	}
	emit(errOut, colorize(cYellow, "! "+msg))
}

// Error prints a fatal-class message. It does not exit; callers decide.
func Error(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	emit(errOut, colorize(cRed, "✗ "+fmt.Sprintf(format, a...)))
}

// Warnings returns everything Warn has recorded so far.
// ResetWarnings clears the buffered-warnings list. Used by long-running
// processes that call into this package many times in one lifetime (the web
// UI, one process serving many separate actions) so the buffer reflects only
// the action just run rather than growing for as long as the process lives.
func ResetWarnings() {
	mu.Lock()
	defer mu.Unlock()
	warnings = nil
}

func Warnings() []string {
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), warnings...)
}

// Summary replays buffered warnings at the end of a run.
func Summary() {
	w := Warnings()
	if len(w) == 0 {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	indent = 0
	emit(errOut, "")
	emit(errOut, colorize(cBold+cYellow, fmt.Sprintf("%d warning(s) during this run:", len(w))))
	indent = 1
	for _, m := range w {
		emit(errOut, colorize(cYellow, "! "+m))
	}
	indent = 0
}

// KV prints an aligned key/value block, used by status and info output.
func KV(pairs [][2]string) {
	mu.Lock()
	defer mu.Unlock()
	width := 0
	for _, p := range pairs {
		if len(p[0]) > width {
			width = len(p[0])
		}
	}
	for _, p := range pairs {
		emit(out, fmt.Sprintf("%s%s  %s",
			colorize(cDim, p[0]+":"),
			strings.Repeat(" ", width-len(p[0])),
			p[1]))
	}
}

// Bullet prints a plain list item at the current indent.
func Bullet(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	emit(out, "- "+fmt.Sprintf(format, a...))
}

// Raw writes a pre-formatted block (diffs, config previews) without decoration.
func Raw(s string) {
	mu.Lock()
	defer mu.Unlock()
	fmt.Fprint(out, s)
	if !strings.HasSuffix(s, "\n") {
		fmt.Fprintln(out)
	}
}
