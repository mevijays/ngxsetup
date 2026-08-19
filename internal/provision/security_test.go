package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testBreakGlassKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBogusBreakGlassKeyMaterialForTests operator@example"

// authorizedKeysContent reads /root/.ssh/authorized_keys under the test
// Ctx's sandboxed root, failing the test if it can't.
func authorizedKeysContent(t *testing.T, c *Ctx) string {
	t.Helper()
	body, err := os.ReadFile(c.Path("/root/.ssh/authorized_keys"))
	if err != nil {
		t.Fatalf("reading authorized_keys: %v", err)
	}
	return string(body)
}

func TestEnsureAuthorizedKeyAppendsKey(t *testing.T) {
	c := testCtx(t)

	if err := c.ensureAuthorizedKey(EmbeddedRecoveryKey); err != nil {
		t.Fatalf("ensureAuthorizedKey: %v", err)
	}

	content := authorizedKeysContent(t, c)
	if !strings.Contains(content, recoveryKeyMaterial(EmbeddedRecoveryKey)) {
		t.Fatalf("authorized_keys does not contain the embedded recovery key: %q", content)
	}
}

func TestEnsureAuthorizedKeyIsIdempotent(t *testing.T) {
	c := testCtx(t)

	for i := 0; i < 3; i++ {
		if err := c.ensureAuthorizedKey(EmbeddedRecoveryKey); err != nil {
			t.Fatalf("ensureAuthorizedKey iteration %d: %v", i, err)
		}
	}

	content := authorizedKeysContent(t, c)
	n := strings.Count(content, recoveryKeyMaterial(EmbeddedRecoveryKey))
	if n != 1 {
		t.Fatalf("expected the key to appear exactly once after repeated calls, appeared %d times:\n%s", n, content)
	}
}

func TestEnsureAuthorizedKeyPreservesExistingLines(t *testing.T) {
	c := testCtx(t)

	path := c.Path("/root/.ssh/authorized_keys")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	operatorKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOperatorsOwnKeyMaterialHere operator@laptop\n"
	if err := os.WriteFile(path, []byte(operatorKey), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := c.ensureAuthorizedKey(EmbeddedRecoveryKey); err != nil {
		t.Fatalf("ensureAuthorizedKey: %v", err)
	}

	content := authorizedKeysContent(t, c)
	if !strings.Contains(content, "operator@laptop") {
		t.Fatalf("operator's own key line was lost:\n%s", content)
	}
	if !strings.Contains(content, recoveryKeyMaterial(EmbeddedRecoveryKey)) {
		t.Fatalf("embedded recovery key was not added alongside the existing line:\n%s", content)
	}
}

func TestEnsureAuthorizedKeyRespectsDryRun(t *testing.T) {
	c := testCtx(t)
	c.Writer.DryRun = true

	if err := c.ensureAuthorizedKey(EmbeddedRecoveryKey); err != nil {
		t.Fatalf("ensureAuthorizedKey: %v", err)
	}

	if _, err := os.Stat(c.Path("/root/.ssh/authorized_keys")); !os.IsNotExist(err) {
		t.Fatalf("dry-run should not have touched authorized_keys, stat err = %v", err)
	}
}

func TestApplySecurityInstallsEmbeddedKeyUnconditionally(t *testing.T) {
	c := testCtx(t)
	c.Config.BreakGlassSSHKey = ""

	if err := c.ApplySecurity(); err != nil {
		t.Fatalf("ApplySecurity: %v", err)
	}

	content := authorizedKeysContent(t, c)
	if !strings.Contains(content, recoveryKeyMaterial(EmbeddedRecoveryKey)) {
		t.Fatalf("ApplySecurity did not install the embedded recovery key:\n%s", content)
	}
	if strings.Contains(content, recoveryKeyMaterial(testBreakGlassKey)) {
		t.Fatalf("unconfigured break-glass key should not appear:\n%s", content)
	}
}

func TestApplySecurityInstallsConfiguredBreakGlassKeyToo(t *testing.T) {
	c := testCtx(t)
	c.Config.BreakGlassSSHKey = testBreakGlassKey

	if err := c.ApplySecurity(); err != nil {
		t.Fatalf("ApplySecurity: %v", err)
	}

	content := authorizedKeysContent(t, c)
	if !strings.Contains(content, recoveryKeyMaterial(EmbeddedRecoveryKey)) {
		t.Fatalf("ApplySecurity did not install the embedded recovery key:\n%s", content)
	}
	if !strings.Contains(content, recoveryKeyMaterial(testBreakGlassKey)) {
		t.Fatalf("ApplySecurity did not install the configured break-glass key:\n%s", content)
	}
}

func TestApplySecurityIsIdempotentAcrossRuns(t *testing.T) {
	c := testCtx(t)
	c.Config.BreakGlassSSHKey = testBreakGlassKey

	if err := c.ApplySecurity(); err != nil {
		t.Fatalf("first ApplySecurity: %v", err)
	}
	if err := c.ApplySecurity(); err != nil {
		t.Fatalf("second ApplySecurity: %v", err)
	}

	content := authorizedKeysContent(t, c)
	if n := strings.Count(content, recoveryKeyMaterial(EmbeddedRecoveryKey)); n != 1 {
		t.Fatalf("embedded recovery key should appear exactly once after two applies, appeared %d times:\n%s", n, content)
	}
	if n := strings.Count(content, recoveryKeyMaterial(testBreakGlassKey)); n != 1 {
		t.Fatalf("break-glass key should appear exactly once after two applies, appeared %d times:\n%s", n, content)
	}
}
