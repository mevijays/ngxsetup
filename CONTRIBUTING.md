# Contributing to ngxsetup

Thanks for considering it. The short version:

```bash
go build ./...
gofmt -l .              # should print nothing
go vet ./...
go test -race -count=1 ./...
```

Pure Go, no cgo — builds anywhere. It only *runs* on Debian/Ubuntu (it
drives `apt`, `systemd`, nginx and PHP-FPM directly), so testing
anything beyond the unit suite needs a real or virtual Ubuntu/Debian
target — see CI's own end-to-end job in `.github/workflows/ci.yml` for
exactly what that looks like.

Open an issue before a large change so we can agree on the approach
first; small fixes and docs corrections can just be a pull request.

**The full guide** — code style, what a good PR looks like, what to
include in a bug report, and how to report a security issue privately —
lives at
**[docs/contributing.md](docs/contributing.md)** (also published at
[mevijays.github.io/ngxsetup/contributing](https://mevijays.github.io/ngxsetup/contributing/)).
