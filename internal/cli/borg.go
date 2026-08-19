package cli

import (
	"context"
	"fmt"
	"os"

	"ngxsetup/internal/borg"
	"ngxsetup/internal/logx"
	"ngxsetup/internal/provision"
)

func cmdBorg(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ngxsetup borg <setup|status|backup|list|restore|delete|schedule> ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "setup":
		return cmdBorgSetup(ctx, rest)
	case "status":
		return cmdBorgStatus(ctx, rest)
	case "backup":
		return cmdBorgBackup(ctx, rest)
	case "list":
		return cmdBorgList(ctx, rest)
	case "restore":
		return cmdBorgRestore(ctx, rest)
	case "delete":
		return cmdBorgDelete(ctx, rest)
	case "schedule":
		return cmdBorgSchedule(ctx, rest)
	default:
		return fmt.Errorf("unknown borg subcommand %q", sub)
	}
}

func cmdBorgSetup(ctx context.Context, args []string) error {
	fs := newFlagSet("borg setup")
	repo := fs.String("repo", "", "repository location, e.g. /mnt/backup/ngxsetup or ssh://user@host:2222/./ngxsetup")
	encryption := fs.String("encryption", "repokey-blake2", "borg encryption mode")
	compression := fs.String("compression", "zstd", "borg compression algorithm")
	generate := fs.Bool("generate", false, "generate a random passphrase instead of prompting for one")
	sshKeyFile := fs.String("ssh-key-file", "", "import an existing private key for reaching a remote repository, instead of generating a new one")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	if *repo == "" {
		return fmt.Errorf("--repo is required")
	}

	var passphrase string
	if !*generate {
		p, err := readSecret("Repository passphrase (leave blank to generate one): ")
		if err != nil && err.Error() != "no password entered" {
			return err
		}
		passphrase = p
	}

	var sshPrivateKey string
	if *sshKeyFile != "" {
		body, err := os.ReadFile(*sshKeyFile)
		if err != nil {
			return fmt.Errorf("reading %s: %w", *sshKeyFile, err)
		}
		sshPrivateKey = string(body)
	}

	c, err := provision.New(ctx, provision.Options{})
	if err != nil {
		return err
	}
	result, err := c.SetupBorgRepo(*repo, *encryption, *compression, passphrase, sshPrivateKey)
	// result can be non-nil even when err is not: SetupBorgRepo still
	// reports a freshly generated SSH key even if the `borg init` that
	// follows failed to reach the repository with it — the single most
	// common way this fails on a first-ever setup against a remote that
	// does not know the new key yet. Show that key regardless of err, so
	// the operator has what they need to register it and retry rather
	// than a bare "Permission denied" with no next step.
	if err != nil {
		if result != nil && result.PublicKey != "" {
			logx.Info("")
			logx.Info("  ssh public key: %s", result.PublicKey)
			if result.GeneratedSSHKey {
				logx.Warn("this is a newly generated key — the remote server does not know it yet. Register it there, then re-run this command.")
				if hint := borg.SuggestRemoteKeyCommand(*repo, result.PublicKey); hint != "" {
					logx.Info("  if the remote end runs ngxborg:  %s", hint)
				}
			}
		}
		return err
	}
	logx.Section("Borg repository ready")
	logx.KV([][2]string{{"repository", *repo}, {"encryption", *encryption}, {"compression", *compression}})
	if result.GeneratedPassphrase != "" {
		logx.Info("")
		logx.Info("  passphrase: %s", result.GeneratedPassphrase)
		logx.Warn("this passphrase is shown once — write it down now. Without it, this repository's backups cannot be restored.")
	}
	if result.PublicKey != "" {
		logx.Info("")
		logx.Info("  ssh public key: %s", result.PublicKey)
		if result.GeneratedSSHKey {
			logx.Warn("this is a newly generated key — the remote server does not know it yet. Register it there before running a backup.")
			if hint := borg.SuggestRemoteKeyCommand(*repo, result.PublicKey); hint != "" {
				logx.Info("  if the remote end runs ngxborg:  %s", hint)
			}
		}
	}
	return nil
}

func cmdBorgStatus(ctx context.Context, args []string) error {
	fs := newFlagSet("borg status")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	c, err := provision.New(ctx, provision.Options{})
	if err != nil {
		return err
	}
	st := c.BorgStatus()
	logx.Section("Borg backup")
	rows := [][2]string{
		{"configured", boolLabel(st.Configured, "yes", "no")},
		{"borg installed", boolLabel(st.Installed, "yes", "no")},
	}
	if st.Configured {
		rows = append(rows, [2]string{"repository", st.Repo})
		rows = append(rows, [2]string{"reachable", boolLabel(st.Reachable, "yes", "no")})
		rows = append(rows, [2]string{"schedule", orDash(st.Schedule)})
	}
	logx.KV(rows)
	if st.StatsOK {
		logx.Section("Repository usage")
		logx.KV([][2]string{
			{"occupied on disk", humanBytes(st.Stats.UniqueCompressedSize)},
			{"before deduplication", humanBytes(st.Stats.TotalSize)},
			{"deduplication savings", fmt.Sprintf("%.0f%%", st.Stats.DedupRatio()*100)},
			{"chunks", fmt.Sprintf("%d unique of %d total", st.Stats.UniqueChunks, st.Stats.TotalChunks)},
		})
	}
	if st.Info != "" {
		logx.Section("Repository info")
		logx.Raw(st.Info)
	}
	return nil
}

// humanBytes renders a byte count the way an operator reads one — KB/MB/GB
// with one decimal place, not a raw integer they have to divide in their
// head. Binary (1024-based) units, matching what borg's own CLI output and
// the rest of this tool's disk/memory figures already use.
func humanBytes(n int64) string {
	if n < 0 {
		return "0 B"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), units[exp])
}

func cmdBorgBackup(ctx context.Context, args []string) error {
	fs := newFlagSet("borg backup")
	prune := fs.Bool("prune", false, "apply the configured retention policy after backing up")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	c, err := provision.New(ctx, provision.Options{})
	if err != nil {
		return err
	}

	domain := arg(pos, 0)
	if domain != "" {
		result, err := c.BorgBackupSite(domain)
		if err != nil {
			return err
		}
		logx.Section("Backup complete")
		logx.KV([][2]string{{"archive", result.Archive}})
	} else {
		results, err := c.BorgBackupAll()
		if err != nil {
			return err
		}
		logx.Section("Summary")
		failed := 0
		for _, r := range results {
			if r.Err != nil {
				logx.Error("%s: %v", r.Domain, r.Err)
				failed++
				continue
			}
			logx.Info("%s -> %s", r.Domain, r.Archive)
		}
		logx.Info("%d site(s) backed up, %d failed", len(results)-failed, failed)
		if failed > 0 {
			return fmt.Errorf("%d borg backup(s) failed — see above", failed)
		}
	}

	if *prune {
		if err := c.BorgPrune(); err != nil {
			return fmt.Errorf("backup succeeded, but pruning failed: %w", err)
		}
	}
	return nil
}

func cmdBorgList(ctx context.Context, args []string) error {
	fs := newFlagSet("borg list")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	c, err := provision.New(ctx, provision.Options{})
	if err != nil {
		return err
	}
	repoStats, archives, err := c.BorgArchiveDetails()
	if err != nil {
		return err
	}
	if len(archives) == 0 {
		logx.Info("No archives yet.")
		return nil
	}
	fmt.Printf("%-32s  %-26s  %10s  %10s  %10s  %6s\n", "ARCHIVE", "TIME", "ORIGINAL", "COMPRESSED", "NEW DATA", "FILES")
	for _, a := range archives {
		fmt.Printf("%-32s  %-26s  %10s  %10s  %10s  %6d\n",
			a.Name, a.Time, humanBytes(a.OriginalSize), humanBytes(a.CompressedSize), humanBytes(a.DeduplicatedSize), a.NFiles)
	}
	fmt.Println()
	fmt.Printf("repository occupies %s on disk (%.0f%% smaller than %s before deduplication)\n",
		humanBytes(repoStats.UniqueCompressedSize), repoStats.DedupRatio()*100, humanBytes(repoStats.TotalSize))
	return nil
}

func cmdBorgRestore(ctx context.Context, args []string) error {
	fs := newFlagSet("borg restore")
	database := fs.Bool("database", false, "restore the database from this archive")
	files := fs.Bool("files", false, "restore the site's files from this archive (overwrites in place)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	noSafetyBackup := fs.Bool("no-safety-backup", false, "skip backing up the database's current contents before overwriting it")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	domain := arg(pos, 0)
	archive := arg(pos, 1)
	if domain == "" || archive == "" {
		return fmt.Errorf("usage: ngxsetup borg restore <domain> <archive> [--database] [--files]")
	}
	if !*database && !*files {
		return fmt.Errorf("specify --database, --files, or both")
	}

	c, err := provision.New(ctx, provision.Options{})
	if err != nil {
		return err
	}
	site, err := c.State.Find(domain)
	if err != nil {
		return err
	}

	if !*yes {
		logx.Warn("this replaces %s's %s with the contents of borg archive %s", site.Domain,
			describeRestoreScope(*database, *files), archive)
		if !confirm(fmt.Sprintf("Restore %s from %s?", site.Domain, archive)) {
			logx.Info("cancelled")
			return nil
		}
	}

	if *database {
		if _, err := c.BorgRestoreDatabase(archive, domain, *noSafetyBackup); err != nil {
			return err
		}
		logx.Change("database restored")
	}
	if *files {
		if err := c.BorgRestoreFiles(archive, domain); err != nil {
			return err
		}
		logx.Change("files restored")
	}
	return nil
}

func cmdBorgDelete(ctx context.Context, args []string) error {
	fs := newFlagSet("borg delete")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	archive := arg(pos, 0)
	if archive == "" {
		return fmt.Errorf("usage: ngxsetup borg delete <archive>")
	}

	c, err := provision.New(ctx, provision.Options{})
	if err != nil {
		return err
	}

	if !*yes {
		logx.Warn("this permanently removes borg archive %s; every other archive in the repository is untouched", archive)
		if !confirm(fmt.Sprintf("Delete archive %s?", archive)) {
			logx.Info("cancelled")
			return nil
		}
	}

	// BorgDeleteArchive -> borg.Client.DeleteArchive already logs the
	// change (see internal/borg/borg.go) — no need to say it twice.
	return c.BorgDeleteArchive(archive)
}

func describeRestoreScope(database, files bool) string {
	switch {
	case database && files:
		return "database and files"
	case database:
		return "database"
	default:
		return "files"
	}
}

func cmdBorgSchedule(ctx context.Context, args []string) error {
	fs := newFlagSet("borg schedule")
	disable := fs.Bool("disable", false, "remove the scheduled backup")
	prune := fs.Bool("prune", true, "also apply the retention policy on each scheduled run")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	c, err := provision.New(ctx, provision.Options{})
	if err != nil {
		return err
	}

	if *disable {
		if err := c.BorgRemoveSchedule(); err != nil {
			return err
		}
		logx.Change("scheduled borg backup removed")
		return nil
	}

	preset := arg(pos, 0)
	if preset == "" {
		return fmt.Errorf("usage: ngxsetup borg schedule <hourly|daily|weekly|OnCalendar-expression> | --disable")
	}
	onCalendar, err := resolveSchedulePreset(preset)
	if err != nil {
		return err
	}
	if err := c.BorgInstallSchedule(onCalendar, *prune); err != nil {
		return err
	}
	logx.Change("scheduled borg backup installed (%s)", onCalendar)
	logx.Info("view its runs with: journalctl -u ngxsetup-borg.service")
	return nil
}

func resolveSchedulePreset(preset string) (string, error) {
	switch preset {
	case "hourly":
		return "hourly", nil
	case "daily":
		return "03:00", nil
	case "weekly":
		return "Sun 03:00", nil
	default:
		return preset, nil // anything else is taken as a raw systemd OnCalendar expression
	}
}
