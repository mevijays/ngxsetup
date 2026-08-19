package borg

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ngxsetup/internal/system"
)

// requireSSHKeygen skips a test when ssh-keygen isn't on PATH — the same
// trade-off requireBorg makes for the real borg binary in borg_test.go.
func requireSSHKeygen(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen is not installed; skipping")
	}
}

// withTempKeyPaths points SSHKeyPath/SSHPubKeyPath at a throwaway
// directory for the duration of one test, and restores the real paths
// afterwards — without this every test in this file would read and write
// /etc/ngxsetup/borg-ssh-key on whatever machine runs `go test`.
func withTempKeyPaths(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	oldKey, oldPub := SSHKeyPath, SSHPubKeyPath
	SSHKeyPath = filepath.Join(dir, "borg-ssh-key")
	SSHPubKeyPath = SSHKeyPath + ".pub"
	t.Cleanup(func() { SSHKeyPath, SSHPubKeyPath = oldKey, oldPub })
}

func TestHasSSHKeyBeforeAndAfterGenerate(t *testing.T) {
	requireSSHKeygen(t)
	withTempKeyPaths(t)

	if HasSSHKey() {
		t.Fatal("HasSSHKey reported true before any key was ever generated")
	}
	if _, _, err := EnsureSSHKey(context.Background(), system.Runner{}, ""); err != nil {
		t.Fatalf("EnsureSSHKey: %v", err)
	}
	if !HasSSHKey() {
		t.Fatal("HasSSHKey reported false immediately after EnsureSSHKey generated one")
	}
}

func TestPublicKeyEmptyBeforeAnyKeyExists(t *testing.T) {
	withTempKeyPaths(t)
	pub, err := PublicKey()
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	if pub != "" {
		t.Errorf("PublicKey = %q, want empty string when no key has been generated", pub)
	}
}

func TestEnsureSSHKeyGeneratesFreshKey(t *testing.T) {
	requireSSHKeygen(t)
	withTempKeyPaths(t)

	pub, generated, err := EnsureSSHKey(context.Background(), system.Runner{}, "")
	if err != nil {
		t.Fatalf("EnsureSSHKey: %v", err)
	}
	if !generated {
		t.Error("generated = false on the very first call, want true")
	}
	if !strings.HasPrefix(pub, "ssh-ed25519 ") {
		t.Errorf("public key = %q, want an ssh-ed25519 key", pub)
	}
	info, err := os.Stat(SSHKeyPath)
	if err != nil {
		t.Fatalf("stat private key: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("private key mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestEnsureSSHKeyIsIdempotentWithoutImport(t *testing.T) {
	requireSSHKeygen(t)
	withTempKeyPaths(t)
	ctx := context.Background()

	firstPub, firstGenerated, err := EnsureSSHKey(ctx, system.Runner{}, "")
	if err != nil {
		t.Fatalf("first EnsureSSHKey: %v", err)
	}
	if !firstGenerated {
		t.Fatal("first call should have generated a key")
	}

	secondPub, secondGenerated, err := EnsureSSHKey(ctx, system.Runner{}, "")
	if err != nil {
		t.Fatalf("second EnsureSSHKey: %v", err)
	}
	if secondGenerated {
		t.Error("second call reported generated = true; a pre-existing key must never be silently replaced")
	}
	if secondPub != firstPub {
		t.Errorf("second call returned a different public key (%q) than the first (%q)", secondPub, firstPub)
	}
}

func TestEnsureSSHKeyImportsSuppliedKey(t *testing.T) {
	requireSSHKeygen(t)
	withTempKeyPaths(t)

	// Generate a keypair elsewhere, standing in for a key an operator
	// pastes in from outside this tool entirely.
	srcDir := t.TempDir()
	srcKey := filepath.Join(srcDir, "external-key")
	if err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-C", "test", "-f", srcKey).Run(); err != nil {
		t.Fatalf("generating a source key to import: %v", err)
	}
	privateKey, err := os.ReadFile(srcKey)
	if err != nil {
		t.Fatal(err)
	}
	wantPub, err := exec.Command("ssh-keygen", "-y", "-f", srcKey).Output()
	if err != nil {
		t.Fatalf("deriving expected public key: %v", err)
	}

	pub, generated, err := EnsureSSHKey(context.Background(), system.Runner{}, string(privateKey))
	if err != nil {
		t.Fatalf("EnsureSSHKey (import): %v", err)
	}
	if generated {
		t.Error("generated = true for an imported key, want false")
	}
	if pub != strings.TrimSpace(string(wantPub)) {
		t.Errorf("imported public key = %q, want %q", pub, strings.TrimSpace(string(wantPub)))
	}

	// PublicKey() must agree with what EnsureSSHKey just returned.
	stored, err := PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	if stored != pub {
		t.Errorf("PublicKey() = %q after import, want %q", stored, pub)
	}
}

func TestEnsureSSHKeyRejectsGarbageImport(t *testing.T) {
	requireSSHKeygen(t)
	withTempKeyPaths(t)

	_, _, err := EnsureSSHKey(context.Background(), system.Runner{}, "this is not a key at all")
	if err == nil {
		t.Fatal("expected an error importing garbage input, got nil")
	}
	if HasSSHKey() {
		t.Error("a failed import must not leave a key file behind")
	}
}

func TestEnsureSSHKeyRejectsPublicKeyPastedAsPrivate(t *testing.T) {
	requireSSHKeygen(t)
	withTempKeyPaths(t)

	_, _, err := EnsureSSHKey(context.Background(), system.Runner{}, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBogus test@example.com\n")
	if err == nil {
		t.Fatal("expected an error when a public key is pasted where a private key belongs")
	}
}

func TestSuggestRemoteKeyCommand(t *testing.T) {
	cases := []struct {
		repo string
		want string
	}{
		{
			repo: "ssh://backupdemo@172.16.1.107:2222/var/lib/ngxborg/repos/backupdemo/backupdemo",
			want: "ngxborg user key add --tenant backupdemo backupdemo 'ssh-ed25519 AAAA... test'",
		},
		{
			repo: "/mnt/backup/ngxsetup", // local path, no ssh:// scheme
			want: "",
		},
		{
			repo: "not a url at all",
			want: "",
		},
	}
	for _, tc := range cases {
		got := SuggestRemoteKeyCommand(tc.repo, "ssh-ed25519 AAAA... test")
		if got != tc.want {
			t.Errorf("SuggestRemoteKeyCommand(%q) = %q, want %q", tc.repo, got, tc.want)
		}
	}
}
