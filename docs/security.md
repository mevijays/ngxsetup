# Security

## The noise floor is absorbed before it reaches PHP

Vulnerability scanners, exploitation frameworks, credential-stuffing
tools and header-injection probes (Shellshock-style payloads stuffed
into the User-Agent header itself) are refused at nginx with a closed
connection — a scanner costs microseconds instead of a PHP worker and a
database round trip. Referrer-spam domains are blocked by default too.

AI-training crawlers (GPTBot, CCBot, ClaudeBot, Bytespider...) and
SEO/backlink crawlers (AhrefsBot, SemrushBot, MJ12bot...) can be
blocked with:

```bash
sudo ngxsetup config set block_scraper_bots true
sudo ngxsetup tune --apply
```

Off by default, since unlike the rest of this list nothing there is
attacking the site; it's a content-policy choice about who gets to
crawl it for free, not a security one.

## Restricting admin access

```bash
sudo ngxsetup config set admin_allow_list 203.0.113.4,198.51.100.0/24
sudo ngxsetup config set block_xmlrpc true
sudo ngxsetup tune --apply
```

`admin_allow_list` restricts `/wp-admin` and `/wp-login.php` to those
addresses on every site — credential stuffing then never reaches
WordPress.

## Site isolation

Each site runs as its own system user with no shell, in its own
PHP-FPM service, inside its own kernel-enforced mount namespace — a
compromised plugin on one site cannot read another site's
`wp-config.php`, because from inside its namespace the other site does
not exist. See [Architecture → Site isolation](architecture.md#site-isolation)
for the full mechanism and why it's namespaces rather than a chroot.

## `secure`

```bash
sudo ngxsetup secure            # firewall (ufw), fail2ban, automatic security updates
sudo ngxsetup secure --apply
```

## Malware scanning

```bash
ngxsetup security scan example.com     # one site
ngxsetup security scan                 # every WordPress site on the box
```

Four layers, each independent so the absence of one degrades rather
than disables the scan:

1. **wp-cli checksum verification** — an exact byte comparison of core
   and wordpress.org-hosted plugin files against the checksums
   wordpress.org itself publishes for that exact version. No signature
   database, no false positives. Plugins not hosted there (premium,
   custom) are reported as *uncheckable*, not silently treated as clean.
2. **ClamAV**, if installed (`ngxsetup security install-clamav`, or
   `apt install clamav-daemon`) — a real, actively maintained
   open-source malware signature database. This project vendors
   nothing here on purpose.
3. **YARA**, if installed (`apt install yara`) — pattern-based
   detection against a bundled ruleset targeting common PHP webshell
   and obfuscation techniques. Point `--yara-rules <dir>` (or
   `config set security_yara_rules_dir <dir>`) at a larger, separately
   maintained ruleset to supplement the bundled one.
4. **Built-in heuristics** — always runs; the fallback when nothing
   else is installed. Regex patterns for the well-documented shapes of
   obfuscated PHP malware (`eval(base64_decode(...))` chains, raw
   request input reaching `eval`/`system`/`exec`, the deprecated
   `preg_replace` `/e` modifier, known webshell self-identification
   strings, PHP files inside `wp-content/uploads`, disguised double
   file extensions).

The report also lists administrator accounts on each site, so an
account nobody remembers creating is easy to spot — planting one is a
common way a compromise persists that no file-integrity check would
ever see.

wp-cli always runs as the target site's own system user
(`runuser -u web-<slug>`), never as root — the same isolation boundary
site creation already relies on, so auditing a site cannot itself
become a way to reach every other site on the box.

## Patching

```bash
ngxsetup security patch example.com    # one site, asks before applying
ngxsetup security patch --yes          # every site, no confirmation (cron-friendly)
```

Shows exactly what would update — core, plugins, themes — before
touching anything, and updates core first, then plugins, then themes.
One item failing does not stop the rest of the plan from being
attempted — an operator who approved five updates should get four
applied, not zero because the first one failed.

## phpMyAdmin

Disabled by default. It is an internet-facing application with full
database access, so enabling it requires saying who may reach it — see
[Getting started → phpMyAdmin](getting-started.md#phpmyadmin).

## Backups as a security control

An off-box, encrypted backup is the thing standing between a
compromise and total data loss. See [Backups](backups.md).
