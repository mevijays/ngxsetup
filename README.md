# ngxsetup

A single binary that turns a bare Ubuntu or Debian server into a tuned, hardened
WordPress host — nginx, PHP-FPM and MariaDB or MySQL — sized for the machine it
is running on.

```bash
sudo ./ngxsetup setup
sudo ngxsetup site add example.com --wordpress --tls --admin-email you@example.com
```

No agent, no runtime dependencies, no configuration management server. Every
template it needs is embedded in the binary.

## What it does differently

**It sizes the stack from a memory budget, not from a table of recommended
values.** Total RAM is split explicitly between the database, the PHP worker
pool, and memory deliberately left free for the kernel page cache. Every other
number — `max_children`, `innodb_buffer_pool_size`, `max_connections`,
`worker_connections`, opcache size — is derived from that split, so the
configuration cannot over-commit memory. Run `ngxsetup tune --explain` and it
shows you the arithmetic for every decision.

**It leans on caching rather than on hardware.** The FastCGI micro-cache is
configured with stale-while-revalidate and stampede protection, and the rules
about what may be cached are written as `map` directives against the cookies
WordPress and WooCommerce actually set. A cached response never starts a PHP
worker or opens a database connection, which is what lets a 2 GB VPS absorb
traffic that would otherwise need several times the hardware.

**Every site is isolated at the kernel level.** Each one gets its own system
user (no shell, no password), its own database and database user, and its own
PHP-FPM service running in its own mount namespace. From inside one site,
`/var/www` contains only that site — other sites do not exist, so a
compromised plugin cannot read another site's `wp-config.php` and therefore
cannot reach another site's database. Unlike a chroot, `/etc` and `/usr` stay
visible read-only, so DNS, CA certificates, and plugin and core updates keep
working with no jail plumbing to maintain. `ngxsetup doctor` proves the
boundary holds by inspecting a live worker's namespace rather than trusting
the config that produced it.

**The noise floor is absorbed before it reaches PHP.** Vulnerability
scanners, exploitation frameworks, credential-stuffing tools and
header-injection probes (Shellshock-style payloads stuffed into the
User-Agent header itself) are refused at nginx with a closed connection — a
scanner costs microseconds instead of a PHP worker and a database round trip.
Referrer-spam domains are blocked by default too. AI-training crawlers
(GPTBot, CCBot, ClaudeBot, Bytespider...) and SEO/backlink crawlers
(AhrefsBot, SemrushBot, MJ12bot...) can be blocked with `config set
block_scraper_bots true` — off by default, since unlike the rest of this list
nothing there is attacking the site; it's a content-policy choice about who
gets to crawl it for free, not a security one.

**Changes are transactional.** Files are written atomically, the affected
service is asked to validate the result, and a failure restores every file the
command touched. Running the same command twice changes nothing the second time.

## Commands

```
setup                       install and configure the stack
tune                        recompute and apply tuning for this machine
secure                      firewall, fail2ban, automatic security updates

site add <domain>           create a vhost, optionally with WordPress and TLS
site list                   list configured sites
site info <domain>          paths, database, certificate
site remove <domain>        remove a site
site enable | disable       take a site in or out of service
site fix-perms              restore correct ownership and modes
                             ('vhost' works identically to 'site'; 'create' to 'add')

status                      load, resources, service health
top                         live per-site dashboard: CPU, memory, req/s, cache hit rate
web                         browser control panel — every command above, no SSH required
doctor                      diagnose configuration, performance and security
cache purge [<domain>]      drop cached responses
ssl issue | renew           obtain or renew certificates
config get | set | show     read and change persisted settings

security scan [<domain>]    core/plugin checksum verification, malware scan
security patch [<domain>]   update outdated WordPress core, plugins and themes

borg setup | status | backup | list | restore | schedule
                             off-box, deduplicated, encrypted backups (files + database)
```

Every command accepts `--dry-run` and `--diff`.

### Web control panel

```bash
sudo ngxsetup web                        # binds 0.0.0.0, random port
sudo ngxsetup web --port 8443             # pin the port (open it ahead of time)
sudo ngxsetup web --bind 127.0.0.1        # local/VPN-only, e.g. behind an SSH tunnel
```

Everything above, from a browser — for a day-to-day operator who should be
able to create a site, watch resource usage, tail a log, run a security scan
or restore a backup without a Linux shell account or SSH key of their own.

**There is no login, on purpose.** `ngxsetup web` is meant to be started in
an operator's own active terminal session and to die with it — it must never
be installed as a systemd service or left running unattended. The access
control is "did you have a shell to start this command in the first place";
closing the terminal (or Ctrl+C) stops the server, closes the firewall port
it opened, and ends the session. Because there is no login, **where you bind
it matters**: prefer `--bind 127.0.0.1` behind an SSH tunnel, or a network
only you can reach, over leaving `--bind 0.0.0.0` (the default, chosen for
the "not a Linux user, needs to reach it directly" case) open on a shared or
public network — the tool prints this warning every time it starts. Every
mutating request still requires a same-origin fetch header
(`X-Requested-With`), which a page in another tab cannot set, as a baseline
guard against a background request from an unrelated site while the panel
happens to be open. Destructive actions (removing a site, restoring a
database, uninstalling) additionally require the browser-side confirmation
dialog and, for the biggest ones, typing the exact domain name or the word
`UNINSTALL`.

The certificate is self-signed (there is no domain name to request a real
one for), so a browser will warn on first visit — expected, not a sign of a
misconfigured server. The firewall port it ends up bound to is opened
automatically for the session (via `ufw`, if present) and closed again on a
clean shutdown, rather than requiring a manual `ufw allow` first.

The dashboard, sidebar and forms are Tailwind CSS and Font Awesome, and
resource charts (load average, memory, disk, per-site CPU and request rate)
are Chart.js — all self-hosted and embedded in the binary, so the page loads
with zero external requests even on a box with no internet access. A **Log
Viewer** page tails fail2ban, nginx access/error logs per site, PHP-FPM
logs, the database's log and the system's auth log — either a snapshot of
the last N lines or a live, polling tail — without ever reading more than a
bounded window of a file no matter how large it has grown. A site's
**Activity** view (from the Sites page) shows currently-active PHP workers,
request rate, distinct visitor IPs and a geography breakdown — the last one
only if `config set geoip_database_path <file>` points at an
operator-supplied MaxMind GeoLite2-Country `.mmdb` file; no geo database
ships with ngxsetup itself, the same reasoning as the YARA ruleset below.

### Live resource dashboard

```bash
ngxsetup top
```

A live, per-site table of CPU%, memory, active/max PHP-FPM workers,
requests/second and FastCGI cache hit rate — the same kind of view `htop`
gives you for processes, scoped to "which site is actually consuming this
box's resources right now." Press `c`/`m`/`r`/`d` to sort by CPU, memory,
request rate or domain (press again to reverse), `p` to purge the selected
site's cache, `q` to quit.

### Security scanning

```bash
ngxsetup security scan example.com     # one site
ngxsetup security scan                 # every WordPress site on the box
```

Layers, each independent so the absence of one degrades rather than disables
the scan:

- **wp-cli checksum verification** — an exact byte comparison of core and
  wordpress.org-hosted plugin files against the checksums wordpress.org
  itself publishes for that exact version. No signature database, no false
  positives.
- **ClamAV**, if installed (`apt install clamav-daemon`) — a real, actively
  maintained open-source malware signature database.
- **YARA**, if installed (`apt install yara`) — pattern-based detection
  against a bundled ruleset targeting common PHP webshell and obfuscation
  techniques. Point `--yara-rules <dir>` (or `config set
  security_yara_rules_dir <dir>`) at a larger, separately maintained ruleset
  to supplement the bundled one.
- **Built-in heuristics** — always runs; the fallback when nothing else is
  installed. Regex patterns for the well-documented shapes of obfuscated PHP
  malware (`eval(base64_decode(...))` chains, raw request input reaching
  `eval`/`system`/`exec`, the deprecated `preg_replace` `/e` modifier, known
  webshell self-identification strings, PHP files inside `wp-content/uploads`,
  disguised double file extensions).

The report also lists administrator accounts on each site, so an account
nobody remembers creating is easy to spot — planting one is a common way a
compromise persists that no file-integrity check would ever see.

```bash
ngxsetup security patch example.com    # one site, asks before applying
ngxsetup security patch --yes          # every site, no confirmation (cron-friendly)
```

Updates core first, then plugins, then themes — one item failing does not
stop the rest of the plan from being attempted.

### Database backups

```bash
sudo ngxsetup db backup example.com                    # one site
sudo ngxsetup db backup                                 # every site, one click
sudo ngxsetup db backup --out /mnt/backups/mysql         # choose the directory
```

A logical dump (`mysqldump`/`mariadb-dump --single-transaction`, a consistent
snapshot without locking writers out) to a timestamped `.sql` file per
database, root-only (`0700`/`0600`) under `/var/backups/ngxsetup/db` by
default.

```bash
sudo ngxsetup db restore example.com /var/backups/ngxsetup/db/example-com-20260101-030000.sql
```

Loads a `.sql` file into a site's database, overwriting its current contents.
Destructive, so it asks for confirmation and — unless `--no-safety-backup` is
given — backs up the database's current contents first, so restoring the
wrong file is a mistake one more command away from fixed rather than
permanent. The web UI's Backups page lists these dumps with a download link
for each.

### Off-box backups (Borg)

```bash
sudo ngxsetup borg setup --repo /mnt/backup/ngxsetup           # local disk
sudo ngxsetup borg setup --repo ssh://user@host:2222/./ngxsetup # remote, over SSH
sudo ngxsetup borg backup                                       # every site, files + database, one archive each
sudo ngxsetup borg backup example.com
sudo ngxsetup borg list
sudo ngxsetup borg restore example.com <archive> --database --files
sudo ngxsetup borg schedule daily                                # one-click "cron" — see below
```

A deduplicated, encrypted, incremental backup of each site's files *and*
database together — [BorgBackup](https://borgbackup.org), driven the same
way `mysqldump` already is: nothing sensitive on a command line, everything
through environment variables the way borg itself recommends for
unattended use. `setup` installs the `borgbackup` package if it isn't
already present, initialises an encrypted repository, and stores the
passphrase root-only at `/etc/ngxsetup/borg-passphrase` — leave `--repo`'s
passphrase prompt blank to generate a strong one, shown exactly once.

`borg backup` puts a site's whole directory tree and a fresh database dump
into a single archive, so a restore is one point in time rather than
reconciling separately-timed files and data. `borg restore` can restore the
database, the files, or both, and (for the database half) takes the same
safety-backup-first precaution `db restore` does.

`borg schedule daily` (also `hourly`, `weekly`, or a raw systemd
`OnCalendar` expression) is the one-click "put this on a schedule" this
tool offers — a systemd timer that calls back into `ngxsetup borg backup
--prune`, not a crontab entry, for the same sandboxing and hard-timeout
reasons the WordPress scheduler already runs as a timer. Its runs show up
in `journalctl -u ngxsetup-borg.service`, which the web UI's Log Viewer
page can also show. `borg schedule --disable` removes it again.

Retention (`config set borg.keep_daily|keep_weekly|keep_monthly <n>`, 0
meaning "keep everything of that granularity") is applied with `borg
backup --prune` or automatically on every scheduled run.

### Uninstalling

```bash
sudo ngxsetup uninstall                                  # asks for confirmation
sudo ngxsetup uninstall --dry-run                         # preview first
sudo ngxsetup uninstall --purge-sites --purge-packages --yes
```

Removes every file ngxsetup manages and restores the packaged defaults for
anything it overwrote (`nginx.conf`, the shared PHP-FPM pool), leaving nginx,
PHP and the database server installed and running. Site files and databases
are **kept** unless `--purge-sites` is also given; the stack packages
themselves are **kept** unless `--purge-packages` is also given. A copy of
the previous configuration is saved to `/root/ngxsetup-uninstalled-<date>/`
before anything is removed. Always shows exactly what it is about to do
before asking for confirmation.

## Tuning profiles

| Profile    | Use for                                                |
|------------|--------------------------------------------------------|
| `balanced` | a handful of ordinary WordPress sites (default)        |
| `cache`    | maximum traffic per gigabyte; the most aggressive cache |
| `density`  | many low-traffic sites on one machine                  |
| `database` | WooCommerce, large catalogues, query-heavy workloads   |

```bash
ngxsetup tune --profile=cache --explain     # preview and reasoning
ngxsetup tune --profile=cache --apply --save
```

## Requirements

- Ubuntu 22.04+ or Debian 12+, on amd64 or arm64
- Root access
- 1 GB RAM minimum; 2 GB or more recommended

## Documentation

- [INSTALL.md](INSTALL.md) — installation, first site, day-to-day operation
- [ARCHITECTURE.md](ARCHITECTURE.md) — how the tuning engine and apply pipeline work
- [MIGRATING.md](MIGRATING.md) — **upgrading from the previous version, including a
  security issue that requires action on every server it provisioned**

## Building

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ngxsetup ./cmd/ngxsetup
```

The result is a single static binary — no runtime dependencies, nothing to
install alongside it. It links a small number of pure-Go libraries at build
time (the terminal dashboard's rendering stack, and `maxminddb-golang` for
the web UI's optional GeoIP lookups; see ARCHITECTURE.md), none of which
require cgo, so `CGO_ENABLED=0` cross-compilation still produces a fully
static binary for any target architecture. The web UI's CSS/JS assets
(Tailwind, Font Awesome, Chart.js) are vendored ahead of time and embedded
the same way — see `internal/webui/frontend-src/README.md` to rebuild them.
