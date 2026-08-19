// Package system wraps the commands and services this tool has to drive.
//
// Two rules shape the design. Errors carry the command's actual output,
// because "exit status 1" from apt is worthless to whoever is reading the
// terminal at 2am. And nothing sensitive is ever passed as a command-line
// argument, because /proc makes every process's argv readable by every user on
// the machine.
package system

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"ngxsetup/internal/logx"
)

// Runner executes external commands.
type Runner struct {
	// DryRun prints commands instead of running them. Read-only commands still
	// execute, since the tool has to be able to inspect the system in order to
	// report what it would do.
	DryRun bool
	// Timeout bounds a single command. Package installs are slow; the default
	// is generous but finite so a hung apt does not hang the tool forever.
	Timeout time.Duration
	// ExtraEnv is appended to the child's environment, for the rare command
	// that is configured through environment variables rather than flags —
	// borg reads its repository and passphrase this way. Nothing sensitive
	// belongs directly in a value here if it can be avoided (a value set this
	// way is visible to anything that can read this process's own
	// environment, e.g. /proc/<pid>/environ for another process running as
	// the same user); BORG_PASSCOMMAND is used instead of BORG_PASSPHRASE for
	// exactly this reason — see internal/borg.
	ExtraEnv []string
	// Dir sets the child's working directory. Empty means inherit this
	// process's own, the same as every call before this field existed.
	// Needed for `borg extract`, which recreates archived paths relative to
	// the current directory rather than accepting a destination flag.
	Dir string
}

const defaultTimeout = 30 * time.Minute

func (r Runner) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return defaultTimeout
}

// Look reports whether a command exists on PATH.
func (r Runner) Look(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// Run executes a command that changes system state. It is suppressed by DryRun.
func (r Runner) Run(ctx context.Context, name string, args ...string) error {
	if r.DryRun {
		logx.Change("[dry-run] would run: %s %s", name, strings.Join(args, " "))
		return nil
	}
	logx.Debug("$ %s %s", name, strings.Join(args, " "))
	_, err := r.capture(ctx, nil, name, args...)
	return err
}

// Output runs a read-only command and returns its stdout. It executes even
// under DryRun: the tool needs to inspect the machine to decide what it would
// change.
func (r Runner) Output(ctx context.Context, name string, args ...string) (string, error) {
	logx.Debug("$ %s %s", name, strings.Join(args, " "))
	out, err := r.capture(ctx, nil, name, args...)
	return strings.TrimSpace(out), err
}

// CombinedOutput runs a read-only command and returns stdout and stderr
// merged, in the order the process wrote them. Use this over Output for
// anything that parses a version banner: several common tools — `nginx -v`
// among them — write that banner to stderr rather than stdout, and Output
// alone would silently return an empty string for those. Executes even under
// DryRun, for the same reason Output does.
func (r Runner) CombinedOutput(ctx context.Context, name string, args ...string) (string, error) {
	logx.Debug("$ %s %s", name, strings.Join(args, " "))

	cctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()

	cmd := exec.CommandContext(cctx, name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	cmd.Dir = r.Dir
	cmd.Env = append(append(os.Environ(), "DEBIAN_FRONTEND=noninteractive", "LC_ALL=C"), r.ExtraEnv...)

	err := cmd.Run()
	out := strings.TrimSpace(buf.String())
	if err != nil && cctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("%s timed out after %s", name, r.timeout())
	}
	return out, err
}

// RunStdin executes a command with input piped to stdin.
//
// This is how SQL reaches the database client. Passing a statement as an
// argument would expose any credential inside it to every user on the machine
// through `ps`, which is exactly what the previous implementation did with
// `mysql -u root -p<password> -e ...`.
func (r Runner) RunStdin(ctx context.Context, stdin, name string, args ...string) (string, error) {
	if r.DryRun {
		logx.Change("[dry-run] would run: %s %s (with piped input)", name, strings.Join(args, " "))
		return "", nil
	}
	logx.Debug("$ %s %s (stdin: %d bytes)", name, strings.Join(args, " "), len(stdin))
	return r.capture(ctx, strings.NewReader(stdin), name, args...)
}

// RunStdinFile executes a command with a file's contents piped to stdin,
// streaming rather than reading the file into memory first.
//
// This is how a database restore reaches the client: a WordPress dump can run
// to hundreds of megabytes, and RunStdin's string parameter would have to hold
// the whole thing in memory (twice, briefly, once for the file read and once
// for the string) before a single byte reached the child process.
func (r Runner) RunStdinFile(ctx context.Context, path, name string, args ...string) (string, error) {
	if r.DryRun {
		logx.Change("[dry-run] would run: %s %s (with input from %s)", name, strings.Join(args, " "), path)
		return "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	logx.Debug("$ %s %s (stdin: %s)", name, strings.Join(args, " "), path)
	return r.capture(ctx, f, name, args...)
}

// TryRun runs a command whose failure is acceptable, logging it as a warning.
func (r Runner) TryRun(ctx context.Context, name string, args ...string) {
	if err := r.Run(ctx, name, args...); err != nil {
		logx.Debug("optional command failed: %s %s: %v", name, strings.Join(args, " "), err)
	}
}

func (r Runner) capture(ctx context.Context, stdin io.Reader, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()

	cmd := exec.CommandContext(cctx, name, args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	cmd.Dir = r.Dir
	if stdin != nil {
		cmd.Stdin = stdin
	}
	cmd.Env = append(append(os.Environ(),
		"DEBIAN_FRONTEND=noninteractive",
		"LC_ALL=C",
	), r.ExtraEnv...)

	err := cmd.Run()
	if err == nil {
		return out.String(), nil
	}
	if cctx.Err() == context.DeadlineExceeded {
		return out.String(), fmt.Errorf("%s timed out after %s", name, r.timeout())
	}

	// Surface what actually went wrong. stderr first, falling back to stdout,
	// since some tools report failures on stdout.
	detail := strings.TrimSpace(errBuf.String())
	if detail == "" {
		detail = strings.TrimSpace(out.String())
	}
	if detail == "" {
		return out.String(), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out.String(), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, indent(detail))
}

func indent(s string) string {
	lines := strings.Split(s, "\n")
	// A wall of output helps nobody; the tail is where the real error is.
	if len(lines) > 20 {
		lines = append([]string{"    ..."}, lines[len(lines)-20:]...)
	}
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}
