# Architecture

## The shape of the program

```
cmd/ngxsetup            entry point
internal/
  cli/                  command surface, flag parsing
  facts/                what this machine is — CPU, RAM, cgroups, storage class
  tuning/               facts + options -> a complete configuration decision
  tmpl/                 embedded config templates and their data contracts
  render/               atomic writes, backups, diffs, rollback
  provision/            composes the above into setup, tune, site, doctor
  state/                the registry of what has been provisioned
  config/               persisted operator policy
  db/                   database provisioning
  site/                 name derivation and validation
  system/               apt, systemd, users, command execution
  logx/                 output
  stats/                live per-site resource sampling (CPU, mem, req rate, cache hit rate)
  tui/                  the `top` live dashboard, built on the stats layer
  security/             malware scanning and wp-cli-driven patching
  webui/                the embedded browser control panel
  borg/                 off-box backup client (see Backups)
```

The dependency direction is one-way: `facts` knows nothing about
`tuning`, `tuning` knows nothing about the filesystem, and `tmpl` knows
nothing about how its output is written. `provision` is the only
package that combines them.

## Dependencies

The binary is static (`CGO_ENABLED=0` cross-compiles cleanly for any
target), but it is not built purely from the standard library:
`internal/tui` links `charmbracelet/bubbletea`, `bubbles` and
`lipgloss` for the live dashboard's rendering, and `internal/webui`
links `maxminddb-golang` for optional GeoIP lookups. All are pure Go —
no cgo, so the static-binary property holds.

`internal/security` shells out to two *optional* external tools rather
than linking anything: ClamAV (`clamscan`) and YARA (`yara`), both
detected at runtime and skipped gracefully — with the gap reported, not
hidden — when absent. See [Security](security.md).

## The tuning engine

`tuning.Compute(facts.Facts, Options) Plan` is a pure function — no I/O,
no commands run, no environment read — which is what makes the sizing
testable: the test suite runs it against synthetic hardware from a
1 GB single-core VPS to a 64 GB 32-core box and asserts properties that
must hold everywhere. See [Tuning](tuning.md) for the memory budget and
how every number is derived from it.

## The apply pipeline

```
Plan ──> tmpl.Render ──> render.Writer.Write ──> service validates ──> commit
                                    │                    │
                                 backup              on failure
                                    │                    │
                                    └────── rollback ────┘
```

Four properties make this safe to run on a live server:

**Atomic.** Every write goes through a temporary file in the same
directory, fsynced, then renamed. A reader sees the old file or the new
one, never a truncated one.

**Idempotent.** Content identical to what is already on disk is
reported as unchanged and not written. Rendering is deterministic — no
timestamps, no hostnames, no random values — so "has anything changed?"
has a real answer. Whether a *reload* actually happens is tracked
separately and just as carefully: `render.Writer.TotalChanges()` counts
real file changes across an entire multi-step apply (nginx, PHP,
database, kernel limits), so a re-apply that changes nothing also
*reloads* nothing — verified live after a real bug where it didn't.

**Journalled.** Modified files are copied into a timestamped backup
directory that mirrors their original paths. A failed validation
restores every file the command touched.

**Bounded.** Files that do not carry the `Managed by ngxsetup` marker
are never overwritten without `--force`. Configuration a human wrote
survives.

Validation is real: `nginx -t` for nginx, `php-fpm -t` for PHP, and for
the database a restart followed by fifteen seconds of watching, because
neither MariaDB nor every MySQL build offers an offline config
validator and InnoDB can abort a moment after systemd reports the unit
active.

## Site isolation

Each site gets:

- a system account `web-<slug>` with no shell (`/usr/sbin/nologin`) and
  no password hash at all
- its own database and database user, granted only on `<db>.*`
- **its own PHP-FPM service**, in its own mount namespace, running as
  that account
- `open_basedir` confined to its own tree plus its own tmp and session
  directories
- directories at `2750` and files at `0640`, owned `web-<slug>:www-data`

The setgid bit on directories is the load-bearing detail for the
permission layer: files WordPress creates inherit the `www-data` group,
so nginx can read newly uploaded media, while `0750` means no other
site's account can traverse into the tree at all. The packaged `www`
pool is removed during setup, because leaving it would give any site a
way to run as `www-data` and undo all of this.

### The jail

Permissions alone are not the whole story. `open_basedir` is a
*userland* check inside PHP, not a kernel boundary, and it has a long
history of bypasses. So each site runs as its own systemd service
(`ngxsetup-fpm@<slug>.service`, one template unit instantiated per
site) whose confinement is enforced by the kernel:

```ini
TemporaryFileSystem=/var/www:ro     # /var/www becomes an empty tmpfs...
BindPaths=/var/www/%i               # ...containing only this one site
ProtectSystem=strict                # everything else read-only
ReadWritePaths=/var/www/%i …        # the complete writable surface
PrivateDevices=true  PrivateTmp=true  NoNewPrivileges=true
```

From inside a site's namespace, other sites are not merely unreadable —
they do not exist. Verified on a real host: with five directories under
`/var/www`, a worker in one site's namespace lists exactly one, and
reading another site's `wp-config.php` returns *No such file or
directory* rather than a permission error.

**Why namespaces rather than chroot.** A chroot starts empty, so DNS
resolution, CA certificates and glibc's NSS modules all have to be
copied or bind-mounted into every jail and kept in sync with the host
forever. Get it wrong and WordPress silently loses outbound HTTPS, and
plugin and core updates stop working. Here `/etc` and `/usr` stay
visible read-only, so all of that keeps working untouched — confirmed
live: DNS resolves, the CA bundle is present, and an HTTPS request to
`api.wordpress.org` succeeds from inside the jail.

Two things fall out of the per-site-service design beyond isolation. A
per-instance drop-in sets `MemoryMax`, so a leaking site is capped at
the memory the tuner budgeted for PHP overall rather than growing into
the database's share. And adding or removing a site restarts only that
site's service — a shared-service model has to bounce PHP for every
site on the box, briefly dropping in-flight requests everywhere.

`ngxsetup doctor` verifies this empirically rather than trusting the
config: it enters a live worker's mount namespace with `nsenter` and
checks whether another site's directory is visible. Checking the unit
file would only confirm what was *intended* — a directive ignored by an
older systemd, or a service still running from a stale unit, leaves
correct-looking config and no isolation.

## Live stats (`ngxsetup top`)

The same pure/impure split runs through this too. `internal/stats`
gathers three independent signals per site:

- **CPU and memory** come from `/proc/[pid]/stat` and
  `/proc/[pid]/status` for every worker process matching a pool's title
  (`php-fpm: pool <slug>`), not from FPM's own status page — no new
  HTTP endpoint needed through nginx just to read it. CPU is a rate,
  so `Sampler` keeps the previous tick's reading per site and hands the
  delta to a pure `CPUPercent(prev, cur, elapsed, ticksPerSec)`.
- **Request rate and cache hit ratio** come from tailing each site's
  own access log — `Tailer` tracks a byte offset per path, returns only
  lines appended since the last call, and survives logrotate's
  create-or-truncate either way.
- **Database size** is one `information_schema` query covering every
  site's schema at once, refreshed on its own slower timer (default
  10s) — unlike CPU and request rate, this costs a real query.

`internal/tui` is bubbletea's Elm architecture: `Update` is a pure
state transition (`msg` in, new `Model` + next `Cmd` out), with every
real I/O operation pushed into a `tea.Cmd` the runtime executes off to
the side — the same split that makes `tuning.Compute` testable without
a server. The one mutating action reachable from the dashboard is a
cache purge, deliberately the only one.

## The web UI (`ngxsetup web`)

`internal/webui` is a second front end on the same engine the CLI
drives, not a second engine. Every handler either reads a fresh
`provision.Ctx` the same way a CLI command does, or calls the exact
function a CLI command would call — there is no parallel provisioning
logic to keep in sync.

Mutating handlers capture `logx` output into a buffer for the duration
of one request and return it verbatim as the response's `output` field
— the browser shows the identical transcript the CLI would have
printed for the same action. A package-level mutex serializes every
mutating action, since `provision.Ctx` and `logx` both assume one
command runs at a time in one process — true for the CLI by
construction but not for an HTTP server that can receive two requests
at once.

**There is no login**, deliberately. This command is designed to be
started in an operator's own active terminal and to die with it — never
a systemd service, never left running unattended — so the access
control is "did you have a shell to start this command in the first
place." The one guard that survives dropping auth is a lightweight
CSRF-style check: every mutating request must carry an
`X-Requested-With` header only same-origin JavaScript can set. See
[Web UI guide](web-ui.md) for how to run it and why *where* you bind it
matters.

The frontend is embedded via `go:embed` and framework-free on the JS
side — vanilla `fetch()` and a small hash-free view router. Styling
(Tailwind CSS, Font Awesome) and charts (Chart.js) are
*compiled/vendored ahead of time* rather than pulled from a CDN, so the
served page makes zero external requests — it works on a box with no
internet access at all.

## Testing

- `tuning` — property tests over synthetic hardware; no server needed
- `facts` — synthetic `/proc` and `/sys` via an injected source
- `tmpl` — every template renders, braces balance in every branch,
  flavour correctness, regression guards on each defect found
- `render` — idempotency, rollback, backup layout, refusal to clobber
- `provision` — the whole apply pipeline against a temporary root
- `db`, `site` — identifier and domain validation as security boundaries
- `stats` — CPU accounting math against fabricated process samples, log
  parsing against real access-log lines
- `tui` — bubbletea `Update` transitions via fed messages; no terminal
  needed
- `security` — heuristic and YARA rules against real malicious and
  legitimate code samples, wp-cli output parsing, patch planning
- **CI** — installs a real nginx, PHP-FPM and MariaDB, applies the
  configuration, and asserts that all three accept it, that a site
  serves, that the pool runs as the site user, and that a second apply
  changes — and reloads — nothing.

The unit tests can only prove that templates render. Only the CI
integration job can prove that what they render is valid.
