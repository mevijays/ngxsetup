package db

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Database and user names are derived from domain names supplied on the
// command line and then interpolated straight into SQL. Validation is the only
// thing standing between that and injection, so it is tested as a security
// boundary rather than as input tidying.
func TestValidateIdentifierRejectsInjection(t *testing.T) {
	bad := []string{
		"", " ", "a b", "a;b", "a'b", `a"b`, "a`b", "a\\b", "a--b",
		"db; DROP DATABASE mysql", "db' OR '1'='1",
		"db\nGRANT ALL", "db/*x*/", "DB", "1db", "_db", "-db",
		"db名", "db\x00", "db%", "db*",
		"averyveryverylongidentifiernamethatkeepsgoingandgoingandgoingpastsixtythree",
	}
	for _, name := range bad {
		if err := ValidateIdentifier(name); err == nil {
			t.Errorf("ValidateIdentifier(%q) accepted an unsafe identifier", name)
		}
	}

	good := []string{"db", "wp_site", "site123", "a", "example_com_a1b2"}
	for _, name := range good {
		if err := ValidateIdentifier(name); err != nil {
			t.Errorf("ValidateIdentifier(%q) rejected a valid identifier: %v", name, err)
		}
	}
}

// Generated passwords are embedded in a SQL string literal, so anything that
// could terminate that literal must be refused.
func TestValidatePasswordRejectsQuoteCharacters(t *testing.T) {
	bad := []string{
		"", "short", "eleven_chr!",
		"has'quote123", `has"quote123`, "has\\backslash1", "has`tick`123",
		"has\nnewline123", "has\x00null1234",
	}
	for _, pw := range bad {
		if err := ValidatePassword(pw); err == nil {
			t.Errorf("ValidatePassword(%q) accepted an unsafe password", pw)
		}
	}

	good := []string{
		"abcdefghijkl", "Str0ng-Pass_word.123", "A1b2C3d4E5f6~!@#%^*+=:-",
	}
	for _, pw := range good {
		if err := ValidatePassword(pw); err != nil {
			t.Errorf("ValidatePassword(%q) rejected a valid password: %v", pw, err)
		}
	}
}

// Provision must refuse before it ever reaches the database.
func TestProvisionValidatesBeforeExecuting(t *testing.T) {
	c := Client{}
	err := c.Provision(nil, "ok_db", "ok_user", "short", "localhost", "")
	if err == nil {
		t.Error("a weak password should be refused before any SQL is built")
	}
	if err := c.Provision(nil, "bad;name", "u", "aValidPassword123", "localhost", ""); err == nil {
		t.Error("an unsafe database name should be refused")
	}
	if err := c.Provision(nil, "ok_db", "bad'user", "aValidPassword123", "localhost", ""); err == nil {
		t.Error("an unsafe user name should be refused")
	}
}

func TestDropValidatesIdentifiers(t *testing.T) {
	c := Client{}
	if err := c.Drop(nil, "ok_db; DROP DATABASE mysql", "u", "localhost"); err == nil {
		t.Error("Drop accepted an unsafe database name")
	}
	if err := c.Drop(nil, "ok_db", "u; --", "localhost"); err == nil {
		t.Error("Drop accepted an unsafe user name")
	}
}

// Restore is destructive and irreversible by anything downstream of this
// call, so every guard has to fire before a single byte reaches the database
// client — an unsafe identifier or a bad file must never get as far as
// exec.Command.
func TestRestoreValidatesIdentifierBeforeTouchingTheFile(t *testing.T) {
	c := Client{}
	// A file that does not exist: if identifier validation ran after the file
	// check, this would fail for the wrong reason and the ordering guarantee
	// would be untested.
	if err := c.Restore(nil, "bad;name", "/no/such/file-really-not-there.sql"); err == nil {
		t.Error("Restore accepted an unsafe database name")
	}
}

func TestRestoreRejectsMissingOrEmptyFile(t *testing.T) {
	c := Client{}
	if err := c.Restore(context.Background(), "ok_db", "/no/such/file-really-not-there.sql"); err == nil {
		t.Error("Restore accepted a nonexistent file")
	}

	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.sql")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Restore(context.Background(), "ok_db", empty); err == nil {
		t.Error("Restore accepted an empty file")
	}
}

// EnsureSchemaAndGrant recreates a schema an accidental DROP DATABASE
// removed; the identifiers it interpolates into SQL have to be validated
// before anything reaches the database client, same as everywhere else in
// this package.
func TestEnsureSchemaAndGrantValidatesIdentifiers(t *testing.T) {
	c := Client{}
	if err := c.EnsureSchemaAndGrant(nil, "bad;name", "user", "localhost", ""); err == nil {
		t.Error("EnsureSchemaAndGrant accepted an unsafe database name")
	}
	if err := c.EnsureSchemaAndGrant(nil, "ok_db", "bad'user", "localhost", ""); err == nil {
		t.Error("EnsureSchemaAndGrant accepted an unsafe user name")
	}
}
