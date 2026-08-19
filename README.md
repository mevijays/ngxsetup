# ngxsetup

[![CI](https://github.com/mevijays/ngxsetup/actions/workflows/ci.yml/badge.svg)](https://github.com/mevijays/ngxsetup/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/mevijays/ngxsetup?include_prereleases)](https://github.com/mevijays/ngxsetup/releases)
[![Docs](https://img.shields.io/badge/docs-mevijays.github.io%2Fngxsetup-blue)](https://mevijays.github.io/ngxsetup/)
[![License: MIT](https://img.shields.io/badge/license-MIT-green)](LICENSE)

A single binary that turns a bare Ubuntu or Debian server into a tuned,
hardened WordPress host — nginx, PHP-FPM and MariaDB or MySQL — sized
for the machine it's running on.

```bash
sudo ./ngxsetup setup
sudo ngxsetup site add example.com --wordpress --tls --admin-email you@example.com
```

No agent, no runtime dependencies, no configuration management server.
Every template it needs is embedded in the binary. Sizes the stack from
a real memory budget, isolates every site at the kernel level (its own
user, its own PHP-FPM service, its own mount namespace), and re-applies
idempotently — a second run with nothing changed does, and reloads,
nothing.

## Install

```bash
curl -fLO https://github.com/mevijays/ngxsetup/releases/latest/download/ngxsetup-linux-amd64
chmod +x ngxsetup-linux-amd64
sudo ./ngxsetup-linux-amd64 setup
```

`arm64` builds are published too. See
[Installation](https://mevijays.github.io/ngxsetup/installation/) for
checksums, building from source, and choosing MySQL over the default
MariaDB.

## Documentation

Full docs: **[mevijays.github.io/ngxsetup](https://mevijays.github.io/ngxsetup/)**

- [Getting started](https://mevijays.github.io/ngxsetup/getting-started/)
- [Architecture](https://mevijays.github.io/ngxsetup/architecture/)
- [CLI reference](https://mevijays.github.io/ngxsetup/cli-reference/)
- [Tuning](https://mevijays.github.io/ngxsetup/tuning/)
- [Security](https://mevijays.github.io/ngxsetup/security/)
- [Backups](https://mevijays.github.io/ngxsetup/backups/) — local
  database dumps and off-box, encrypted Borg backups, including pairing
  with [ngxborg](https://github.com/mevijays/ngxborg)

## Contributing

Issues and pull requests are welcome. See
[CONTRIBUTING.md](CONTRIBUTING.md) for dev setup, running tests, and
what makes a good PR — it's a short read.

## License

[MIT](LICENSE)
