// Package migrate imports a WordPress site from another Linux server: it
// reads that server's nginx vhosts and wp-config.php files over SSH, then
// for each site an operator selects, dumps and transfers its database and
// rsyncs its document root onto this machine.
//
// It shells out to ssh, rsync and mysqldump rather than reimplementing any
// of those protocols — the same reasoning the rest of this codebase already
// applies to nginx, wp-cli and borg: these are exactly the tools built for
// this, already installed almost everywhere, and reimplementing SSH or
// rsync's delta-transfer algorithm in Go would be a large amount of new
// surface for a security-sensitive protocol this tool has no business
// re-inventing. rsync in particular is why a dropped connection can resume
// instead of restarting: re-running it against the same destination with
// --partial only transfers what is still missing.
package migrate

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// RemoteConfig identifies the server to migrate from and how to reach it.
type RemoteConfig struct {
	Host string
	Port int    // 0 means 22
	User string // "root", or an account with passwordless sudo

	// KeyPath is a private key file already on disk, mode 0600 — ssh
	// refuses a key with looser permissions. The caller owns writing and
	// removing this file; see provision.Ctx's migration job, which keeps it
	// in a job-scoped temp directory for exactly as long as the job runs.
	KeyPath string
	// KnownHostsPath is a per-job known_hosts file rather than the
	// operating user's own ~/.ssh/known_hosts, so a migration never
	// silently trusts (or pollutes) whatever that account has accepted
	// before, and leaves nothing behind once the job's temp directory is
	// cleaned up.
	KnownHostsPath string

	// Sudo means User is not root, so every remote command needs `sudo -n`
	// in front of it to read root-owned nginx config and another account's
	// wp-config.php. The -n (non-interactive) means a remote sudo that
	// would prompt for a password fails with a clear error instead of
	// hanging forever waiting for one that can never arrive over this
	// connection.
	Sudo bool
}

func (cfg RemoteConfig) port() int {
	if cfg.Port > 0 {
		return cfg.Port
	}
	return 22
}

func (cfg RemoteConfig) target() string { return cfg.User + "@" + cfg.Host }

// sshOptions are the connection flags shared by every ssh and rsync -e
// invocation.
func (cfg RemoteConfig) sshOptions() []string {
	return []string{
		"-i", cfg.KeyPath,
		"-p", strconv.Itoa(cfg.port()),
		"-o", "BatchMode=yes", // never prompt; a key that does not work must fail fast, not hang
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + cfg.KnownHostsPath,
		"-o", "ConnectTimeout=15",
	}
}

func (cfg RemoteConfig) sshArgs() []string {
	return append(append([]string{}, cfg.sshOptions()...), cfg.target())
}

// remoteShellCommand wraps cmd in `sudo -n sh -c '...'` when the connection
// is not already root — a single, consistently-quoted remote invocation
// regardless of how many pipes or redirects cmd itself contains.
func (cfg RemoteConfig) remoteShellCommand(cmd string) string {
	if !cfg.Sudo {
		return cmd
	}
	return "sudo -n sh -c " + shellQuote(cmd)
}

// shellQuote wraps a value in single quotes for safe interpolation into a
// command a remote shell will parse — the one place in this package a
// value legitimately needs shell quoting, since sudo's argv genuinely is
// "a shell command run for you," not something this process's own
// exec.Command has to interpret.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Client drives one remote host.
type Client struct {
	Cfg RemoteConfig
	// Log receives one line for every meaningful step, including retry
	// attempts — the migration log an operator watches live in the web UI
	// or CLI. Nil discards it (used by pure unit tests).
	Log func(string)
}

func (c Client) log(format string, args ...any) {
	if c.Log != nil {
		c.Log(fmt.Sprintf(format, args...))
	}
}

// Run executes one command on the remote host and returns its combined
// stdout+stderr. Not retried on its own — callers that need retry wrap this
// in Retry, since not every call site wants the same attempt budget (a
// quick existence check should not spend 20 attempts the way a multi-hour
// database dump legitimately might).
func (c Client) Run(ctx context.Context, cmd string) (string, error) {
	args := append(append([]string{}, c.Cfg.sshOptions()...), c.Cfg.target(), "--", c.Cfg.remoteShellCommand(cmd))
	out, err := runCommand(ctx, "", "ssh", args...)
	if err != nil {
		return out, fmt.Errorf("remote command failed: %w\n%s", err, strings.TrimSpace(out))
	}
	return out, nil
}

// RunRetry is Run wrapped in Retry — but only actually retries a
// connection-level failure. ssh's own exit-code convention is what makes
// the distinction possible without parsing any output: it exits 255 when
// ssh itself could not establish or maintain the connection, and passes
// the remote command's own exit status through unchanged for anything
// from 0 to 254. A `cat` of a file that does not exist is exit code 1 —
// the connection worked fine and the answer is simply "no" — and no
// number of retries turns that into a "yes." Confirmed live: without this
// distinction, discovery probing a candidate path that is not a
// WordPress site (nginx's own ACME-challenge webroot among them) spent
// several minutes retrying a deterministic "file not found" 20 times
// before this fix.
func (c Client) RunRetry(ctx context.Context, maxAttempts int, cmd string) (string, error) {
	var out string
	err := Retry(ctx, maxAttempts, c.Log, func() error {
		var runErr error
		out, runErr = c.Run(ctx, cmd)
		if runErr != nil && !isSSHConnectionFailure(runErr) {
			return StopRetrying(runErr)
		}
		return runErr
	})
	return out, err
}

// isSSHConnectionFailure reports whether err came from ssh itself failing
// to connect (exit 255) rather than from the remote command it ran
// exiting non-zero on its own. An error that is not even an *exec.ExitError
// (ssh not found locally, a context deadline) is treated as a connection
// failure too — genuinely unknown failure modes keep the previous
// always-retry behaviour rather than risk giving up on something that
// really was transient.
func isSSHConnectionFailure(err error) bool {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode() == 255
	}
	return true
}

// writeRemoteFile creates a file on the remote host with the given content
// and mode, via ssh's own stdin — the file's content, which may be a
// database credential, is piped through the connection rather than ever
// appearing as a command-line argument either locally (visible via
// /proc/<pid>/cmdline on this machine) or on the remote end (visible via
// its own /proc, or in its shell history).
func (c Client) writeRemoteFile(ctx context.Context, path, content, mode string) error {
	cmd := fmt.Sprintf("cat > %s && chmod %s %s", shellQuote(path), mode, shellQuote(path))
	args := append(append([]string{}, c.Cfg.sshOptions()...), c.Cfg.target(), "--", c.Cfg.remoteShellCommand(cmd))
	return Retry(ctx, DefaultMaxAttempts, c.Log, func() error {
		out, err := runCommand(ctx, content, "ssh", args...)
		if err == nil {
			return nil
		}
		wrapped := fmt.Errorf("writing %s on remote host: %w\n%s", path, err, strings.TrimSpace(out))
		if !isSSHConnectionFailure(err) {
			return StopRetrying(wrapped)
		}
		return wrapped
	})
}

// TestConnection confirms the key, user and host actually work before
// anything else is attempted — the single check that turns "wrong
// password" or "wrong path to the key file" into one clear error instead
// of a confusing failure three steps into discovery.
func (c Client) TestConnection(ctx context.Context) error {
	out, err := c.Run(ctx, "echo ngxsetup-migrate-ok")
	if err != nil {
		return err
	}
	if !strings.Contains(out, "ngxsetup-migrate-ok") {
		return fmt.Errorf("connected, but the remote shell's response was unexpected: %q", out)
	}
	if c.Cfg.Sudo {
		if _, err := c.Run(ctx, "true"); err != nil {
			return fmt.Errorf("connected as %s, but sudo failed — configure passwordless sudo (NOPASSWD) for this account, or connect as root instead: %w", c.Cfg.User, err)
		}
	}
	return nil
}

// EnsureRemoteTool installs pkg on the remote host if the tool it provides
// is not already on its PATH — the remote-side counterpart to this
// codebase's own ensureBorgInstalled/installWPCLI pattern. Requires root or
// working sudo, which TestConnection has already confirmed by the time
// this runs.
func (c Client) EnsureRemoteTool(ctx context.Context, tool, pkg string) error {
	if _, err := c.Run(ctx, "command -v "+shellQuote(tool)); err == nil {
		return nil
	}
	c.log("installing %s on the remote host", pkg)
	_, err := c.RunRetry(ctx, DefaultMaxAttempts, "apt-get update -qq && apt-get install -y -qq "+shellQuote(pkg))
	if err != nil {
		return fmt.Errorf("installing %s on the remote host: %w", pkg, err)
	}
	return nil
}

// discoverSitesEnabledCmd lists every regular file under
// /etc/nginx/sites-enabled and its content, wrapped in delimiters
// ParseSitesEnabled knows how to split back apart — one round trip for
// every vhost on the box rather than one per file, since each round trip
// is a full SSH exchange worth retrying for.
const discoverSitesEnabledCmd = `for f in /etc/nginx/sites-enabled/*; do ` +
	`[ -f "$f" ] || continue; ` +
	`echo "===NGXFILE:$f==="; cat "$f" 2>/dev/null; echo "===NGXFILE_END==="; ` +
	`done`

// DiscoverVHosts lists every vhost configured on the remote host's nginx.
func (c Client) DiscoverVHosts(ctx context.Context) ([]NginxVHost, error) {
	raw, err := c.RunRetry(ctx, DefaultMaxAttempts, discoverSitesEnabledCmd)
	if err != nil {
		return nil, fmt.Errorf("reading /etc/nginx/sites-enabled on the remote host: %w", err)
	}
	return ParseSitesEnabled(raw), nil
}

// ReadWPConfig reads and parses <root>/wp-config.php. A missing or
// unparseable file is reported through the bool return, not an error —
// "this vhost is not a WordPress site this tool can migrate" is an
// expected, common outcome of discovery, not a failure of the connection.
func (c Client) ReadWPConfig(ctx context.Context, root string) (WPConfigInfo, bool) {
	wpConfigPath := path.Join(root, "wp-config.php")
	raw, err := c.RunRetry(ctx, DefaultMaxAttempts, "cat "+shellQuote(wpConfigPath))
	if err != nil {
		return WPConfigInfo{}, false
	}
	return ParseWPConfig(raw)
}

// DumpDatabase runs mysqldump on the remote host, writing a gzip-compressed
// dump to remoteDumpPath. The site's own database credentials — not root —
// authenticate it, via a temporary defaults file rather than a command-line
// argument, and that file is removed immediately afterward regardless of
// whether the dump itself succeeded.
func (c Client) DumpDatabase(ctx context.Context, info WPConfigInfo, remoteDumpPath string) error {
	remoteDir := path.Dir(remoteDumpPath)
	if _, err := c.RunRetry(ctx, DefaultMaxAttempts, fmt.Sprintf("mkdir -p %s && chmod 700 %s", shellQuote(remoteDir), shellQuote(remoteDir))); err != nil {
		return fmt.Errorf("preparing a staging directory on the remote host: %w", err)
	}

	cnfPath := remoteDir + "/.my.cnf"
	cnfContent := fmt.Sprintf("[client]\nuser=%s\npassword=%s\nhost=%s\n", info.DBUser, info.DBPassword, info.DBHost)
	if err := c.writeRemoteFile(ctx, cnfPath, cnfContent, "600"); err != nil {
		return fmt.Errorf("writing remote database credentials file: %w", err)
	}
	defer func() { _, _ = c.Run(ctx, "rm -f "+shellQuote(cnfPath)) }()

	dumpCmd := fmt.Sprintf(
		"mysqldump --defaults-extra-file=%s --single-transaction --quick --no-tablespaces --default-character-set=utf8mb4 %s | gzip -c > %s",
		shellQuote(cnfPath), shellQuote(info.DBName), shellQuote(remoteDumpPath))
	if _, err := c.RunRetry(ctx, DefaultMaxAttempts, dumpCmd); err != nil {
		return fmt.Errorf("dumping the remote database: %w", err)
	}
	return nil
}

// RemoveRemotePath deletes one file or directory on the remote host — used
// to clean up the staging dump this package created, never anything the
// operator's site itself owns.
func (c Client) RemoveRemotePath(ctx context.Context, remotePath string) {
	_, _ = c.Run(ctx, "rm -rf "+shellQuote(remotePath))
}

// ---- transfer (rsync) ------------------------------------------------------

// TransferProgress reports one line of rsync's own progress output, parsed
// for a percentage where rsync's --info=progress2 provides one.
type TransferProgress struct {
	Line    string
	Percent int // -1 when this line carried no percentage
}

var rsyncPercent = regexp.MustCompile(`(\d{1,3})%`)

// RsyncPull copies remotePath (a file or, with a trailing slash, a
// directory's contents) from the remote host to localPath, retried up to
// maxAttempts times. rsync's own --partial keeps whatever a failed attempt
// already transferred, so a retry resumes rather than restarting — this is
// the concrete mechanism behind "a dropped connection resumes instead of
// starting the large site over."
func (c Client) RsyncPull(ctx context.Context, maxAttempts int, remotePath, localPath string, excludes []string, onProgress func(TransferProgress)) error {
	sshCmd := "ssh " + strings.Join(quoteAll(c.Cfg.sshOptions()), " ")
	args := []string{
		"-az", "--partial", "--info=progress2", "--timeout=60",
		"-e", sshCmd,
	}
	if c.Cfg.Sudo {
		args = append(args, "--rsync-path=sudo -n rsync")
	}
	for _, ex := range excludes {
		args = append(args, "--exclude="+ex)
	}
	args = append(args, c.Cfg.target()+":"+remotePath, localPath)

	return Retry(ctx, maxAttempts, c.Log, func() error {
		out, err := runStreaming(ctx, "rsync", args, func(line string) {
			if onProgress == nil {
				return
			}
			p := TransferProgress{Percent: -1, Line: line}
			if m := rsyncPercent.FindStringSubmatch(line); m != nil {
				if pct, perr := strconv.Atoi(m[1]); perr == nil {
					p.Percent = pct
				}
			}
			onProgress(p)
		})
		if err != nil {
			return fmt.Errorf("rsync: %w\n%s", err, lastLines(out, 10))
		}
		return nil
	})
}

func quoteAll(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = shellQuote(a)
	}
	return out
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ---- local process execution -----------------------------------------------

// runCommand runs a local command (ssh, mysqldump on this side, etc.),
// optionally with stdin content, returning combined stdout+stderr.
func runCommand(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// runStreaming runs a local command and calls onLine for every line of
// output as it arrives, in addition to returning the full combined output
// once the command exits — what rsync's live percentage needs, since
// buffering the whole run before parsing it would only ever report 0% and
// then 100%.
//
// rsync (and many progress-reporting tools) redraw a single line with '\r'
// rather than emitting '\n' for every update, so lines are split on either.
func runStreaming(ctx context.Context, name string, args []string, onLine func(string)) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	pr, pw, err := os.Pipe()
	if err != nil {
		return "", err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	var full bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(pr)
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		scanner.Split(scanLinesOrCR)
		for scanner.Scan() {
			line := scanner.Text()
			full.WriteString(line)
			full.WriteByte('\n')
			if onLine != nil && strings.TrimSpace(line) != "" {
				onLine(line)
			}
		}
	}()

	startErr := cmd.Start()
	if startErr != nil {
		pw.Close()
		pr.Close()
		return "", startErr
	}
	runErr := cmd.Wait()
	pw.Close()
	<-done
	pr.Close()
	return full.String(), runErr
}

// scanLinesOrCR is a bufio.SplitFunc that splits on '\n' or '\r', whichever
// comes first — see runStreaming.
func scanLinesOrCR(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
