# Getting started

## Your first site

```bash
sudo ngxsetup config set acme_email you@example.com

sudo ngxsetup site add example.com \
  --wordpress \
  --tls \
  --install \
  --admin-email you@example.com
```

This creates the system account and directory tree, provisions a
database, installs WordPress, obtains a Let's Encrypt certificate,
writes the nginx server block and the PHP-FPM pool, and completes the
WordPress installation.

Credentials are written to `/root/ngxsetup-sites/<slug>.txt`, mode
`0600`.

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

`--tls` requires DNS to already resolve to this server and ports 80 and
443 to be reachable. If it is not ready yet, use `--self-signed` and
upgrade later:

```bash
sudo ngxsetup ssl issue example.com
```

## Tuning for this machine

The stack is tuned during `setup`. Re-run it whenever the machine's
resources change — after a VPS resize, or after adding sites:

```bash
ngxsetup tune --explain            # show the plan and the reasoning
sudo ngxsetup tune --apply         # apply it
```

See [Tuning](tuning.md) for profiles and the reasoning behind every
number.

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

`doctor` exits non-zero when a check fails, so it works as a monitoring
probe:

```
*/10 * * * * root /usr/local/bin/ngxsetup doctor >/dev/null || echo "ngxsetup doctor failed on $(hostname)"
```

## Watching resource usage live

```bash
ngxsetup top
```

A live per-site table — CPU%, memory, active/max PHP-FPM workers,
requests/second, FastCGI cache hit rate. `c`/`m`/`r`/`d` sort by CPU,
memory, request rate or domain (press again to reverse); `p` purges the
selected site's cache; `q` quits.

## Customising a site

Generated files are overwritten on the next apply. Per-site additions
belong in the override file, which is created once and never rewritten:

```bash
sudo nano /etc/nginx/sites-available/<slug>.custom.conf
sudo nginx -t && sudo systemctl reload nginx
```

It is included at the end of the server block, so it can add locations,
set redirects, or override earlier directives.

Server-wide policy goes through `config`:

```bash
ngxsetup config show
sudo ngxsetup config set admin_allow_list 203.0.113.4,198.51.100.0/24
sudo ngxsetup config set block_xmlrpc true
sudo ngxsetup config set upload_max_mb 256
sudo ngxsetup tune --apply
```

Setting `admin_allow_list` restricts `/wp-admin` and `/wp-login.php` to
those addresses on every site — credential stuffing then never reaches
WordPress.

## Removing a site

Nothing is deleted unless you ask:

```bash
# Remove the vhost and pool; keep files and database
sudo ngxsetup site remove example.com

# Remove everything
sudo ngxsetup site remove example.com --purge-files --purge-db
```

The second form asks for confirmation and tells you exactly what it
will destroy.

## phpMyAdmin

Disabled by default. It is an internet-facing application with full
database access, so enabling it requires saying who may reach it:

```bash
sudo ngxsetup config set phpmyadmin.allow_list 203.0.113.4
sudo ngxsetup config set phpmyadmin.enabled true
sudo ngxsetup secure --phpmyadmin-user admin      # prompts for a password
sudo ngxsetup secure --apply
```

It then listens on port 9443 (configurable), restricted to the
allowlist, behind an HTTP credential, running as its own user in its
own PHP pool. It is not mounted on any site.

## Uninstalling

```bash
sudo ngxsetup uninstall --dry-run     # see exactly what would happen first
sudo ngxsetup uninstall               # asks for confirmation, then does it
```

Removes every file ngxsetup wrote or manages and restores the packaged
defaults for anything it overwrote — `nginx.conf`, the shared PHP-FPM
`www` pool — so nginx, PHP and the database server go back to running
their distribution defaults. A copy of the configuration that was in
place gets saved to `/root/ngxsetup-uninstalled-<timestamp>/` first.

Two things are kept by default, the same "nothing destroyed unless you
ask" rule every other destructive command in this tool follows:

```bash
sudo ngxsetup uninstall --purge-sites       # also delete every site's files, database, system user
sudo ngxsetup uninstall --purge-packages    # also remove nginx, PHP and the database server
sudo ngxsetup uninstall --purge-sites --purge-packages --yes   # full clean slate, no prompt
```

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

## Next steps

- [Web UI guide](web-ui.md) — the same operations, from a browser.
- [Backups](backups.md) — local database dumps and off-box Borg backups.
- [Security](security.md) — scanning, patching, and the hardening this
  tool applies by default.
