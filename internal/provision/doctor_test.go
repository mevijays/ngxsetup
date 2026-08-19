package provision

import (
	"os"
	"path/filepath"
	"testing"
)

// findCheck returns the first Check with the given label, or nil.
func findCheck(checks []Check, label string) *Check {
	for i := range checks {
		if checks[i].Name == label {
			return &checks[i]
		}
	}
	return nil
}

func TestCheckSecurityRecoveryKeyPresent(t *testing.T) {
	c := testCtx(t)
	path := c.Path("/root/.ssh/authorized_keys")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(EmbeddedRecoveryKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	checks := c.checkSecurity()
	chk := findCheck(checks, "ssh recovery key")
	if chk == nil {
		t.Fatalf("expected an ssh recovery key check, got: %+v", checks)
	}
	if chk.Status != StatusOK {
		t.Fatalf("expected StatusOK with the built-in key present, got %v: %s", chk.Status, chk.Detail)
	}
}

func TestCheckSecurityRecoveryKeyMissing(t *testing.T) {
	c := testCtx(t)
	path := c.Path("/root/.ssh/authorized_keys")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("ssh-ed25519 AAAASomeoneElsesKey someone@else\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	checks := c.checkSecurity()
	chk := findCheck(checks, "ssh recovery key")
	if chk == nil {
		t.Fatalf("expected an ssh recovery key check, got: %+v", checks)
	}
	if chk.Status != StatusWarn {
		t.Fatalf("expected StatusWarn when the built-in key is absent, got %v: %s", chk.Status, chk.Detail)
	}
}

func TestCheckSecurityBreakGlassKeyConfiguredAndPresent(t *testing.T) {
	c := testCtx(t)
	c.Config.BreakGlassSSHKey = testBreakGlassKey
	path := c.Path("/root/.ssh/authorized_keys")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := EmbeddedRecoveryKey + "\n" + testBreakGlassKey + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	checks := c.checkSecurity()
	chk := findCheck(checks, "ssh break-glass key")
	if chk == nil {
		t.Fatalf("expected an ssh break-glass key check when configured, got: %+v", checks)
	}
	if chk.Status != StatusOK {
		t.Fatalf("expected StatusOK with the configured key present, got %v: %s", chk.Status, chk.Detail)
	}
}

func TestCheckSecurityBreakGlassKeyConfiguredButMissing(t *testing.T) {
	c := testCtx(t)
	c.Config.BreakGlassSSHKey = testBreakGlassKey
	path := c.Path("/root/.ssh/authorized_keys")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(EmbeddedRecoveryKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	checks := c.checkSecurity()
	chk := findCheck(checks, "ssh break-glass key")
	if chk == nil {
		t.Fatalf("expected an ssh break-glass key check when configured, got: %+v", checks)
	}
	if chk.Status != StatusWarn {
		t.Fatalf("expected StatusWarn when the configured key is absent, got %v: %s", chk.Status, chk.Detail)
	}
}

func TestCheckSecurityNoBreakGlassCheckWhenUnconfigured(t *testing.T) {
	c := testCtx(t)
	c.Config.BreakGlassSSHKey = ""
	path := c.Path("/root/.ssh/authorized_keys")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(EmbeddedRecoveryKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	checks := c.checkSecurity()
	if chk := findCheck(checks, "ssh break-glass key"); chk != nil {
		t.Fatalf("did not expect an ssh break-glass key check when unconfigured, got: %+v", chk)
	}
}
