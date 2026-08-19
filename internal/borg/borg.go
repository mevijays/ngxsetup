// Package borg drives BorgBackup (https://borgbackup.org) for off-box,
// deduplicated, encrypted backups of a site's files and database.
//
// Everything here is configured through environment variables rather than
// flags — BORG_REPO and BORG_PASSCOMMAND — which is borg's own recommended
// way to run it non-interactively (cron/systemd), and which keeps the
// repository location and passphrase out of argv, where /proc makes every
// process's command line readable by every user on the machine. The
// passphrase itself is never held in this process as plaintext for longer
// than it takes to write it to its file; every actual borg invocation reads
// it back out via BORG_PASSCOMMAND, exactly as a human running borg by hand
// from a terminal would.
package borg

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"ngxsetup/internal/logx"
	"ngxsetup/internal/system"
)

// PassphraseFile is where the repository passphrase lives, base64-encoded
// and root-only. Base64 rather than plaintext isn't meaningful obfuscation
// on its own — root can read either — but it's what lets the same file be
// read by both a human (`base64 -d` to check it) and BORG_PASSCOMMAND
// without either of them having to worry about a raw passphrase containing
// a newline or shell-special character on the way through a pipe.
const PassphraseFile = "/etc/ngxsetup/borg-passphrase"

// Client drives one borg repository.
type Client struct {
	Runner         system.Runner
	Repo           string // e.g. "/mnt/backup/ngxsetup" or "ssh://user@host:2222/./ngxsetup"
	PassphraseFile string // defaults to PassphraseFile when empty
	SSHKeyFile     string // defaults to SSHKeyPath when empty; see sshkey.go
}

func (c Client) passphraseFile() string {
	if c.PassphraseFile != "" {
		return c.PassphraseFile
	}
	return PassphraseFile
}

func (c Client) sshKeyFile() string {
	if c.SSHKeyFile != "" {
		return c.SSHKeyFile
	}
	return SSHKeyPath
}

// runner returns a system.Runner pre-configured with this repository's
// environment, so every method below just calls a borg subcommand without
// repeating the env setup.
func (c Client) runner() system.Runner {
	r := c.Runner
	r.ExtraEnv = append([]string{}, r.ExtraEnv...)
	r.ExtraEnv = append(r.ExtraEnv,
		"BORG_REPO="+c.Repo,
		// borg splits BORG_PASSCOMMAND itself (Python shlex, no shell) and
		// runs the result directly — so `base64 -d <file>`, which relies on
		// GNU coreutils accepting a positional filename, is not portable:
		// confirmed live, BSD base64 (macOS) rejects that argument outright
		// and only reads from stdin. Routing through `sh -c '... < file'`
		// makes the redirection itself a shell feature rather than an
		// assumption about which base64 is installed, which works
		// identically on every target this tool runs on. The path is
		// quoted twice on purpose: once so the shell sees it as one
		// argument to `<`, and the whole `sh -c '...'` invocation quoted
		// again so borg's own argv-splitting keeps the shell script intact
		// as a single token.
		"BORG_PASSCOMMAND="+passCommand(c.passphraseFile()),
		// Backups run unattended; borg's interactive confirmations
		// ("Warning: Attempting to access a previously unknown
		// unencrypted repository") have no terminal to answer them from
		// and would otherwise just hang. Every repository this package
		// creates is encrypted (Init always passes --encryption), so
		// this flag only ever matters for a repository pointed at by
		// hand at a location borg has not seen from this machine before.
		"BORG_RELOCATED_REPO_ACCESS_IS_OK=yes",
		"BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes",
	)
	// A remote repository needs an SSH identity, and this tool never
	// wants to depend on whatever key `ssh` happens to resolve on its own
	// for whoever is running it — confirmed live: that resolution differs
	// between an interactive shell (an agent, a ~/.ssh/config entry an
	// operator set up by hand) and how a backup actually runs in
	// practice (root, non-interactive, cron/systemd, neither of those
	// things present), and the difference surfaces as a confusing
	// "Permission denied (publickey)" the first time an unattended run
	// tries to connect. Pointing BORG_RSH at the key this package itself
	// generates or imports (see sshkey.go) removes that dependency
	// entirely — an explicit identity, not an environmental accident.
	// IdentitiesOnly=yes stops ssh from also offering every other key it
	// can find first, which would otherwise let the *wrong* key work by
	// coincidence in testing and then fail once it is no longer on the
	// machine. AcceptNewHostKeys keeps first-contact non-interactive,
	// the same reasoning BORG_RELOCATED_REPO_ACCESS_IS_OK already applies
	// to borg's own first-contact prompts.
	if keyFile := c.sshKeyFile(); strings.HasPrefix(c.Repo, "ssh://") && fileExists(keyFile) {
		r.ExtraEnv = append(r.ExtraEnv,
			"BORG_RSH=ssh -i "+keyFile+" -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new")
	}
	// borg's own timeout for a slow network repository dwarfs the rest of
	// this tool's commands; give it real room rather than inheriting a
	// timeout sized for a local apt install.
	if r.Timeout == 0 {
		r.Timeout = 2 * time.Hour
	}
	return r
}

// SetPassphrase stores the repository passphrase. Overwrites whatever was
// there before — changing it here does not re-encrypt an already-initialised
// repository's data (borg has its own `key change-passphrase` for that);
// this is for first-time setup or for pointing at a repository whose
// passphrase is already known.
func SetPassphrase(path, plaintext string) error {
	if path == "" {
		path = PassphraseFile
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(plaintext))
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(encoded), 0o600)
}

// Installed reports whether the borg binary is present.
func (c Client) Installed() bool { return c.Runner.Look("borg") }

// Reachable reports whether the repository exists and its passphrase is
// correct — the "is this actually set up and working" check, distinct from
// merely having a repo URL and a passphrase file on disk.
func (c Client) Reachable(ctx context.Context) bool {
	_, err := c.runner().Output(ctx, "borg", "info", "--json")
	return err == nil
}

// Init creates a new, encrypted repository. Safe to call on a repository
// that already exists and is reachable with the current passphrase — it
// checks first rather than letting borg fail with "repository already
// exists," so setup can be re-run after changing unrelated settings without
// an operator needing to know whether this specific step already happened.
func (c Client) Init(ctx context.Context, encryption string) error {
	if c.Reachable(ctx) {
		logx.Skip("borg repository %s already initialised", c.Repo)
		return nil
	}
	if encryption == "" {
		encryption = "repokey-blake2"
	}
	if _, err := c.runner().Output(ctx, "borg", "init", "--encryption="+encryption); err != nil {
		return fmt.Errorf("initialising borg repository %s: %w", c.Repo, err)
	}
	logx.Change("initialised borg repository %s (%s)", c.Repo, encryption)
	return nil
}

// Info returns borg's own human-readable repository summary, for display
// rather than parsing.
func (c Client) Info(ctx context.Context) (string, error) {
	return c.runner().CombinedOutput(ctx, "borg", "info")
}

// CreateArchive backs up paths into a new archive. Each path is backed up
// as its own absolute-path entry, which is what makes ExtractFile able to
// restore a specific one back to its original location later.
func (c Client) CreateArchive(ctx context.Context, name, compression string, paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("no paths given to back up")
	}
	if compression == "" {
		compression = "zstd"
	}
	args := []string{"create", "--compression=" + compression, "--stats", c.Repo + "::" + name}
	args = append(args, paths...)
	out, err := c.runner().CombinedOutput(ctx, "borg", args...)
	if err != nil {
		return fmt.Errorf("creating archive %s: %w\n%s", name, err, out)
	}
	logx.Change("created borg archive %s", name)
	return nil
}

// Archive is one backup point in time.
type Archive struct {
	Name string `json:"name"`
	Time string `json:"time"` // RFC3339-ish, whatever borg reports; displayed as-is
}

type archiveListJSON struct {
	Archives []struct {
		Name  string `json:"name"`
		Start string `json:"start"`
		Time  string `json:"time"`
	} `json:"archives"`
}

// ListArchives returns every archive in the repository, oldest first (borg's
// own listing order).
func (c Client) ListArchives(ctx context.Context) ([]Archive, error) {
	out, err := c.runner().Output(ctx, "borg", "list", "--json")
	if err != nil {
		return nil, fmt.Errorf("listing archives in %s: %w", c.Repo, err)
	}
	var parsed archiveListJSON
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return nil, fmt.Errorf("parsing borg list output: %w", err)
	}
	list := make([]Archive, 0, len(parsed.Archives))
	for _, a := range parsed.Archives {
		when := a.Start
		if when == "" {
			when = a.Time
		}
		list = append(list, Archive{Name: a.Name, Time: when})
	}
	return list, nil
}

// RepoStats is the repository-wide size and deduplication summary borg
// itself tracks in its local cache — the same numbers `borg info`'s
// human-readable "All archives" / "Chunk index" table shows, structured for
// display rather than parsed out of text.
type RepoStats struct {
	ID           string
	Encryption   string
	LastModified string

	// TotalSize/TotalCompressedSize sum every archive's original/compressed
	// size, counting a chunk once for every archive that references it —
	// "how much data this would be without deduplication across archives."
	TotalSize           int64
	TotalCompressedSize int64
	// UniqueSize/UniqueCompressedSize count each chunk exactly once, no
	// matter how many archives share it. UniqueCompressedSize is the
	// repository's real, physical footprint — what it actually occupies on
	// disk or on the remote end right now.
	UniqueSize           int64
	UniqueCompressedSize int64

	TotalChunks  int64
	UniqueChunks int64
}

// DedupRatio reports how much smaller the deduplicated, compressed
// repository is than the sum of every archive's original size — e.g. 0.75
// means the repository occupies 25% of what storing every archive
// separately, uncompressed, would take. 0 before there is any data to
// compute a ratio from, never negative.
func (s RepoStats) DedupRatio() float64 {
	if s.TotalSize <= 0 {
		return 0
	}
	ratio := 1 - float64(s.UniqueCompressedSize)/float64(s.TotalSize)
	if ratio < 0 {
		return 0
	}
	return ratio
}

// ArchiveDetail is one archive's identity plus its own size contribution —
// the counterpart to Archive with borg's per-archive stats block folded in.
type ArchiveDetail struct {
	Name string
	Time string

	OriginalSize   int64
	CompressedSize int64
	// DeduplicatedSize is this archive's own marginal contribution to the
	// repository — the compressed size of chunks that were new when this
	// archive was created, not shared with any earlier archive. A second
	// backup of mostly-unchanged files legitimately reports close to 0 here
	// even though OriginalSize is large — that is deduplication working, not
	// a bug.
	DeduplicatedSize int64
	NFiles           int64
}

type infoStatsJSON struct {
	Archives []struct {
		Name  string `json:"name"`
		Start string `json:"start"`
		Stats struct {
			OriginalSize     int64 `json:"original_size"`
			CompressedSize   int64 `json:"compressed_size"`
			DeduplicatedSize int64 `json:"deduplicated_size"`
			NFiles           int64 `json:"nfiles"`
		} `json:"stats"`
	} `json:"archives"`
	Cache struct {
		Stats struct {
			TotalSize         int64 `json:"total_size"`
			TotalCSize        int64 `json:"total_csize"`
			UniqueSize        int64 `json:"unique_size"`
			UniqueCSize       int64 `json:"unique_csize"`
			TotalChunks       int64 `json:"total_chunks"`
			TotalUniqueChunks int64 `json:"total_unique_chunks"`
		} `json:"stats"`
	} `json:"cache"`
	Repository struct {
		ID           string `json:"id"`
		LastModified string `json:"last_modified"`
	} `json:"repository"`
	Encryption struct {
		Mode string `json:"mode"`
	} `json:"encryption"`
}

// Stats returns the repository's deduplication summary together with every
// archive's own size contribution, from a single `borg info -a '*' --json`
// call rather than one round trip per archive: a glob matching every
// archive name returns every matched archive's stats block alongside the
// repository-wide cache stats in one response — confirmed live against both
// borg 1.2.8 (Ubuntu 24.04's packaged version) and 1.4.5. An empty
// repository (freshly initialised, no archives yet) is not an error: borg
// returns zeroed stats and an empty archive list, and so does this.
func (c Client) Stats(ctx context.Context) (RepoStats, []ArchiveDetail, error) {
	out, err := c.runner().Output(ctx, "borg", "info", "-a", "*", "--json")
	if err != nil {
		return RepoStats{}, nil, fmt.Errorf("reading repository stats for %s: %w", c.Repo, err)
	}
	var parsed infoStatsJSON
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return RepoStats{}, nil, fmt.Errorf("parsing borg info output: %w", err)
	}

	repo := RepoStats{
		ID:                   parsed.Repository.ID,
		Encryption:           parsed.Encryption.Mode,
		LastModified:         parsed.Repository.LastModified,
		TotalSize:            parsed.Cache.Stats.TotalSize,
		TotalCompressedSize:  parsed.Cache.Stats.TotalCSize,
		UniqueSize:           parsed.Cache.Stats.UniqueSize,
		UniqueCompressedSize: parsed.Cache.Stats.UniqueCSize,
		TotalChunks:          parsed.Cache.Stats.TotalChunks,
		UniqueChunks:         parsed.Cache.Stats.TotalUniqueChunks,
	}

	archives := make([]ArchiveDetail, 0, len(parsed.Archives))
	for _, a := range parsed.Archives {
		archives = append(archives, ArchiveDetail{
			Name:             a.Name,
			Time:             a.Start,
			OriginalSize:     a.Stats.OriginalSize,
			CompressedSize:   a.Stats.CompressedSize,
			DeduplicatedSize: a.Stats.DeduplicatedSize,
			NFiles:           a.Stats.NFiles,
		})
	}
	return repo, archives, nil
}

// ExtractPath restores one member of an archive. Because archives are
// created from absolute paths, extracting with dir set to "/" recreates the
// member at its original location — the standard borg in-place restore
// pattern. Pass a different dir to extract somewhere else instead (a
// scratch directory, to pull out a database dump without touching the
// site's live files).
func (c Client) ExtractPath(ctx context.Context, archiveName, memberPath, dir string) error {
	r := c.runner()
	r.Dir = dir
	// borg's members are stored without a leading slash; memberPath is
	// given here as an absolute path for callers' clarity, so strip it.
	member := strings.TrimPrefix(memberPath, "/")
	out, err := r.CombinedOutput(ctx, "borg", "extract", c.Repo+"::"+archiveName, member)
	if err != nil {
		return fmt.Errorf("extracting %s from %s: %w\n%s", memberPath, archiveName, err, out)
	}
	return nil
}

// DeleteArchive removes a single archive from the repository, leaving every
// other archive and the repository itself untouched — the one-off,
// operator-chosen counterpart to Prune's retention-policy-driven bulk
// removal. Irreversible: once deleted, an archive's data is gone the next
// time the repository is compacted, the same as `borg delete` run by hand.
func (c Client) DeleteArchive(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("no archive name given")
	}
	out, err := c.runner().CombinedOutput(ctx, "borg", "delete", "--stats", c.Repo+"::"+name)
	if err != nil {
		return fmt.Errorf("deleting archive %s: %w\n%s", name, err, out)
	}
	logx.Change("deleted borg archive %s", name)
	return nil
}

// Prune removes old archives according to a retention policy, keeping the
// most recent of each. A zero value for any Keep field omits that rule
// entirely (borg's own convention: an absent --keep-* flag imposes no limit
// of that kind, rather than meaning zero).
type Retention struct {
	KeepDaily   int
	KeepWeekly  int
	KeepMonthly int
}

func (r Retention) empty() bool { return r.KeepDaily == 0 && r.KeepWeekly == 0 && r.KeepMonthly == 0 }

func (c Client) Prune(ctx context.Context, ret Retention) error {
	if ret.empty() {
		return nil // nothing configured to prune by; leave every archive alone
	}
	args := []string{"prune", "--stats"}
	if ret.KeepDaily > 0 {
		args = append(args, fmt.Sprintf("--keep-daily=%d", ret.KeepDaily))
	}
	if ret.KeepWeekly > 0 {
		args = append(args, fmt.Sprintf("--keep-weekly=%d", ret.KeepWeekly))
	}
	if ret.KeepMonthly > 0 {
		args = append(args, fmt.Sprintf("--keep-monthly=%d", ret.KeepMonthly))
	}
	args = append(args, c.Repo)
	out, err := c.runner().CombinedOutput(ctx, "borg", args...)
	if err != nil {
		return fmt.Errorf("pruning %s: %w\n%s", c.Repo, err, out)
	}
	logx.Change("pruned old archives in %s", c.Repo)
	return nil
}

// passCommand builds the BORG_PASSCOMMAND value for reading path's
// base64-encoded content back out as the repository passphrase. See the
// comment at its call site for why this goes through `sh -c` with
// redirection rather than `base64 -d path` directly.
func passCommand(path string) string {
	inner := "base64 -d < " + shellQuote(path)
	return "sh -c " + shellQuote(inner)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func dirOf(path string) string {
	i := strings.LastIndexByte(path, '/')
	if i <= 0 {
		return "/"
	}
	return path[:i]
}

// shellQuote wraps a value in single quotes for safe interpolation into the
// BORG_PASSCOMMAND string, which borg itself passes to a shell — the one
// place in this package a value legitimately needs shell quoting rather
// than being kept out of a command line entirely, since BORG_PASSCOMMAND's
// entire contract *is* "a shell command borg runs for you."
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
