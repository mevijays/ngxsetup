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
```

The dependency direction is one-way: `facts` knows nothing about `tuning`,
`tuning` knows nothing about the filesystem, and `tmpl` knows nothing about how
its output is written. `provision` is the only package that combines them.

### Dependencies

The binary is static (`CGO_ENABLED=0` cross-compiles cleanly for any target),
but it is no longer built purely from the standard library: `internal/tui`
links `charmbracelet/bubbletea`, `bubbles` and `lipgloss` for the live
dashboard's rendering. All three are pure Go — no cgo, so the static-binary
property holds — and are widely used, actively maintained terminal-UI
libraries rather than something narrow or unaudited. Every other package
remains standard-library only.

`internal/security` shells out to two *optional* external tools rather than
linking anything: ClamAV (`clamscan`) and YARA (`yara`), both detected at
runtime via `Look()` and skipped gracefully — with the gap reported, not
hidden — when absent. Neither is a build dependency; a binary built on a
machine with neither installed still works, and simply runs fewer detection
layers against whatever machine it is deployed to. This mirrors how the
provisioning side already treats optional packages like `libnginx-mod-http-brotli`
or `redis-server`.

## The tuning engine

`tuning.Compute(facts.Facts, Options) Plan` is a pure function. It performs no
I/O, runs no commands and reads no environment. That is what makes the sizing
testable: the test suite runs it against synthetic hardware from a 1 GB
single-core VPS to a 64 GB 32-core box and asserts properties that must hold
everywhere — that the plan never commits more memory than the machine has, that
`worker_rlimit_nofile` always covers `worker_connections`, that PHP-FPM's
`dynamic` pool values satisfy the relationships FPM validates at startup.

### The memory budget

Everything starts here:

```
total RAM
  − reserve for the kernel, sshd, systemd, monitoring
  = usable

usable × db_weight   → database
usable × php_weight  → PHP worker pool
remainder            → left free, on purpose
```

The remainder is not slack. The kernel page cache serves the FastCGI cache and
every static asset out of it, so free memory is what turns a cache hit into a
memory read. A tuner that allocates 100% of RAM to services produces a slower
server than one that does not.

Weights come from the profile. The reserve shrinks proportionally as machines
grow, because the base system cost is largely fixed.

### Deriving the rest

Each number follows from the budget and from what actually constrains it:

- **PHP `max_children`** is the smaller of `php_budget ÷ per_worker_MB` and
  `cores × 8`. Memory decides how many workers can coexist; CPU decides how many
  can make progress. `tune --explain` names which one bound the result.
- **`max_connections`** is `max_children + 25`. Only PHP workers, wp-cli and
  admin sessions can open connections, so sizing it from RAM (as the previous
  tuner did, at 100 per gigabyte) creates thousands of slots nothing will use
  while charging real memory for the buffers behind them.
- **`innodb_buffer_pool_size`** is the database budget minus per-connection
  buffers and fixed overheads, then rounded *down* to a multiple of
  `instances × chunk_size`. InnoDB rounds up otherwise, which would push it past
  its budget.
- **InnoDB I/O settings** come from the detected storage class. Rotational and
  solid-state differ by an order of magnitude in the right `io_capacity`; an
  unknown device defaults to the SSD profile rather than being guessed as
  rotational, which would cripple fast storage.
- **`worker_rlimit_nofile`** is derived from `worker_connections`, so the two
  always agree.

### Flavour awareness

MariaDB removed `innodb_buffer_pool_instances` in 10.5 and refuses to start if
it is present. MySQL 8 removed the query cache. `utf8mb4_0900_ai_ci` exists only
on MySQL. `default_authentication_plugin` was removed in MySQL 8.4. The plan
records which flavour is installed and the template branches accordingly, which
is why the same code produces a working configuration on both.

## The apply pipeline

```
Plan ──> tmpl.Render ──> render.Writer.Write ──> service validates ──> commit
                                    │                    │
                                 backup              on failure
                                    │                    │
                                    └────── rollback ────┘
```

Four properties make this safe to run on a live server:

**Atomic.** Every write goes through a temporary file in the same directory,
fsynced, then renamed. A reader sees the old file or the new one, never a
truncated one.

**Idempotent.** Content identical to what is already on disk is reported as
unchanged and not written. Rendering is deterministic — no timestamps, no
hostnames, no random values — so "has anything changed?" has a real answer.

**Journalled.** Modified files are copied into a timestamped backup directory
that mirrors their original paths. A failed validation restores every file the
command touched.

**Bounded.** Files that do not carry the `Managed by ngxsetup` marker are never
overwritten without `--force`. Configuration a human wrote survives.

Validation is real: `nginx -t` for nginx, `php-fpm -t` for PHP, and for the
database a restart followed by fifteen seconds of watching, because neither
MariaDB nor every MySQL build offers an offline config validator and InnoDB can
abort a moment after systemd reports the unit active.

## Site isolation

Each site gets:

- a system account `web-<slug>` with no shell (`/usr/sbin/nologin`) and no password hash at all
- its own database and database user, granted only on `<db>.*`
- **its own PHP-FPM service**, in its own mount namespace, running as that account
- `open_basedir` confined to its own tree plus its own tmp and session directories
- directories at `2750` and files at `0640`, owned `web-<slug>:www-data`

The setgid bit on directories is the load-bearing detail for the permission
layer: files WordPress creates inherit the `www-data` group, so nginx can read
newly uploaded media, while `0750` means no other site's account can traverse
into the tree at all. The packaged `www` pool is removed during setup, because
leaving it would give any site a way to run as `www-data` and undo all of this.

### The jail

Permissions alone were not the whole story. `open_basedir` is a *userland*
check inside PHP, not a kernel boundary, and it has a long history of
bypasses. So each site runs as its own systemd service
(`ngxsetup-fpm@<slug>.service`, one template unit instantiated per site)
whose confinement is enforced by the kernel:

```
TemporaryFileSystem=/var/www:ro     # /var/www becomes an empty tmpfs...
BindPaths=/var/www/%i               # ...containing only this one site
ProtectSystem=strict                # everything else read-only
ReadWritePaths=/var/www/%i …        # the complete writable surface
PrivateDevices=true  PrivateTmp=true  NoNewPrivileges=true
```

From inside a site's namespace, other sites are not merely unreadable —
they do not exist. Verified on a real host: with five directories under
`/var/www`, a worker in one site's namespace lists exactly one, and reading
another site's `wp-config.php` returns *No such file or directory* rather
than a permission error.

**Why namespaces rather than chroot.** A chroot starts empty, so DNS
resolution, CA certificates and glibc's NSS modules all have to be copied or
bind-mounted into every jail and kept in sync with the host forever. Get it
wrong and WordPress silently loses outbound HTTPS, and plugin and core
updates stop working. Here `/etc` and `/usr` stay visible read-only, so all
of that keeps working untouched — confirmed live: DNS resolves, the CA
bundle is present, and an HTTPS request to `api.wordpress.org` succeeds from
inside the jail. `PrivateDevices=true` supplies `/dev/urandom` too, which a
chroot would have needed a hand-made device node for.

Two things fall out of the per-site-service design beyond isolation. A
per-instance drop-in sets `MemoryMax`, so a leaking site is capped at the
memory the tuner budgeted for PHP overall rather than growing into the
database's share. And adding or removing a site now restarts only that
site's service — the shared-service model had to bounce PHP for every site
on the box, briefly dropping in-flight requests everywhere.

The cost is one master process per site (~12 MB, much of it shared pages),
which the tuning engine charges against the memory budget up front rather
than discovering as a shortfall later.

`ngxsetup doctor` verifies this empirically rather than trusting the config:
it enters a live worker's mount namespace with `nsenter` and checks whether
another site's directory is visible. Checking the unit file would only
confirm what was *intended* — a directive ignored by an older systemd, or a
service still running from a stale unit because nobody reloaded, leaves
correct-looking config and no isolation.

## Caching

The FastCGI micro-cache is the main performance mechanism. Three parts matter:

**What may be cached** is decided by `map` directives matching the cookies
WordPress and WooCommerce actually set, the request method, and a list of
personal or administrative paths. Notably *not* excluded: feeds, sitemaps,
paginated archives and URLs with query strings — the previous configuration
excluded all of those, which meant almost no real traffic was ever cached.

**The cache key** normalises away campaign and click-tracking parameters, so a
URL whose query string is nothing but `utm_*` and `fbclid` folds onto its clean
form. Every shared social link becomes a hit instead of a miss.

**The serving policy** combines `fastcgi_cache_lock` (a stampede of concurrent
misses for one key collapses into a single upstream request) with
`fastcgi_cache_background_update` and `use_stale` (an expired entry is served
immediately while it refreshes behind the scenes). Together they mean a cache
expiry is not a latency spike.

Purging reads the key nginx stores in each cache file's header, so per-site
purge works on a stock nginx without the third-party cache-purge module — which
the previous configuration referenced but never installed.

## Live stats (`ngxsetup top`)

The same pure/impure split runs through this too. `internal/stats` gathers
four independent signals per site:

- **CPU and memory** come from `/proc/[pid]/stat` and `/proc/[pid]/status`
  for every worker process matching a pool's title (`php-fpm: pool <slug>` —
  confirmed against a real php-fpm, not assumed), not from FPM's own status
  page. That avoids having to expose a new HTTP endpoint through nginx just
  to read it. CPU is a rate, so it needs two samples: `Sampler` keeps the
  previous tick's reading per site and hands the delta to a pure
  `CPUPercent(prev, cur, elapsed, ticksPerSec)`, tested with fabricated
  process samples covering a pool at rest, a busy multi-core pool (400%+ is
  correct, not a bug to clamp), a worker that just forked with no baseline
  yet, and one recycled by `pm.max_requests` between samples.
- **Request rate and cache hit ratio** come from tailing each site's own
  access log — `Tailer` tracks a byte offset per path, returns only lines
  appended since the last call, and survives logrotate's create-or-truncate
  either way. The cache field is pulled from the exact `log_format` string
  nginx.conf.tmpl defines; a test renders that template and asserts the
  field is still there, so a future edit to the log format breaks loudly
  instead of leaving `top` silently blind to cache status.
- **Database size** is one `information_schema` query covering every site's
  schema at once, refreshed on its own slower timer (default 10s) — unlike
  CPU and request rate, this costs a real query, and a dashboard ticking
  every 2 seconds has no need for a number that does not change that often.

`internal/tui` is bubbletea's Elm architecture: `Update` is a pure state
transition (`msg` in, new `Model` + next `Cmd` out), with every real I/O
operation — sampling, purging a cache — pushed into a `tea.Cmd` the runtime
executes off to the side. That split is what makes keybindings, sorting and
the purge flow testable by feeding the model fake messages and inspecting the
result, the same way `tuning.Compute` is tested without a server. The one
mutating action reachable from the dashboard is a cache purge — deliberately
the only one: a live dashboard is not where a site should be removable by a
stray keypress.

## The web UI (`ngxsetup web`)

`internal/webui` is a second front end on the same engine the CLI drives, not
a second engine. Every handler either reads a fresh `provision.Ctx` the same
way a CLI command does, or calls the exact function a CLI command would call
(`AddSite`, `RemoveSite`, `ApplySecurity`...) — there is no parallel
provisioning logic to keep in sync. `internal/provision/settings.go`
(`ConfigRows`, `SetConfigKey`) and `RequireSetup` exist specifically so the
CLI and the web UI read from one definition of "what settings exist" and
"is the stack installed," rather than two copies that could drift.

Mutating handlers capture `logx` output into a buffer for the duration of one
request (`runCaptured`) and return it verbatim as the response's `output`
field — the browser shows the identical transcript the CLI would have
printed for the same action, for free, instead of a second human-readable
summary written by hand for every endpoint. A package-level mutex serializes
every mutating action for the same reason: `provision.Ctx` and `logx` both
assume one command runs at a time in one process, true for the CLI by
construction but not for an HTTP server that can receive two requests at
once. Read-only endpoints (status, site list, live stats) skip the lock.

**There is no login**, deliberately, not as a first cut of something a
later session was going to add. This command is designed to be started in an
operator's own active terminal and to die with it — never a systemd service,
never left running unattended — so the access control is "did you have a
shell to start this command in the first place," which a password in front
of the same command would not meaningfully add to. `cmdWeb` layers a second
`signal.NotifyContext` for `SIGHUP` — what a process actually receives when
its controlling terminal goes away — on top of the CLI's own SIGINT/SIGTERM
handling, scoped to this one command rather than added globally, since
aborting `setup` mid-`apt-get install` on a dropped SSH connection would
trade one problem for a worse one. Catching SIGHUP rather than leaving it at
its default disposition (which also kills the process, just without
`Serve`'s deferred firewall-rule cleanup running first) is what makes a
closed terminal a *clean* shutdown rather than an abrupt one. The one guard
that survives dropping auth is a lightweight CSRF-style check: every
mutating request must carry an `X-Requested-With` header only same-origin
JavaScript can set, which stops a page in another browser tab from firing a
background request at this server while it happens to be running — cheap
insurance against a passive drive-by, not a substitute for binding the
listener somewhere only the operator can reach it (`--bind 127.0.0.1` behind
an SSH tunnel), which is what the startup banner recommends.

The frontend (`internal/webui/static/`) is embedded via `go:embed` and
framework-free on the JS side — vanilla `fetch()` and a small hash-free view
router, no npm dependency tree for a tool whose entire pitch is "one binary,
nothing else to install." Styling is Tailwind CSS and Font Awesome, and
dashboard charts are Chart.js — but all three are *compiled/vendored ahead
of time* rather than pulled from a CDN at request time: `frontend-src/`
holds Tailwind's utility-class source (compiled via the standalone
Tailwind CLI — no Node required — into `static/vendor/tailwind.css`, see
`frontend-src/README.md`), and Font Awesome's CSS+webfonts and Chart.js's
UMD bundle are vendored as static files. The served page makes zero external
requests, which matters more here than it would for an ordinary web app:
this is meant to work on a box with no internet access at all.

The log viewer (`internal/webui/logs.go`) and a site's Activity panel
(`handlers_activity.go`) are both built around one constraint: never load
the server. `snapshotTail` and `tailFrom` never read more than a fixed
window (`maxTailWindow`, 256 KiB) off disk regardless of how large the
underlying file has grown — the same trade-off `tail` itself makes on a file
with no line index — and live tailing is a client-side poll against a
byte-offset cursor rather than a held-open streaming connection per viewer,
consistent with how `/api/stats` already works. `tailFrom` only returns
lines whose trailing newline has actually been written, holding back a
partial line until the next poll, which is what makes it behave like a real
`tail -f` instead of occasionally emitting a line mid-write. Visitor
geography is opt-in and never bundled: `config geoip_database_path` points
at an operator-supplied MaxMind-format `.mmdb` file (read via the small,
pure-Go `maxminddb-golang`, no cgo), the same reasoning
`security_yara_rules_dir` already applies to a bigger malware ruleset —
a real GeoIP database runs several megabytes and goes stale, and deciding to
license a current one is the operator's call, not this tool's.

The one piece of infrastructure this command manages that the rest of the
tool does not: the firewall. `ngxsetup web` can bind an arbitrary or random
port, and ufw's default-deny policy blocks it until something explicitly
allows it — confirmed live, where the listener came up correctly (reachable
from the box itself) while every off-box request got no response at all,
because nothing had told ufw about the port. `Serve` opens it on start and
closes it again on a clean shutdown, so a past session's random port does
not accumulate as a standing exception forever.

## Security scanning

`internal/security` layers four detection methods, each independent so one's
absence degrades rather than disables the scan:

1. **wp-cli checksum verification** — an exact byte comparison against the
   checksums wordpress.org publishes for that exact version. Authoritative
   for anything hosted there; plugins that are not (premium, custom) are
   reported as *uncheckable*, not silently treated as clean.
2. **ClamAV**, if installed — shells out to `clamscan`; the signature
   database is ClamAV's own, updated via `freshclam`. This package vendors
   nothing here on purpose — a dedicated, continuously updated project does
   that job better than a bundled copy ever could stay current.
3. **YARA**, if installed — a bundled ruleset (`internal/security/rules`,
   embedded via `go:embed`) written for this project and validated against a
   real `yara` binary during development, both that it compiles (YARA's
   regex dialect does not support non-capturing groups — `(?:...)` — a real,
   easy-to-ship-broken constraint the test suite checks against a live
   binary, not by assumption) and that it matches real samples of the
   patterns it targets with zero false positives on realistic WordPress
   code. `--yara-rules <dir>` / `security_yara_rules_dir` supplements it with
   a larger, separately maintained ruleset without replacing the bundled one.
4. **Built-in heuristics** — regex patterns for well-documented PHP malware
   shapes (`eval` of decoded/decompressed input, raw request input reaching
   `eval`/`exec`/`system`, the deprecated `preg_replace` `/e` modifier, known
   webshell markers, PHP files under `wp-content/uploads`, disguised double
   extensions). Always runs — the floor every scan has regardless of what is
   installed. Tested against real malicious samples for detection and against
   realistic WordPress/plugin code for false positives, since a scanner that
   cries wolf on legitimate code teaches an operator to ignore it.

wp-cli always runs as the target site's own system user (`runuser -u
web-<slug>`), never as root — the same isolation boundary site.go's WordPress
install already relies on, so auditing a site cannot itself become a way to
reach every other site on the box.

`security patch` gathers a plan (outdated core/plugin/theme versions) before
touching anything, so an operator approves the actual diff rather than taking
"apply updates" on faith. Core updates first, then plugins, then themes; one
item failing does not stop the rest of the plan — an operator who approved
five updates should get four applied, not zero because the first one failed.

## Testing

- `tuning` — property tests over synthetic hardware; no server needed
- `facts` — synthetic `/proc` and `/sys` via an injected source
- `tmpl` — every template renders, braces balance in every branch, flavour
  correctness, regression guards on each defect found in the legacy configs
- `render` — idempotency, rollback, backup layout, refusal to clobber
- `provision` — the whole apply pipeline against a temporary root
- `db`, `site` — identifier and domain validation as security boundaries
- `stats` — CPU accounting math against fabricated process samples, log
  parsing against real access-log lines, sampler orchestration with a fake
  DB querier
- `tui` — bubbletea `Update` transitions via fed messages; no terminal needed
- `security` — heuristic and YARA rules against real malicious and
  legitimate code samples (the YARA suite also compiles and runs the bundled
  ruleset through a real `yara` binary when one is available), wp-cli output
  parsing against realistic sample text, patch planning and partial-failure
  handling
- CI — installs a real nginx, PHP-FPM and MariaDB, applies the configuration,
  and asserts that all three accept it, that a site serves, that the pool runs
  as the site user, and that a second apply changes nothing

The unit tests can only prove that templates render. Only the CI integration
job can prove that what they render is valid.
