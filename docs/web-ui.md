# Web UI guide

```bash
sudo ngxsetup web                        # binds 0.0.0.0, random port
sudo ngxsetup web --port 8443             # pin the port (open it ahead of time)
sudo ngxsetup web --bind 127.0.0.1        # local/VPN-only, e.g. behind an SSH tunnel
```

Everything the CLI can do, from a browser — for a day-to-day operator
who should be able to create a site, watch resource usage, tail a log,
run a security scan or restore a backup without a Linux shell account
or SSH key of their own.

## There is no login, on purpose

`ngxsetup web` is meant to be started in an operator's own active
terminal session and to die with it — it must **never** be installed as
a systemd service or left running unattended. The access control is
"did you have a shell to start this command in the first place";
closing the terminal (or Ctrl+C) stops the server, closes the firewall
port it opened, and ends the session.

Because there is no login, **where you bind it matters**: prefer
`--bind 127.0.0.1` behind an SSH tunnel, or a network only you can
reach, over leaving `--bind 0.0.0.0` (the default, chosen for the "not
a Linux user, needs to reach it directly" case) open on a shared or
public network — the tool prints this warning every time it starts.

Every mutating request still requires a same-origin fetch header
(`X-Requested-With`), which a page in another tab cannot set, as a
baseline guard against a background request from an unrelated site
while the panel happens to be open. Destructive actions (removing a
site, restoring a database, uninstalling) additionally require the
browser-side confirmation dialog and, for the biggest ones, typing the
exact domain name or the word `UNINSTALL`.

The certificate is self-signed (there is no domain name to request a
real one for), so a browser will warn on first visit — expected, not a
sign of a misconfigured server. The firewall port it ends up bound to
is opened automatically for the session (via `ufw`, if present) and
closed again on a clean shutdown, rather than requiring a manual
`ufw allow` first.

## What's in it

The dashboard, sidebar and forms are Tailwind CSS and Font Awesome, and
resource charts (load average, memory, disk, per-site CPU and request
rate) are Chart.js — all self-hosted and embedded in the binary, so the
page loads with zero external requests even on a box with no internet
access.

**Log Viewer** tails fail2ban, nginx access/error logs per site,
PHP-FPM logs, the database's log and the system's auth log — either a
snapshot of the last N lines or a live, polling tail — without ever
reading more than a bounded window of a file no matter how large it has
grown.

**A site's Activity view** (from the Sites page) shows currently-active
PHP workers, request rate, distinct visitor IPs and a geography
breakdown — the last one only if `config set geoip_database_path
<file>` points at an operator-supplied MaxMind GeoLite2-Country
`.mmdb` file; no geo database ships with ngxsetup itself, for the same
reason no YARA ruleset beyond the small bundled one does (see
[Security](security.md)).

**Backups** lists local database dumps with a download link for each,
and covers the same Borg setup/status/backup/restore flow as the CLI
— see [Backups](backups.md).

See [Architecture → The web UI](architecture.md#the-web-ui-ngxsetup-web)
for how it's built.
