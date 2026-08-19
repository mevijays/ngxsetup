# Installation

## Requirements

- Ubuntu 22.04+ or Debian 12+, on amd64 or arm64
- Root access
- 1 GB RAM minimum; 2 GB or more recommended

## Before you start

`setup` changes the machine. It installs packages, replaces
`/etc/nginx/nginx.conf`, writes PHP and database configuration, enables
a firewall and restarts services. Run it on a fresh server, or read the
diff first:

```bash
sudo ./ngxsetup setup --dry-run --diff
```

## Download a release

```bash
# amd64
curl -fLO https://github.com/mevijays/ngxsetup/releases/latest/download/ngxsetup-linux-amd64
# arm64
curl -fLO https://github.com/mevijays/ngxsetup/releases/latest/download/ngxsetup-linux-arm64
```

Verify against the published checksums:

```bash
curl -fLO https://github.com/mevijays/ngxsetup/releases/latest/download/SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing
```

## Install

```bash
# On your machine
scp ngxsetup-linux-amd64 root@YOUR_SERVER:/root/ngxsetup

# On the server
chmod +x /root/ngxsetup
sudo /root/ngxsetup setup
```

`setup` copies itself to `/usr/local/bin/ngxsetup` and creates the
compatibility aliases `vhostsetup`, `fixperm`, `mysqltune` and
`loadcheck`.

### Choosing a database

MariaDB is the default and the better fit for WordPress. For MySQL:

```bash
sudo ngxsetup setup --db=mysql
```

### Adopting a server that already has the stack

```bash
sudo ngxsetup setup --skip-packages
```

## Building from source

```bash
git clone https://github.com/mevijays/ngxsetup.git
cd ngxsetup
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ngxsetup ./cmd/ngxsetup
```

The result is a single static binary — no runtime dependencies, nothing
to install alongside it. It links a small number of pure-Go libraries
at build time (the terminal dashboard's rendering stack, and
`maxminddb-golang` for the web UI's optional GeoIP lookups — see
[Architecture](architecture.md)), none of which require cgo, so
`CGO_ENABLED=0` cross-compilation still produces a fully static binary
for any target architecture. The web UI's CSS/JS assets (Tailwind, Font
Awesome, Chart.js) are vendored ahead of time and embedded the same way
— see `internal/webui/frontend-src/README.md` in the repository to
rebuild them.

!!! note "macOS and Windows"
    ngxsetup only *runs* on Linux (it drives `apt`, `systemd`, nginx and
    PHP-FPM directly), but the binary itself cross-compiles cleanly from
    any platform with a Go toolchain — build it on macOS or Windows and
    copy the result to your server, as the example above does.

## Next steps

Continue with [Getting started](getting-started.md) to create your
first site.
