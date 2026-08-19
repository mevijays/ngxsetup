// Package db provisions databases and users for WordPress sites.
//
// Two things matter here beyond getting the SQL right.
//
// Credentials never appear in a command line. On Linux every process's argv is
// readable by every user through /proc, so `mysql -uroot -phunter2` publishes
// the root password to anyone with a shell — including the site accounts this
// tool creates. SQL is piped through stdin and passwords are passed through a
// mode-0600 defaults file instead.
//
// Identifiers are validated rather than escaped. Database and user names are
// derived from domain names, and the only safe way to interpolate an
// identifier into SQL is to guarantee up front that it contains nothing that
// needs escaping.
package db

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"ngxsetup/internal/facts"
	"ngxsetup/internal/logx"
	"ngxsetup/internal/system"
)

// Client talks to the local database server.
type Client struct {
	Runner system.Runner
	Flavor facts.DBFlavor
	// DefaultsFile, when set, supplies credentials. Empty means connecting as
	// root over the unix socket, which is how Debian and Ubuntu configure both
	// MariaDB and MySQL by default.
	DefaultsFile string
}

// identifier accepts only characters that need no quoting anywhere in SQL.
var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// safePassword rejects anything that would need escaping inside the SQL string
// literal it is about to be placed in. Generated passwords always satisfy this;
// the check exists to catch an operator-supplied value.
var safePassword = regexp.MustCompile(`^[A-Za-z0-9._~!@#%^*+=:-]{12,128}$`)

// ValidateIdentifier reports whether a name is safe to use unquoted.
func ValidateIdentifier(name string) error {
	if !identifier.MatchString(name) {
		return fmt.Errorf("invalid database identifier %q: must start with a letter and contain only lowercase letters, digits and underscores (max 63 characters)", name)
	}
	return nil
}

// ValidatePassword reports whether a password can be embedded safely.
func ValidatePassword(pw string) error {
	if !safePassword.MatchString(pw) {
		return fmt.Errorf("password contains characters that cannot be safely embedded in SQL, or is shorter than 12 characters")
	}
	return nil
}

func (c Client) binary() string {
	if c.Runner.Look("mariadb") {
		return "mariadb"
	}
	return "mysql"
}

func (c Client) baseArgs() []string {
	var args []string
	if c.DefaultsFile != "" {
		// Must be the first argument; the client rejects it anywhere else.
		args = append(args, "--defaults-file="+c.DefaultsFile)
	}
	args = append(args, "--batch", "--skip-column-names")
	return args
}

// Exec runs statements that produce no result set.
func (c Client) Exec(ctx context.Context, sql string) error {
	_, err := c.Runner.RunStdin(ctx, sql, c.binary(), c.baseArgs()...)
	return err
}

// Query runs a statement and returns raw tab-separated output.
func (c Client) Query(ctx context.Context, sql string) (string, error) {
	out, err := c.Runner.RunStdin(ctx, sql, c.binary(), c.baseArgs()...)
	return strings.TrimSpace(out), err
}

// Ping reports whether the server is reachable and accepting our credentials.
func (c Client) Ping(ctx context.Context) error {
	if _, err := c.Query(ctx, "SELECT 1;"); err != nil {
		return fmt.Errorf("cannot connect to the database server: %w", err)
	}
	return nil
}

// ServerVersion reports the running server version.
func (c Client) ServerVersion(ctx context.Context) (string, error) {
	return c.Query(ctx, "SELECT VERSION();")
}

// DatabaseExists reports whether a schema is present.
func (c Client) DatabaseExists(ctx context.Context, name string) (bool, error) {
	if err := ValidateIdentifier(name); err != nil {
		return false, err
	}
	out, err := c.Query(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='%s';", name))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "0", nil
}

// UserExists reports whether an account is present for the given host.
func (c Client) UserExists(ctx context.Context, name, host string) (bool, error) {
	if err := ValidateIdentifier(name); err != nil {
		return false, err
	}
	out, err := c.Query(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM mysql.user WHERE User='%s' AND Host='%s';", name, host))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "0", nil
}

// Provision creates the database, the account and its grant.
//
// It is idempotent: an existing database is left alone and an existing account
// has its password reset, so re-running site creation after a partial failure
// converges rather than erroring.
func (c Client) Provision(ctx context.Context, dbName, user, password, host, collation string) error {
	if err := ValidateIdentifier(dbName); err != nil {
		return err
	}
	if err := ValidateIdentifier(user); err != nil {
		return err
	}
	if err := ValidatePassword(password); err != nil {
		return err
	}
	if host == "" {
		host = "localhost"
	}
	charset := "utf8mb4"
	if collation == "" {
		collation = "utf8mb4_general_ci"
	}

	// One statement batch, so a failure part-way leaves nothing half-created.
	// CREATE USER is used rather than GRANT-with-IDENTIFIED-BY, which MySQL 8
	// removed.
	sql := fmt.Sprintf(`
CREATE DATABASE IF NOT EXISTS %[1]s CHARACTER SET %[4]s COLLATE %[5]s;
CREATE USER IF NOT EXISTS '%[2]s'@'%[3]s' IDENTIFIED BY '%[6]s';
ALTER USER '%[2]s'@'%[3]s' IDENTIFIED BY '%[6]s';
GRANT ALL PRIVILEGES ON %[1]s.* TO '%[2]s'@'%[3]s';
FLUSH PRIVILEGES;
`, dbName, user, host, charset, collation, password)

	if err := c.Exec(ctx, sql); err != nil {
		return fmt.Errorf("provisioning database %s: %w", dbName, err)
	}
	logx.Change("provisioned database %s and user %s@%s", dbName, user, host)
	return nil
}

// EnsureSchemaAndGrant recreates a database schema that has gone missing —
// most commonly an accidental `DROP DATABASE`, as opposed to a dropped
// table or deleted rows, which never leaves the schema itself absent — and
// re-issues its grant to an account that already exists. Unlike Provision,
// it never touches the account's password: the common case after a
// database-only accident is that the user account survived untouched (both
// MySQL and MariaDB leave granted privileges in mysql.db in place even
// after the schema they were granted on disappears), so all that is
// missing is the schema and possibly its grant row. Idempotent: both
// CREATE DATABASE IF NOT EXISTS and GRANT are safe to repeat.
func (c Client) EnsureSchemaAndGrant(ctx context.Context, dbName, user, host, collation string) error {
	if err := ValidateIdentifier(dbName); err != nil {
		return err
	}
	if err := ValidateIdentifier(user); err != nil {
		return err
	}
	if host == "" {
		host = "localhost"
	}
	charset := "utf8mb4"
	if collation == "" {
		collation = "utf8mb4_general_ci"
	}

	sql := fmt.Sprintf(`
CREATE DATABASE IF NOT EXISTS %[1]s CHARACTER SET %[3]s COLLATE %[4]s;
GRANT ALL PRIVILEGES ON %[1]s.* TO '%[2]s'@'%[5]s';
FLUSH PRIVILEGES;
`, dbName, user, charset, collation, host)

	if err := c.Exec(ctx, sql); err != nil {
		return fmt.Errorf("recreating database %s: %w", dbName, err)
	}
	logx.Change("recreated missing database %s and re-granted %s@%s", dbName, user, host)
	return nil
}

// Drop removes a site's database and account.
func (c Client) Drop(ctx context.Context, dbName, user, host string) error {
	if err := ValidateIdentifier(dbName); err != nil {
		return err
	}
	if err := ValidateIdentifier(user); err != nil {
		return err
	}
	if host == "" {
		host = "localhost"
	}
	sql := fmt.Sprintf(`
DROP DATABASE IF EXISTS %s;
DROP USER IF EXISTS '%s'@'%s';
FLUSH PRIVILEGES;
`, dbName, user, host)
	if err := c.Exec(ctx, sql); err != nil {
		return fmt.Errorf("dropping database %s: %w", dbName, err)
	}
	logx.Change("dropped database %s and user %s@%s", dbName, user, host)
	return nil
}

// Dump writes a compressed logical backup of one database.
func (c Client) Dump(ctx context.Context, dbName, path string) error {
	if err := ValidateIdentifier(dbName); err != nil {
		return err
	}
	bin := "mysqldump"
	if c.Runner.Look("mariadb-dump") {
		bin = "mariadb-dump"
	}
	args := []string{}
	if c.DefaultsFile != "" {
		args = append(args, "--defaults-file="+c.DefaultsFile)
	}
	args = append(args,
		"--single-transaction", // consistent snapshot without locking writers out
		"--quick",              // stream rows instead of buffering the table
		"--default-character-set=utf8mb4",
		"--no-tablespaces", // avoids needing PROCESS privilege
		dbName,
	)
	out, err := c.Runner.Output(ctx, bin, args...)
	if err != nil {
		return fmt.Errorf("dumping %s: %w", dbName, err)
	}
	return os.WriteFile(path, []byte(out), 0o600)
}

// Restore loads a logical dump into an existing database, streaming the file
// straight to the client's stdin rather than reading it into memory first —
// the same reasoning as Dump writing straight to disk, in reverse.
//
// This does not touch the database or account itself: dbName must already
// exist (site creation is what provisions it), and Restore only replaces its
// contents. A dump produced by Dump has no CREATE DATABASE or USE statement —
// it was taken as a single-database dump — so every statement in it runs in
// whatever schema dbName names here, which is what makes restoring into a
// differently-named database (a copy, a staging site) work correctly instead
// of silently landing in the wrong place.
func (c Client) Restore(ctx context.Context, dbName, path string) error {
	if err := ValidateIdentifier(dbName); err != nil {
		return err
	}
	if info, err := os.Stat(path); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	} else if info.Size() == 0 {
		return fmt.Errorf("%s is empty", path)
	}

	args := c.baseArgs()
	args = append(args, dbName)
	if _, err := c.Runner.RunStdinFile(ctx, path, c.binary(), args...); err != nil {
		return fmt.Errorf("restoring %s from %s: %w", dbName, path, err)
	}
	logx.Change("restored %s from %s", dbName, path)
	return nil
}

// Harden applies the checks that mysql_secure_installation performs, without
// its interactive prompts.
func (c Client) Harden(ctx context.Context) error {
	// Anonymous accounts allow any local user to connect with no credential.
	// The test database is world-writable by design.
	sql := `
DELETE FROM mysql.user WHERE User='';
DROP DATABASE IF EXISTS test;
DELETE FROM mysql.db WHERE Db='test' OR Db='test\\_%';
FLUSH PRIVILEGES;
`
	if err := c.Exec(ctx, sql); err != nil {
		return fmt.Errorf("hardening database: %w", err)
	}
	logx.Change("removed anonymous database accounts and the test database")
	return nil
}

// GlobalStatus returns the server's live counters (SHOW GLOBAL STATUS) as a
// name -> value map, for the web UI's live database performance graphs
// (queries/sec, connections, buffer pool hit ratio...). Every value comes
// back as its literal string form; callers that need a number parse it
// themselves, since a handful of status variables (Ssl_version, and others
// only Percona/MariaDB-specific builds add) are never numeric and this
// method has no business knowing in advance which is which.
func (c Client) GlobalStatus(ctx context.Context) (map[string]string, error) {
	out, err := c.Query(ctx, "SHOW GLOBAL STATUS;")
	if err != nil {
		return nil, fmt.Errorf("reading server status: %w", err)
	}
	status := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		name, value, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		status[name] = value
	}
	return status, nil
}

// RemoteRootAccounts lists root accounts reachable from outside this machine.
// A finding here is reported rather than silently corrected: an operator may
// be relying on remote root for a reason this tool cannot see.
func (c Client) RemoteRootAccounts(ctx context.Context) ([]string, error) {
	out, err := c.Query(ctx, "SELECT CONCAT(User,'@',Host) FROM mysql.user WHERE User='root' AND Host NOT IN ('localhost','127.0.0.1','::1');")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// WriteDefaultsFile writes a mode-0600 credentials file for the client to use,
// keeping the password out of every process listing on the machine.
func WriteDefaultsFile(path, user, password string) error {
	content := fmt.Sprintf("[client]\nuser=%s\npassword=%s\n", user, password)
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "/"
}
