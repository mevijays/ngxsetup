package provision

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ngxsetup/internal/borg"
	"ngxsetup/internal/logx"
	"ngxsetup/internal/state"
	"ngxsetup/internal/system"
	"ngxsetup/internal/tmpl"
)

const (
	borgServiceUnit = "/etc/systemd/system/ngxsetup-borg.service"
	borgTimerUnit   = "/etc/systemd/system/ngxsetup-borg.timer"
)

// BorgStagingDir holds a database dump just long enough for a borg archive
// to pick it up alongside the site's files, then is cleaned up immediately
// after — never a permanent copy the way DefaultBackupDir's dumps are.
const BorgStagingDir = "/var/backups/ngxsetup/borg-staging"

func (c *Ctx) borgClient() borg.Client {
	return borg.Client{Runner: c.Runner, Repo: c.Config.Borg.Repo}
}

// ensureBorgInstalled installs the borgbackup package if it is not already
// present, the same on-demand pattern setup.go uses for Redis: nothing
// about the base stack needs it, so it is only ever installed once an
// operator actually asks for remote backups.
func (c *Ctx) ensureBorgInstalled() error {
	return system.AptInstall(c.Context, c.Runner, "borgbackup")
}

// BorgSetupResult reports what SetupBorgRepo actually did, including a
// generated passphrase when the caller did not supply one — shown to the
// operator exactly once, the same pattern this tool uses for every other
// generated secret.
type BorgSetupResult struct {
	GeneratedPassphrase string // "" unless one was generated
	// PublicKey is the dedicated borg SSH key's public half — always
	// populated when the repository is remote (ssh://), whether the key
	// was just generated, just imported, or already existed from an
	// earlier setup. Shown to the operator so they know what to register
	// on the repository server; not a secret, so unlike the passphrase
	// there is no reason to only show it once.
	PublicKey string
	// GeneratedSSHKey is true only when this call generated a brand new
	// key — the moment it is most worth an operator's attention, since an
	// existing repository's remote end has never seen this key before
	// and needs it registered before the first connection can succeed.
	GeneratedSSHKey bool
}

// SetupBorgRepo configures and initialises a borg repository: installs
// borg if needed, stores the passphrase, ensures a dedicated SSH identity
// exists for a remote repository, persists the repo/encryption/
// compression settings, and runs `borg init` if the repository is not
// already reachable with this passphrase.
//
// An empty passphrase generates a strong random one rather than leaving the
// repository unprotected or failing outright — consistent with how this
// tool has handled every other secret (database passwords, the old web UI
// login) since the beginning. sshPrivateKey works the same way for the
// SSH identity a remote repository needs: empty generates a fresh
// dedicated key (or reuses one from an earlier setup); a non-empty value
// imports that key instead, for an operator who already has one they want
// this repository to use — see borg.EnsureSSHKey for exactly what "reuse"
// means and why replacing a key already in use has to be an explicit
// choice, never an accidental side effect of re-running setup.
func (c *Ctx) SetupBorgRepo(repo, encryption, compression, passphrase, sshPrivateKey string) (*BorgSetupResult, error) {
	if repo == "" {
		return nil, fmt.Errorf("a repository location is required (e.g. /mnt/backup/ngxsetup or ssh://user@host/./ngxsetup)")
	}
	if err := c.ensureBorgInstalled(); err != nil {
		return nil, err
	}

	result := &BorgSetupResult{}
	if passphrase == "" {
		generated, err := system.Password(32)
		if err != nil {
			return nil, err
		}
		passphrase = generated
		result.GeneratedPassphrase = generated
	}
	if err := borg.SetPassphrase(borg.PassphraseFile, passphrase); err != nil {
		return nil, fmt.Errorf("storing the repository passphrase: %w", err)
	}

	if strings.HasPrefix(repo, "ssh://") {
		pub, generated, err := borg.EnsureSSHKey(c.Context, c.Runner, sshPrivateKey)
		if err != nil {
			return nil, fmt.Errorf("preparing an SSH identity for the remote repository: %w", err)
		}
		result.PublicKey = pub
		result.GeneratedSSHKey = generated
	}

	c.Config.Borg.Repo = repo
	c.Config.Borg.Encryption = encryption
	c.Config.Borg.Compression = compression
	c.Config.Borg.Enabled = true
	if err := c.Config.Save(); err != nil {
		return nil, err
	}

	if err := c.borgClient().Init(c.Context, encryption); err != nil {
		// Deliberately return result alongside the error rather than nil:
		// on a brand new remote repository this Init is often the very
		// call that first proves a freshly generated key isn't
		// registered on the other end yet ("Permission denied
		// (publickey)") — the most common way this whole function fails.
		// Discarding result here would discard the one piece of
		// information (the new public key) the operator needs to fix
		// that and retry. Every caller must treat a non-nil result on a
		// non-nil error as "partially set up, see the error" rather than
		// "succeeded."
		return result, err
	}
	return result, nil
}

// BorgSSHPublicKey returns the dedicated borg SSH key's public half, for
// re-displaying it later (the status page, `borg status`) without
// generating or importing anything — "" if no key has been set up yet.
func (c *Ctx) BorgSSHPublicKey() (string, error) {
	return borg.PublicKey()
}

// BorgStatus reports whether a repository is configured and currently
// reachable, for a dashboard/status display.
type BorgStatus struct {
	Configured bool
	Repo       string
	Installed  bool
	Reachable  bool
	Info       string
	Schedule   string

	// Stats and StatsOK are the repository's deduplication summary — total
	// vs. unique/compressed size, the real "how much space does this
	// actually occupy" answer. StatsOK is false (rather than Stats being a
	// zero value indistinguishable from "an empty repository") whenever it
	// could not be gathered at all — borg installed but the repository not
	// currently reachable, most commonly — so a caller can tell "no data
	// yet" apart from "asked, got nothing."
	Stats   borg.RepoStats
	StatsOK bool
}

func (c *Ctx) BorgStatus() BorgStatus {
	st := BorgStatus{
		Configured: c.Config.Borg.Repo != "",
		Repo:       c.Config.Borg.Repo,
		Schedule:   c.Config.Borg.Schedule,
	}
	client := c.borgClient()
	st.Installed = client.Installed()
	if st.Configured && st.Installed {
		st.Reachable = client.Reachable(c.Context)
		if st.Reachable {
			st.Info, _ = client.Info(c.Context)
			if stats, _, err := client.Stats(c.Context); err == nil {
				st.Stats = stats
				st.StatsOK = true
			}
		}
	}
	return st
}

// BorgBackupResult records the outcome of archiving one site.
type BorgBackupResult struct {
	Domain  string
	Archive string
	Err     error
}

// BorgBackupSite archives one site's files and, if it has one, a fresh
// database dump — one borg archive covering both, so restoring means
// picking a single point in time rather than reconciling a files archive
// against a separately-timed database dump.
func (c *Ctx) BorgBackupSite(nameOrSlug string) (*BorgBackupResult, error) {
	site, err := c.State.Find(nameOrSlug)
	if err != nil {
		return nil, err
	}
	if c.Config.Borg.Repo == "" {
		return nil, fmt.Errorf("no borg repository configured; run `ngxsetup borg setup` first")
	}
	return c.borgBackupOne(*site)
}

// BorgBackupAll archives every registered site. A failure on one site is
// recorded on its own result rather than aborting the batch, the same
// resilience BackupAllDatabases already has.
func (c *Ctx) BorgBackupAll() ([]BorgBackupResult, error) {
	if c.Config.Borg.Repo == "" {
		return nil, fmt.Errorf("no borg repository configured; run `ngxsetup borg setup` first")
	}
	var results []BorgBackupResult
	for _, site := range c.State.Sites {
		result, err := c.borgBackupOne(site)
		if err != nil {
			results = append(results, BorgBackupResult{Domain: site.Domain, Err: err})
			continue
		}
		results = append(results, *result)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("no sites are registered")
	}
	return results, nil
}

func (c *Ctx) borgBackupOne(site state.Site) (*BorgBackupResult, error) {
	paths := []string{c.Path(c.SiteRoot(site.Slug))}

	var dumpPath string
	if site.DBName != "" {
		if err := os.MkdirAll(c.Path(BorgStagingDir), 0o700); err != nil {
			return nil, err
		}
		dumpPath = c.Path(filepath.Join(BorgStagingDir, site.Slug+".sql"))
		logx.Step("dumping %s for borg", site.DBName)
		if err := c.DBClient().Dump(c.Context, site.DBName, dumpPath); err != nil {
			return nil, err
		}
		defer os.Remove(dumpPath)
		paths = append(paths, dumpPath)
	}

	archiveName := fmt.Sprintf("%s-%s", site.Slug, time.Now().UTC().Format("20060102-150405"))
	logx.Step("archiving %s to borg", site.Domain)
	if err := c.borgClient().CreateArchive(c.Context, archiveName, c.Config.Borg.Compression, paths); err != nil {
		return nil, err
	}
	logx.Change("backed up %s to borg archive %s", site.Domain, archiveName)
	return &BorgBackupResult{Domain: site.Domain, Archive: archiveName}, nil
}

// BorgListArchives lists every archive in the configured repository. Fast —
// `borg list`, no per-archive size accounting — for callers that just need
// names and times; see BorgArchiveDetails for the size/dedup breakdown.
func (c *Ctx) BorgListArchives() ([]borg.Archive, error) {
	if c.Config.Borg.Repo == "" {
		return nil, fmt.Errorf("no borg repository configured; run `ngxsetup borg setup` first")
	}
	return c.borgClient().ListArchives(c.Context)
}

// BorgArchiveDetails lists every archive together with its own size and
// deduplication contribution, alongside the repository-wide summary — one
// round trip to the repository (see Client.Stats), not one per archive.
func (c *Ctx) BorgArchiveDetails() (borg.RepoStats, []borg.ArchiveDetail, error) {
	if c.Config.Borg.Repo == "" {
		return borg.RepoStats{}, nil, fmt.Errorf("no borg repository configured; run `ngxsetup borg setup` first")
	}
	return c.borgClient().Stats(c.Context)
}

// BorgDeleteArchive removes a single archive from the repository — the
// operator-chosen counterpart to BorgPrune's retention-policy-driven bulk
// removal. Irreversible, and left to the caller (CLI confirmation prompt,
// web UI confirm modal) to guard, the same as BorgRestoreFiles.
func (c *Ctx) BorgDeleteArchive(archiveName string) error {
	if c.Config.Borg.Repo == "" {
		return fmt.Errorf("no borg repository configured; run `ngxsetup borg setup` first")
	}
	if archiveName == "" {
		return fmt.Errorf("an archive name is required")
	}
	return c.borgClient().DeleteArchive(c.Context, archiveName)
}

// BorgRestoreDatabase extracts an archive's database dump into a scratch
// directory and restores it through the same path `db restore` uses —
// including its safety-backup-first behaviour — rather than duplicating
// that logic here.
func (c *Ctx) BorgRestoreDatabase(archiveName, nameOrSlug string, skipSafetyBackup bool) (*RestoreResult, error) {
	site, err := c.State.Find(nameOrSlug)
	if err != nil {
		return nil, err
	}
	if site.DBName == "" {
		return nil, fmt.Errorf("%s has no database", site.Domain)
	}
	scratch, err := os.MkdirTemp("", "ngxsetup-borg-restore-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)

	dumpPath := c.Path(filepath.Join(BorgStagingDir, site.Slug+".sql"))
	if err := c.borgClient().ExtractPath(c.Context, archiveName, dumpPath, scratch); err != nil {
		return nil, err
	}
	extracted := filepath.Join(scratch, dumpPath)
	return c.RestoreDatabase(nameOrSlug, extracted, skipSafetyBackup)
}

// BorgRestoreFiles restores an archive's copy of a site's file tree back in
// place, overwriting whatever is there now. There is no separate safety
// backup for this the way database restore has one — a files tree can be
// arbitrarily large, and borg itself already holds every prior version of
// it; restoring the current state as its own archive first is a deliberate
// choice left to the caller (the CLI and web UI both offer "back up first"
// as a distinct, explicit step) rather than an automatic, possibly very
// slow side effect of every restore.
func (c *Ctx) BorgRestoreFiles(archiveName, nameOrSlug string) error {
	site, err := c.State.Find(nameOrSlug)
	if err != nil {
		return err
	}
	root := c.Path(c.SiteRoot(site.Slug))
	logx.Step("restoring %s's files from borg archive %s", site.Domain, archiveName)
	if err := c.borgClient().ExtractPath(c.Context, archiveName, root, "/"); err != nil {
		return err
	}
	logx.Change("restored %s's files from %s", site.Domain, archiveName)
	return nil
}

// BorgPrune removes archives older than the configured retention policy.
func (c *Ctx) BorgPrune() error {
	if c.Config.Borg.Repo == "" {
		return nil
	}
	ret := borg.Retention{
		KeepDaily:   c.Config.Borg.KeepDaily,
		KeepWeekly:  c.Config.Borg.KeepWeekly,
		KeepMonthly: c.Config.Borg.KeepMonthly,
	}
	return c.borgClient().Prune(c.Context, ret)
}

// BorgInstallSchedule writes and enables a systemd timer that runs
// `ngxsetup borg backup` on the given schedule — the "one click" cron
// this tool offers, implemented as a timer rather than a crontab entry for
// the same reasons wp-cron.timer already is: sandboxing (ProtectSystem,
// no arbitrary shell), a hard timeout so a stuck run cannot pile up, and a
// journal-backed log the web UI's log viewer can show without this package
// needing to know how to parse a cron log file.
//
// onCalendar is a systemd OnCalendar expression (e.g. "daily", "03:00",
// "Sun 03:00", or a full "*-*-* 03:00:00" form) — validated by systemd
// itself when the timer is started, not re-implemented here.
func (c *Ctx) BorgInstallSchedule(onCalendar string, prune bool) error {
	if c.Config.Borg.Repo == "" {
		return fmt.Errorf("no borg repository configured; run `ngxsetup borg setup` first")
	}

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving this binary's own path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(execPath); err == nil {
		execPath = resolved
	}

	serviceBody, err := tmpl.Render("system/borg-backup.service.tmpl", tmpl.BorgBackup{
		DBUnit: c.DBUnit, Prune: prune, ExecPath: execPath,
	})
	if err != nil {
		return err
	}
	if _, err := c.Writer.Write(borgServiceUnit, serviceBody, 0o644, false); err != nil {
		return err
	}

	timerBody, err := tmpl.Render("system/borg-backup.timer.tmpl", tmpl.BorgBackup{OnCalendar: onCalendar})
	if err != nil {
		return err
	}
	if _, err := c.Writer.Write(borgTimerUnit, timerBody, 0o644, false); err != nil {
		return err
	}

	c.Config.Borg.Schedule = onCalendar
	if err := c.Config.Save(); err != nil {
		return err
	}

	if c.Writer.DryRun {
		return nil
	}
	if err := system.DaemonReload(c.Context, c.Runner); err != nil {
		return err
	}
	return system.EnableNow(c.Context, c.Runner, "ngxsetup-borg.timer")
}

// BorgRemoveSchedule disables and removes the scheduled backup timer. The
// repository and its archives are untouched — this only stops future
// automatic runs.
func (c *Ctx) BorgRemoveSchedule() error {
	if !c.Writer.DryRun {
		c.Runner.TryRun(c.Context, "systemctl", "disable", "--now", "ngxsetup-borg.timer")
	}
	if err := c.Writer.Remove(borgTimerUnit); err != nil {
		return err
	}
	if err := c.Writer.Remove(borgServiceUnit); err != nil {
		return err
	}
	c.Config.Borg.Schedule = ""
	if err := c.Config.Save(); err != nil {
		return err
	}
	if c.Writer.DryRun {
		return nil
	}
	return system.DaemonReload(c.Context, c.Runner)
}
