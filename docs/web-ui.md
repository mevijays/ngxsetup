# Web UI guide

```bash
sudo ngxsetup web                        # binds 127.0.0.1, random port; reach it over an SSH tunnel
sudo ngxsetup web --port 8443             # pin the port
sudo ngxsetup web --bind 0.0.0.0          # every interface (opens the port in ufw for the session)
```

Everything the CLI can do, from a browser — for a day-to-day operator
who should be able to create a site, watch resource usage, tail a log,
run a security scan or restore a backup without a Linux shell account
or SSH key of their own.

## Signing in

`ngxsetup web` is meant to be started in an operator's own active
terminal session and to die with it — it must **never** be installed as
a systemd service or left running unattended. Closing the terminal (or
Ctrl+C) stops the server, closes any firewall port it opened, and ends
every session.

Each run prints a URL containing a random sign-in token:

```
https://127.0.0.1:41234/login?token=…
```

Opening it sets a session cookie (`Secure`, `HttpOnly`,
`SameSite=Strict`) and drops the token from the address bar. Without
that cookie nothing is served — not the page, not the API, not a backup
download. The token and session exist only in the running process, so
restarting the command signs every browser out. Treat the URL like a
root password for as long as the command runs.

By default the server listens on `127.0.0.1` only. From your own
computer, tunnel in and open the printed URL locally:

```bash
ssh -L 41234:127.0.0.1:41234 you@your-server
```

`--bind 0.0.0.0` (or a specific address) exposes it directly; the port
is then opened in `ufw` for the session and closed again on a clean
shutdown. Prefer the tunnel on shared or public networks.

Every mutating request still requires a same-origin fetch header
(`X-Requested-With`), which a page in another tab cannot set, as a
baseline guard against a background request from an unrelated site
while the panel happens to be open. Destructive actions (removing a
site, restoring a database, uninstalling) additionally require the
browser-side confirmation dialog and, for the biggest ones, typing the
exact domain name or the word `UNINSTALL`.

The certificate is self-signed (there is no domain name to request a
real one for), so a browser will warn on first visit — expected, not a
sign of a misconfigured server.

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

**A WordPress site's WordPress page** (from the Sites page) shows the
core version and available updates, and lists plugins, themes and users
with one-click activate, deactivate, update, delete and password reset,
plus a plugin installer, search-and-replace, cache and rewrite flushes
and a box for any other wp-cli command. All of it runs through the same
sandboxed wp-cli as `ngxsetup wp`. A reset password is generated on the
server, passed to wp-cli on stdin, and shown once.

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
