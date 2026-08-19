package borg

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"ngxsetup/internal/logx"
	"ngxsetup/internal/system"
)

// SSHKeyPath and SSHPubKeyPath are a keypair dedicated to reaching a
// remote borg repository — never an operator's own SSH identity, and
// never shared with anything else this tool does. Confirmed live: without
// a key of its own, this package fell back to whatever default identity
// `ssh` happened to resolve for the operator running the command, which
// worked by accident from an interactive shell with an agent running and
// failed outright ("Permission denied (publickey)") the moment `borg`
// setup or a backup ran the way it actually runs in practice — as root,
// non-interactively, with no agent and no ~/.ssh/config entry for a host
// it had never been told about. Generating (or importing) one key and
// wiring BORG_RSH to it explicitly removes that dependency on whatever
// SSH state the invoking shell happened to have.
// SSHKeyPath and SSHPubKeyPath are vars, not consts, so tests can point
// them at a throwaway directory instead of touching the real
// /etc/ngxsetup on whatever machine runs `go test`.
var (
	SSHKeyPath    = "/etc/ngxsetup/borg-ssh-key"
	SSHPubKeyPath = SSHKeyPath + ".pub"
)

// HasSSHKey reports whether a dedicated key has already been generated or
// imported.
func HasSSHKey() bool {
	_, err := os.Stat(SSHKeyPath)
	return err == nil
}

// PublicKey returns the dedicated key's public half — what an operator
// pastes into the remote repository server's own access-control step
// (ngxborg's `user key add`, or an equivalent `authorized_keys` entry by
// hand). Empty if no key exists yet.
func PublicKey() (string, error) {
	body, err := os.ReadFile(SSHPubKeyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading %s: %w", SSHPubKeyPath, err)
	}
	return strings.TrimSpace(string(body)), nil
}

// EnsureSSHKey makes sure a dedicated key exists, generating a fresh
// ed25519 keypair if none does and importPrivateKey is empty, or
// importing importPrivateKey (an operator-supplied private key — pasted
// into the web UI, or passed via --ssh-key-file on the CLI) otherwise.
// Returns the public key and whether a new one was generated, so a caller
// can decide whether this is worth showing the operator (a freshly
// generated key needs registering on the remote end; an already-existing
// or freshly-imported one usually does not need re-announcing).
//
// Re-running this with importPrivateKey empty on a host that already has
// a key is a no-op — it never silently replaces a key already in use,
// which would orphan every repository already registered against the old
// one's public half. Pass importPrivateKey explicitly to replace it on
// purpose.
func EnsureSSHKey(ctx context.Context, r system.Runner, importPrivateKey string) (pubKey string, generated bool, err error) {
	if err := os.MkdirAll(dirOf(SSHKeyPath), 0o755); err != nil {
		return "", false, err
	}

	if importPrivateKey != "" {
		if err := importSSHKey(ctx, r, importPrivateKey); err != nil {
			return "", false, err
		}
		pub, err := PublicKey()
		return pub, false, err
	}

	if HasSSHKey() {
		pub, err := PublicKey()
		return pub, false, err
	}

	if err := r.Run(ctx, "ssh-keygen",
		"-t", "ed25519",
		"-N", "", // no passphrase on the key itself — it is already root-only (0600), and a passphrase would block unattended backups
		"-C", "ngxsetup-borg",
		"-f", SSHKeyPath,
	); err != nil {
		return "", false, fmt.Errorf("generating a borg SSH key: %w", err)
	}
	if err := os.Chmod(SSHKeyPath, 0o600); err != nil {
		return "", false, err
	}
	logx.Change("generated a dedicated SSH key for borg at %s", SSHKeyPath)
	pub, err := PublicKey()
	return pub, true, err
}

// importSSHKey validates and stores an operator-supplied private key.
// Validation is deliberately real, not cosmetic: `ssh-keygen -y` refuses
// anything that is not actually a private key it can parse, so a pasted
// public key, a corrupted paste, or a passphrase-protected key (which
// would hang an unattended backup waiting for input this tool can never
// supply) is caught here rather than surfacing later as a confusing
// connection failure.
func importSSHKey(ctx context.Context, r system.Runner, privateKey string) error {
	privateKey = strings.TrimRight(privateKey, "\n") + "\n"
	if !strings.Contains(privateKey, "PRIVATE KEY") {
		return fmt.Errorf("that does not look like a private key (expected a PEM block containing \"PRIVATE KEY\")")
	}
	tmp := SSHKeyPath + ".importing"
	if err := os.WriteFile(tmp, []byte(privateKey), 0o600); err != nil {
		return fmt.Errorf("writing the imported key: %w", err)
	}
	// ssh-keygen -y derives (and prints) the public half from a private
	// key file, which doubles here as the validation step: it fails
	// loudly on anything malformed or passphrase-protected rather than
	// this tool discovering that later, mid-backup, as an opaque SSH
	// connection failure.
	if _, err := r.Output(ctx, "ssh-keygen", "-y", "-f", tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("that key could not be read (it may be passphrase-protected, which an unattended backup can never unlock): %w", err)
	}
	if err := os.Rename(tmp, SSHKeyPath); err != nil {
		return err
	}
	pub, err := r.Output(ctx, "ssh-keygen", "-y", "-f", SSHKeyPath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(SSHPubKeyPath, []byte(pub+"\n"), 0o644); err != nil {
		return err
	}
	logx.Change("imported the supplied SSH key for borg at %s", SSHKeyPath)
	return nil
}

// SuggestRemoteKeyCommand builds the `ngxborg user key add` command an
// operator would run on the repository's own host to register the public
// key EnsureSSHKey just produced — parsed straight out of the same repo
// URL already given to --repo/the web UI, so there is nothing left to
// transcribe by hand between the two tools. Returns "" for anything that
// is not a recognisable ssh:// URL rather than guessing. Shared between
// the CLI and web UI so both surfaces suggest the identical command.
func SuggestRemoteKeyCommand(repo, pubKey string) string {
	u, err := url.Parse(repo)
	if err != nil || u.Scheme != "ssh" || u.User == nil {
		return ""
	}
	tenant := u.User.Username()
	path := u.Path
	if path == "" {
		return ""
	}
	repoName := path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		repoName = path[i+1:]
	}
	if tenant == "" || repoName == "" {
		return ""
	}
	return fmt.Sprintf("ngxborg user key add --tenant %s %s '%s'", tenant, repoName, pubKey)
}
