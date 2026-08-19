# Pairing with ngxborg

[ngxborg](https://github.com/mevijays/ngxborg) is a companion project —
a multi-tenant, PAM-authenticated Borg backup server. This page covers
pointing ngxsetup's own [Borg backup feature](backups.md) at an ngxborg
server. If your repository is local disk or a plain SSH host instead,
see [Backups](backups.md) — everything below is specific to ngxborg.

## Why they pair well

Neither tool depends on the other to function, but together they close
a real gap: ngxsetup needs *somewhere* to send off-box backups, and
ngxborg needs *nothing more than a URL and a registered key* from a
client. ngxborg never runs `borg init` on a client's behalf — repository
initialisation stays entirely client-side, matching Borg's own
architecture, so a backup's passphrase never crosses the network to the
ngxborg server at all.

## Step by step

### 1. Create the tenant and repository on ngxborg

```bash
sudo ngxborg user create backupdemo
sudo ngxborg repo create --tenant backupdemo websites
```

### 2. Point ngxsetup at it

```bash
sudo ngxsetup borg setup \
  --repo ssh://backupdemo@your-ngxborg-host:2222/var/lib/ngxborg/repos/backupdemo/websites \
  --encryption repokey-blake2 --compression zstd --generate
```

Or the web UI's Backups page: paste the same `ssh://` URL into
**Repository**, leave **SSH private key** blank to have ngxsetup
generate one, and leave **Passphrase** blank to generate that too — it
is shown once, write it down immediately.

### 3. Register the public key ngxsetup shows you

The first attempt fails with `Permission denied (publickey)` — expected:
ngxborg doesn't know this new key yet. ngxsetup prints exactly the
command you need, host and repository already filled in:

```bash
ngxborg user key add --tenant backupdemo websites 'ssh-ed25519 AAAA...'
```

Run that on the **ngxborg** host (or paste the printed public key into
its web UI's **SSH Keys → Register a key**).

### 4. Re-run setup

```bash
sudo ngxsetup borg setup --repo ssh://backupdemo@your-ngxborg-host:2222/... --generate
```

The key is now registered, `borg init` succeeds, and the repository is
ready:

```bash
ngxsetup borg status   # reachable: yes
sudo ngxsetup borg backup
```

## What to give ngxborg vs. what ngxborg gives you

| From ngxborg, you need | Into ngxsetup |
|---|---|
| The tenant username you created | The `user@` part of `--repo` |
| The repository name you created | The last path segment of `--repo` |
| The host and Borg port (`ngxborg doctor`, or its web UI's [client-commands panel](https://github.com/mevijays/ngxborg#readme)) | The rest of the `ssh://` URL |

| From ngxsetup, give ngxborg |
|---|
| The public key `borg setup` prints — via `ngxborg user key add`, or pasted into its web UI |

Nothing else needs to move between the two tools by hand — no shared
credentials, no manual `ssh-copy-id`.

## Restoring

ngxborg has no role in restoring — that stays entirely in ngxsetup,
which talks to the same repository with `borg extract`/`borg mount`
under the hood:

```bash
sudo ngxsetup borg list
sudo ngxsetup borg restore example.com <archive> --database --files
```

ngxborg's job ends at "let the right key reach the right repository."
