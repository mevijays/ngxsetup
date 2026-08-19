# Backups

Two layers, independent of each other: local database dumps for a
quick, before-you-touch-anything safety net, and off-box, deduplicated,
encrypted backups (files *and* database) via
[Borg](https://borgbackup.org) for real disaster recovery.

## Local database backups

```bash
sudo ngxsetup db backup example.com                    # one site
sudo ngxsetup db backup                                 # every site, one click
sudo ngxsetup db backup --out /mnt/backups/mysql         # choose the directory
```

A logical dump (`mysqldump`/`mariadb-dump --single-transaction`, a
consistent snapshot without locking writers out) to a timestamped
`.sql` file per database, root-only (`0700`/`0600`) under
`/var/backups/ngxsetup/db` by default. Backing up every site
independently means one database failing to dump does not stop the
others.

```bash
sudo ngxsetup db restore example.com /var/backups/ngxsetup/db/example-com-20260101-030000.sql
```

Loads a `.sql` file into a site's database, overwriting its current
contents. Destructive, so it asks for confirmation and — unless
`--no-safety-backup` is given — backs up the database's current
contents first, so restoring the wrong file is a mistake one more
command away from fixed rather than permanent. The web UI's Backups
page lists these dumps with a download link for each.

## Off-box backups (Borg)

```bash
sudo ngxsetup borg setup --repo /mnt/backup/ngxsetup            # local disk
sudo ngxsetup borg setup --repo ssh://user@host:2222/./ngxsetup  # remote, over SSH
sudo ngxsetup borg backup                                        # every site, files + database, one archive each
sudo ngxsetup borg backup example.com
sudo ngxsetup borg list
sudo ngxsetup borg restore example.com <archive> --database --files
sudo ngxsetup borg schedule daily                                 # one-click "cron" — see below
```

A deduplicated, encrypted, incremental backup of each site's files
*and* database together, driven the same way `mysqldump` already is:
nothing sensitive on a command line, everything through environment
variables the way Borg itself recommends for unattended use.

`borg setup` installs the `borgbackup` package if it isn't already
present, initialises an encrypted repository, and stores the passphrase
root-only at `/etc/ngxsetup/borg-passphrase` — leave the passphrase
prompt blank to generate a strong one, shown exactly once.

`borg backup` puts a site's whole directory tree and a fresh database
dump into a single archive, so a restore is one point in time rather
than reconciling separately-timed files and data. `borg restore` can
restore the database, the files, or both, and (for the database half)
takes the same safety-backup-first precaution `db restore` does.

### A remote repository needs its own SSH key

For a `ssh://` repository, ngxsetup manages a dedicated SSH keypair for
reaching it — never an operator's own interactive-shell identity, which
"works by accident" locally and fails the moment a backup runs
unattended (cron, systemd, no agent). Leave `--repo`'s key prompt blank
to generate one automatically; the public half is printed after setup
so you can register it on the repository server. See
[Pairing with ngxborg](ngxborg-integration.md) for the full walkthrough
against ngxsetup's own companion backup server.

### Scheduling

```bash
sudo ngxsetup borg schedule daily     # also: hourly, weekly, or a raw systemd OnCalendar expression
sudo ngxsetup borg schedule --disable
```

A systemd timer that calls back into `ngxsetup borg backup --prune`,
not a crontab entry — the same sandboxing and hard-timeout reasons the
WordPress scheduler already runs as a timer. Its runs show up in
`journalctl -u ngxsetup-borg.service`, which the web UI's Log Viewer
page can also show.

### Retention

```bash
sudo ngxsetup config set borg.keep_daily 7
sudo ngxsetup config set borg.keep_weekly 4
sudo ngxsetup config set borg.keep_monthly 6
```

`0` means "keep everything of that granularity." Applied with
`borg backup --prune` or automatically on every scheduled run.
