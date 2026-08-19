# CLI reference

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
security install-clamav     install and configure the ClamAV daemon

db backup | restore         local database dumps

borg setup | status | backup | list | restore | schedule
                             off-box, deduplicated, encrypted backups (files + database)

migrate discover | run      pull a WordPress site from a remote server over SSH
```

Every command accepts `--dry-run` and `--diff`.

See [Getting started](getting-started.md) for `site`/`config`/`ssl`
walkthroughs, [Tuning](tuning.md) for `tune`, [Security](security.md)
for `security`, [Backups](backups.md) for `db` and `borg`, and
[Web UI guide](web-ui.md) for `web`.

## `setup`

```bash
sudo ngxsetup setup                    # install and configure everything
sudo ngxsetup setup --dry-run --diff   # preview first
sudo ngxsetup setup --db=mysql         # MySQL instead of the MariaDB default
sudo ngxsetup setup --skip-packages    # adopt a server that already has the stack
```

## `tune`

```bash
ngxsetup tune --explain                       # show the plan and the reasoning
sudo ngxsetup tune --apply                    # apply it
sudo ngxsetup tune --profile=cache --apply --save
ngxsetup tune --php-worker-mb 160 --explain   # tell it what a worker really costs
ngxsetup tune --memory-mb 16384 --explain     # plan for a machine you don't have yet
```

## `status` / `doctor` / `top`

```bash
ngxsetup status     # load, memory, disk, services, cache — one screen
ngxsetup doctor     # diagnose problems, with the fix for each; exits non-zero on failure
ngxsetup top         # live per-site resource dashboard
```

## `site`

```bash
sudo ngxsetup site add example.com --wordpress --tls --install --admin-email you@example.com
ngxsetup site list
ngxsetup site info example.com
sudo ngxsetup site disable example.com   # take out of service, keep everything
sudo ngxsetup site enable example.com
sudo ngxsetup site remove example.com                          # keep files and database
sudo ngxsetup site remove example.com --purge-files --purge-db  # remove everything
sudo ngxsetup site fix-perms example.com
```

## `cache`

```bash
sudo ngxsetup cache purge example.com
sudo ngxsetup cache purge         # every site
ngxsetup cache stats
```

## `ssl`

```bash
sudo ngxsetup ssl issue example.com    # after starting with --self-signed
sudo ngxsetup ssl renew                # renews everything due
```

## `config`

```bash
ngxsetup config show
sudo ngxsetup config set <key> <value>
ngxsetup config get <key>
```

## `secure`

```bash
sudo ngxsetup secure                          # firewall, fail2ban, unattended-upgrades
sudo ngxsetup secure --refresh-cloudflare      # re-derive the trusted-proxy list
sudo ngxsetup secure --phpmyadmin-user admin   # set up phpMyAdmin's HTTP credential
sudo ngxsetup secure --apply
```

## `web`

```bash
sudo ngxsetup web                        # binds 0.0.0.0, random port
sudo ngxsetup web --port 8443             # pin the port (open it ahead of time)
sudo ngxsetup web --bind 127.0.0.1        # local/VPN-only, e.g. behind an SSH tunnel
```

## `migrate`

```bash
ngxsetup migrate discover --host old-server.example.com --user root --key ~/.ssh/id_ed25519
sudo ngxsetup migrate run --host old-server.example.com --user root --key ~/.ssh/id_ed25519 example.com
sudo ngxsetup migrate run --host old-server.example.com --user root --key ~/.ssh/id_ed25519 --all
```

Discovers WordPress installs on a remote server over SSH, then pulls one
or more across (files and database) into new local sites — for moving
off a server that never had ngxsetup on it. `--all` migrates every site
`discover` found instead of naming domains individually.

## `uninstall`

```bash
sudo ngxsetup uninstall --dry-run
sudo ngxsetup uninstall
sudo ngxsetup uninstall --purge-sites --purge-packages --yes
```

## `version`

```bash
ngxsetup version
```
