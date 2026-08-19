# Installing and operating ngxsetup

## Before you start

`setup` changes the machine. It installs packages, replaces `/etc/nginx/nginx.conf`,
writes PHP and database configuration, enables a firewall and restarts services.
Run it on a fresh server, or read the diff first:

```bash
sudo ./ngxsetup setup --dry-run --diff
```

Requirements: Ubuntu 22.04+ or Debian 12+, amd64 or arm64, root access, and at
least 1 GB of RAM.

## Install

```bash
# On your machine
scp ngxsetup-linux-amd64 root@YOUR_SERVER:/root/ngxsetup

# On the server
chmod +x /root/ngxsetup
sudo /root/ngxsetup setup
```

`setup` copies itself to `/usr/local/bin/ngxsetup` and creates the compatibility
aliases `vhostsetup`, `fixperm`, `mysqltune` and `loadcheck`.

Verify the download if you fetched a release:

```bash
sha256sum -c SHA256SUMS
```

### Choosing a database

MariaDB is the default and the better fit for WordPress. For MySQL:

```bash
sudo ngxsetup setup --db=mysql
```

### Adopting a server that already has the stack

```bash
sudo ngxsetup setup --skip-packages
```

## Your first site

```bash
sudo ngxsetup config set acme_email you@example.com

sudo ngxsetup site add example.com \
  --wordpress \
  --tls \
  --install \
  --admin-email you@example.com
```

This creates the system account and directory tree, provisions a database,
installs WordPress, obtains a Let's Encrypt certificate, writes the nginx server
block and the PHP-FPM pool, and completes the WordPress installation.

Credentials are written to `/root/ngxsetup-sites/<slug>.txt`, mode `0600`.

### Variations

```bash
# Empty vhost, no WordPress, no TLS
sudo ngxsetup site add example.com

# WordPress, no certificate yet — DNS not pointing here
sudo ngxsetup site add example.com --wordpress --self-signed

# Extra hostnames
sudo ngxsetup site add example.com --wordpress --tls --alias shop.example.com,eu.example.com

# Behind Cloudflare or another CDN that terminates TLS
sudo ngxsetup config set trust_cloudflare true
sudo ngxsetup secure --refresh-cloudflare
```

`--tls` requires DNS to already resolve to this server and ports 80 and 443 to be
reachable. If it is not ready yet, use `--self-signed` and upgrade later:

```bash
sudo ngxsetup ssl issue example.com
```

## Tuning

The stack is tuned during `setup`. Re-run it whenever the machine's resources
change — after a VPS resize, or after adding sites:

```bash
ngxsetup tune --explain            # show the plan and the reasoning
sudo ngxsetup tune --apply         # apply it
```

Profiles shift the memory split:

```bash
# Maximum traffic per gigabyte: the most aggressive caching posture
sudo ngxsetup tune --profile=cache --apply --save

# Many low-traffic sites: on-demand pools, minimal idle footprint
sudo ngxsetup tune --profile=density --apply --save

# WooCommerce or a large catalogue: more memory to the buffer pool
sudo ngxsetup tune --profile=database --apply --save
```

`--save` persists the choice so later commands use it too.

If your sites run heavy page builders, tell the tuner what a worker really
costs, and it will size the pool accordingly:

```bash
ngxsetup tune --php-worker-mb 160 --explain
```

You can also plan for a machine you do not have yet:

```bash
ngxsetup tune --memory-mb 16384 --explain
```

## Day-to-day

```bash
ngxsetup status                  # load, memory, disk, services, cache
ngxsetup doctor                  # diagnose problems, with the fix for each
ngxsetup site list
ngxsetup site info example.com

sudo ngxsetup cache purge example.com
sudo ngxsetup cache purge         # everything
ngxsetup cache stats

sudo ngxsetup site disable example.com   # take out of service, keep everything
sudo ngxsetup site enable example.com
```

`doctor` exits non-zero when a check fails, so it works as a monitoring probe:

```bash
*/10 * * * * root /usr/local/bin/ngxsetup doctor >/dev/null || echo "ngxsetup doctor failed on $(hostname)"
```

## Watching resource usage live

```bash
ngxsetup top
```

A live per-site table — CPU%, memory, active/max PHP-FPM workers,
requests/second, FastCGI cache hit rate — for answering "which site is doing
this to my box" without reaching for `htop` and cross-referencing PIDs by
hand. `c`/`m`/`r`/`d` sort by CPU, memory, request rate or domain (press
again to reverse); `p` purges the selected site's cache; `q` quits.

## Scanning for compromise

```bash
sudo ngxsetup security scan example.com
sudo ngxsetup security scan               # every WordPress site
```

Verifies WordPress core and wordpress.org-hosted plugin files against
published checksums, scans with ClamAV and YARA when installed
(`apt install clamav-daemon yara` — both optional; the scan still runs
without them, just with fewer layers), and always runs a built-in heuristic
pass for obfuscated-malware patterns regardless of what else is installed.
Lists administrator accounts too, so one nobody remembers creating is easy to
spot.

```bash
sudo ngxsetup security patch example.com   # asks before applying
sudo ngxsetup security patch --yes         # every site, unattended (cron-friendly)
```

Shows exactly what would update — core, plugins, themes — before touching
anything. One item failing does not stop the rest of the plan.

## Customising a site

Generated files are overwritten on the next apply. Per-site additions belong in
the override file, which is created once and never rewritten:

```bash
sudo nano /etc/nginx/sites-available/<slug>.custom.conf
sudo nginx -t && sudo systemctl reload nginx
```

It is included at the end of the server block, so it can add locations, set
redirects, or override earlier directives.

Server-wide policy goes through `config`:

```bash
ngxsetup config show
sudo ngxsetup config set admin_allow_list 203.0.113.4,198.51.100.0/24
sudo ngxsetup config set block_xmlrpc true
sudo ngxsetup config set upload_max_mb 256
sudo ngxsetup tune --apply
```

Setting `admin_allow_list` restricts `/wp-admin` and `/wp-login.php` to those
addresses on every site — credential stuffing then never reaches WordPress.

## Removing a site

Nothing is deleted unless you ask:

```bash
# Remove the vhost and pool; keep files and database
sudo ngxsetup site remove example.com

# Remove everything
sudo ngxsetup site remove example.com --purge-files --purge-db
```

The second form asks for confirmation and tells you exactly what it will destroy.

## Backing up databases

```bash
sudo ngxsetup db backup example.com                # one site
sudo ngxsetup db backup                             # every site, one click
sudo ngxsetup db backup --out /mnt/backups/mysql     # choose where the .sql files land
```

Each database gets its own timestamped `.sql` file (`mysqldump`/`mariadb-dump
--single-transaction`, so writers are never blocked), written root-only under
`/var/backups/ngxsetup/db` unless you pass `--out`. Running it against every
site backs each one up independently — one database failing to dump does not
stop the others.

## Uninstalling

```bash
sudo ngxsetup uninstall --dry-run     # see exactly what would happen first
sudo ngxsetup uninstall               # asks for confirmation, then does it
```

Removes every file ngxsetup wrote or manages and restores the packaged
defaults for anything it overwrote — `nginx.conf`, the shared PHP-FPM `www`
pool — so nginx, PHP and the database server go back to running their
distribution defaults. A copy of the configuration that was in place gets
saved to `/root/ngxsetup-uninstalled-<timestamp>/` first, in case you want it
later.

Two things are kept by default, the same "nothing destroyed unless you ask"
rule every other destructive command in this tool follows:

```bash
sudo ngxsetup uninstall --purge-sites       # also delete every site's files, database, system user
sudo ngxsetup uninstall --purge-packages    # also remove nginx, PHP and the database server
sudo ngxsetup uninstall --purge-sites --purge-packages --yes   # full clean slate, no prompt
```

## phpMyAdmin

Disabled by default. It is an internet-facing application with full database
access, so enabling it requires saying who may reach it:

```bash
sudo ngxsetup config set phpmyadmin.allow_list 203.0.113.4
sudo ngxsetup config set phpmyadmin.enabled true
sudo ngxsetup secure --phpmyadmin-user admin      # prompts for a password
sudo ngxsetup secure --apply
```

It then listens on port 9443 (configurable), restricted to the allowlist, behind
an HTTP credential, running as its own user in its own PHP pool. It is not
mounted on any site.

## Where things live

| Path | Contents |
|---|---|
| `/etc/ngxsetup/config.json` | operator settings |
| `/var/lib/ngxsetup/state.json` | the site registry |
| `/var/lib/ngxsetup/backups/` | timestamped backups of every modified file |
| `/var/www/<slug>/public` | document root |
| `/var/www/<slug>/{tmp,sessions}` | outside the web root, per site |
| `/etc/nginx/sites-available/<slug>.conf` | generated vhost |
| `/etc/nginx/sites-available/<slug>.custom.conf` | your additions |
| `/etc/php/<v>/fpm/pool.d/<slug>.conf` | generated pool |
| `/var/log/nginx/<slug>.{access,error}.log` | per-site logs |
| `/root/ngxsetup-sites/<slug>.txt` | credentials, mode 0600 |

## Troubleshooting

**A change was rejected and rolled back.** The error includes the service's own
output. Nothing was left on disk; the previous versions are in
`/var/lib/ngxsetup/backups/<timestamp>/`.

**Certificate issuance failed.** Confirm DNS resolves to this server and that
ports 80 and 443 are reachable from the internet. Create the site with
`--self-signed` and run `ngxsetup ssl issue <domain>` once DNS is ready.

**Pages are not being cached.** Check the `X-Cache-Status` response header. A
`BYPASS` means one of the skip rules matched — a login cookie, a POST, or an
administrative path. `MISS` followed by `HIT` on a second request is correct.

**The site is slow.** Run `ngxsetup doctor`. If memory is the constraint, try
`ngxsetup tune --profile=cache --apply`, which shifts the budget toward serving
more traffic from cache and fewer requests from PHP.

**A configuration file will not update.** ngxsetup refuses to overwrite files it
did not create. Move yours aside, or pass `--force` if you are sure.
