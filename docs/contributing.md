# Contributing

Contributions are welcome — bug reports, documentation fixes, and pull
requests alike.

## Development setup

```bash
git clone https://github.com/mevijays/ngxsetup.git
cd ngxsetup
go build ./...
```

Pure Go, no cgo — builds on macOS, Windows or Linux. It only *runs* on
Debian/Ubuntu (it drives `apt`, `systemd`, nginx and PHP-FPM directly),
so testing anything beyond the unit suite needs a real or virtual
Ubuntu/Debian target. `tuning`, `facts`, `tmpl`, `render`, `stats` and
`tui` are all designed to be fully unit-testable without one — see
[Architecture → Testing](architecture.md#testing).

## Running the tests

```bash
gofmt -l .              # should print nothing
go vet ./...
go test -race -count=1 ./...
```

CI additionally runs a full end-to-end job: installs a real nginx,
PHP-FPM and MariaDB, applies the configuration, creates a real
WordPress site, confirms isolation holds, and asserts that a second
`tune --apply` changes — and reloads — nothing. See
`.github/workflows/ci.yml` to run the same steps locally in a
disposable VM.

## Code style

- Run `gofmt` before committing; CI fails otherwise.
- This codebase favors comments that explain *why* a piece of code
  looks the way it does — especially around a real bug that shaped it —
  over comments that restate *what* the next line obviously does. If
  you're fixing something non-obvious, a sentence on what broke and why
  the fix works this way is more valuable than the diff alone.
- Keep the "pure core, impure edges" split: `tuning`, `facts`, `tmpl`
  are pure functions with no I/O, which is what makes them fast and
  reliable to test. Prefer extending that split over reaching for a
  live system call from inside one of them.
- No CDN dependencies in the web UI. Tailwind CSS, Font Awesome, and
  Chart.js are vendored ahead of time under
  `internal/webui/static/vendor/` — keep it that way; the tool's whole
  pitch includes working on a box with no internet access.
- Prefer a real, disposable Ubuntu/Debian host (or VM/container) for
  testing anything that touches `nginx`, `systemd`, PHP-FPM, or the
  database — mocking these convincingly is harder than standing up the
  real thing, and this project's own history has caught more than one
  bug (socket-activated sshd, `MkdirAll` ownership, disk-drift-driven
  reload flapping) that only a real target surfaced.

## Making a pull request

1. Fork the repository and create a branch off `main`.
2. Keep the change focused — a pull request that does one thing is much
   easier to review than one that reorganizes unrelated code along the
   way.
3. Add or update tests for anything behavioral. A change with no test
   coverage for what it fixes is much likelier to regress silently
   later.
4. Make sure `gofmt`, `go vet`, and `go test -race ./...` all pass
   locally before opening the PR — CI runs the same checks, but
   catching it locally is faster for everyone.
5. Describe *why*, not just *what*, in the PR description — especially
   for a bug fix: what was the actual failure, and how did you confirm
   the fix addresses it?

## Reporting bugs

Please include:

- The exact command (or web UI action) and what happened, verbatim
  (error text, not a paraphrase).
- `sudo ngxsetup doctor`'s output.
- Your OS/distribution and `ngxsetup version`'s output.

## Reporting a security issue

Please open an issue at
[github.com/mevijays/ngxsetup/issues](https://github.com/mevijays/ngxsetup/issues).
For anything you'd rather not disclose publicly before a fix ships,
mention that in the issue and a maintainer will follow up privately.
