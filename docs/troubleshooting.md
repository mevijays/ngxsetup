# Troubleshooting

## A change was rejected and rolled back

The error includes the service's own output (`nginx -t`, `php-fpm -t`,
or the database's startup log). Nothing was left on disk; the previous
versions are in `/var/lib/ngxsetup/backups/<timestamp>/`. See
[Architecture → The apply pipeline](architecture.md#the-apply-pipeline).

## Certificate issuance failed

Confirm DNS resolves to this server and that ports 80 and 443 are
reachable from the internet. Create the site with `--self-signed` and
run `ngxsetup ssl issue <domain>` once DNS is ready.

## Pages are not being cached

Check the `X-Cache-Status` response header. A `BYPASS` means one of the
skip rules matched — a login cookie, a POST, or an administrative path.
`MISS` followed by `HIT` on a second request is correct. See
[Tuning → Caching](tuning.md#caching).

## The site is slow

Run `ngxsetup doctor`. If memory is the constraint, try
`ngxsetup tune --profile=cache --apply`, which shifts the budget toward
serving more traffic from cache and fewer requests from PHP. See
[Tuning](tuning.md).

## A configuration file will not update

ngxsetup refuses to overwrite files it did not create — see
[Architecture → The apply pipeline](architecture.md#the-apply-pipeline)
("Bounded"). Move yours aside, or pass `--force` if you are sure.

## `tune --apply` says something changed, but I didn't touch anything

Genuinely nothing to worry about if it's the *first* apply after a real
change (a new site, a resized VPS, a config value you set). If it keeps
reporting a change on every single re-run with nothing else different,
that's worth a bug report — rendering is deterministic by design and a
second, truly no-op apply should report (and do) nothing; see
[Architecture → The apply pipeline](architecture.md#the-apply-pipeline)
for what "idempotent" is supposed to mean here.

## `borg setup` fails with "Permission denied (publickey)"

Expected on the very first attempt against a remote repository: the
server doesn't know the key ngxsetup just generated yet. ngxsetup
prints the public key and, if the remote end runs ngxborg, the exact
command to register it — see
[Pairing with ngxborg](ngxborg-integration.md) for the full sequence.

## I lost a repository passphrase

There's no recovery path — ngxsetup never stores it anywhere retrievable
after the one-time display, and neither does Borg itself. A repository
without its passphrase is permanently unreadable. This is why both the
CLI and web UI show a generated passphrase exactly once, with an
explicit warning to write it down immediately.

## Browser warns about the certificate (`ngxsetup web`)

Expected — the panel serves a self-signed certificate, since there is
no domain name to request a real one for. See
[Web UI guide](web-ui.md).

## Still stuck?

Please [open an issue](https://github.com/mevijays/ngxsetup/issues/new)
with the output of `sudo ngxsetup doctor`, the exact command and error
(verbatim, not a paraphrase), and your OS/distribution — see
[Contributing](contributing.md).
