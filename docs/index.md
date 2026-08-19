# ngxsetup

A single binary that turns a bare Ubuntu or Debian server into a tuned,
hardened WordPress host — nginx, PHP-FPM and MariaDB or MySQL — sized for
the machine it is running on.

```bash
sudo ./ngxsetup setup
sudo ngxsetup site add example.com --wordpress --tls --admin-email you@example.com
```

No agent, no runtime dependencies, no configuration management server.
Every template it needs is embedded in the binary.

## What it does differently

**It sizes the stack from a memory budget, not from a table of
recommended values.** Total RAM is split explicitly between the
database, the PHP worker pool, and memory deliberately left free for the
kernel page cache. Every other number — `max_children`,
`innodb_buffer_pool_size`, `max_connections`, `worker_connections`,
opcache size — is derived from that split, so the configuration cannot
over-commit memory. Run `ngxsetup tune --explain` and it shows you the
arithmetic for every decision. See [Tuning](tuning.md).

**It leans on caching rather than on hardware.** The FastCGI micro-cache
is configured with stale-while-revalidate and stampede protection, and
the rules about what may be cached are written as `map` directives
against the cookies WordPress and WooCommerce actually set. A cached
response never starts a PHP worker or opens a database connection.

**Every site is isolated at the kernel level.** Each one gets its own
system user, its own database, and its own PHP-FPM service running in
its own mount namespace — from inside one site, `/var/www` contains
only that site. See [Architecture → Site isolation](architecture.md#site-isolation).

**The noise floor is absorbed before it reaches PHP.** Scanners,
exploitation frameworks, credential-stuffing tools and header-injection
probes are refused at nginx with a closed connection. See
[Security](security.md).

**Changes are transactional.** Files are written atomically, the
affected service validates the result, and a failure restores every
file the command touched. Running the same command twice changes
nothing the second time — verified in CI against a real nginx/PHP/
MariaDB/WordPress stack on every push.

## Where to go next

- New to ngxsetup? Start with [Installation](installation.md), then
  [Getting started](getting-started.md).
- Want the reasoning before you run it? Read
  [Architecture](architecture.md), [Tuning](tuning.md), and
  [Security](security.md).
- Setting up backups? See [Backups](backups.md) — local database dumps
  and off-box, deduplicated, encrypted backups via
  [Borg](https://borgbackup.org), including pairing with
  [ngxborg](https://github.com/mevijays/ngxborg), a companion backup
  server — see [Pairing with ngxborg](ngxborg-integration.md).
- Something not working? Check [Troubleshooting](troubleshooting.md).

## License

ngxsetup is [MIT licensed](https://github.com/mevijays/ngxsetup/blob/main/LICENSE).
Contributions are welcome — see [Contributing](contributing.md).
