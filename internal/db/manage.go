// Database management (browsing existing databases/users/tables/rows) is a
// different trust boundary from the rest of this package: everywhere else,
// identifiers come from domain names ngxsetup itself derives and validates
// against ValidateIdentifier's strict "this tool made it" regex. Here, an
// identifier can be *any* existing database, table, column, or user name —
// including ones created outside ngxsetup entirely, in whatever case or
// character set MySQL/MariaDB itself allows. ValidateIdentifier would
// reject perfectly real names (mixed case, a leading digit), so this file
// uses backtick-quoting instead — the general, correct way to make an
// arbitrary MySQL identifier safe to interpolate — and always confirms an
// identifier actually exists (via information_schema) before using it,
// rather than trusting client input at all.
package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// quoteIdent backtick-quotes an arbitrary identifier for interpolation into
// SQL, doubling any literal backtick — the standard MySQL/MariaDB escaping.
// A backtick-quoted identifier cannot be broken out of (there is no escape
// sequence that ends quoting early the way there is inside a string
// literal), which is what makes this safe for a name this package did not
// generate itself.
func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// quoteLiteral quotes an arbitrary value as a single-quoted SQL string
// literal, escaping backslash and single quote — the two characters that
// matter under MySQL's default (non-NO_BACKSLASH_ESCAPES) SQL mode.
func quoteLiteral(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}

// systemSchemas are never shown in the database browser — they belong to
// the server itself, not to any site or operator-created database, and
// nothing good comes from an operator casually editing rows in them by
// hand from a generic table grid.
var systemSchemas = map[string]bool{
	"information_schema": true,
	"performance_schema": true,
	"mysql":              true,
	"sys":                true,
}

// QueryRaw runs a statement and returns its single-field, single-row output
// completely unescaped — used only for the JSON_ARRAYAGG(...) queries below,
// where the point is to get back exactly one already-encoded JSON blob with
// no reinterpretation. Query (used everywhere else in this package) asks
// the client to escape control characters for safe tab-separated parsing of
// potentially many fields; that escaping would corrupt a JSON blob it
// doesn't know is JSON, so this bypasses it with --raw instead.
func (c Client) QueryRaw(ctx context.Context, sql string) (string, error) {
	args := append(append([]string{}, c.baseArgs()...), "--raw")
	out, err := c.Runner.RunStdin(ctx, sql, c.binary(), args...)
	return strings.TrimSpace(out), err
}

// DatabaseInfo summarises one schema for the database browser's overview.
type DatabaseInfo struct {
	Name      string
	SizeMB    int64
	Tables    int
	Collation string
}

// ListDatabases lists every non-system schema with its size and table
// count, one round trip.
func (c Client) ListDatabases(ctx context.Context) ([]DatabaseInfo, error) {
	const sql = `SELECT s.SCHEMA_NAME, s.DEFAULT_COLLATION_NAME,
	COALESCE(SUM(t.data_length + t.index_length), 0), COUNT(t.TABLE_NAME)
FROM information_schema.SCHEMATA s
LEFT JOIN information_schema.TABLES t ON t.TABLE_SCHEMA = s.SCHEMA_NAME
GROUP BY s.SCHEMA_NAME, s.DEFAULT_COLLATION_NAME
ORDER BY s.SCHEMA_NAME;`
	out, err := c.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	var dbs []DatabaseInfo
	for _, line := range strings.Split(out, "\n") {
		// TrimRight("\r"), not TrimSpace: TrimSpace also strips a
		// trailing tab, silently merging the row's last field into
		// nothing when that field is legitimately empty (COLUMN_KEY,
		// most commonly) — found live: every non-indexed column of a
		// real WordPress table vanished from the column list because of
		// exactly this.
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 4 || systemSchemas[f[0]] {
			continue
		}
		sizeBytes, _ := strconv.ParseInt(f[2], 10, 64)
		tables, _ := strconv.Atoi(f[3])
		dbs = append(dbs, DatabaseInfo{Name: f[0], Collation: f[1], SizeMB: sizeBytes / (1 << 20), Tables: tables})
	}
	return dbs, nil
}

// UserInfo summarises one MySQL/MariaDB account for the database browser.
type UserInfo struct {
	User string
	Host string
	// Grants is the raw output of SHOW GRANTS, one statement per entry —
	// deliberately shown as the server's own authoritative text rather than
	// this tool's own summary of it, so an operator sees exactly what the
	// server will actually enforce.
	Grants []string
}

// ListUsers lists every account (excluding the ones a distro/db install
// itself creates, which this tool did not create and has no business
// letting an operator delete by accident from a generic grid).
var builtinAccounts = map[string]bool{
	"root": true, "mysql.sys": true, "mysql.session": true,
	"mysql.infoschema": true, "mariadb.sys": true, "debian-sys-maint": true,
}

func (c Client) ListUsers(ctx context.Context) ([]UserInfo, error) {
	out, err := c.Query(ctx, "SELECT User, Host FROM mysql.user ORDER BY User, Host;")
	if err != nil {
		return nil, err
	}
	var users []UserInfo
	for _, line := range strings.Split(out, "\n") {
		// TrimRight("\r"), not TrimSpace: TrimSpace also strips a
		// trailing tab, silently merging the row's last field into
		// nothing when that field is legitimately empty (COLUMN_KEY,
		// most commonly) — found live: every non-indexed column of a
		// real WordPress table vanished from the column list because of
		// exactly this.
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 2 || builtinAccounts[f[0]] {
			continue
		}
		users = append(users, UserInfo{User: f[0], Host: f[1]})
	}
	return users, nil
}

// UserGrants returns SHOW GRANTS's own output for one account, one
// statement per line.
func (c Client) UserGrants(ctx context.Context, user, host string) ([]string, error) {
	sql := fmt.Sprintf("SHOW GRANTS FOR %s@%s;", quoteLiteral(user), quoteLiteral(host))
	out, err := c.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	var grants []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			grants = append(grants, line)
		}
	}
	return grants, nil
}

// CreateManagedUser creates a new account with a password, granting
// nothing — a separate GrantPrivileges call decides what it can reach.
// Password strength is the caller's responsibility (ValidatePassword),
// the same as every other account this package creates.
func (c Client) CreateManagedUser(ctx context.Context, user, host, password string) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}
	sql := fmt.Sprintf("CREATE USER IF NOT EXISTS %s@%s IDENTIFIED BY %s;",
		quoteLiteral(user), quoteLiteral(host), quoteLiteral(password))
	return c.Exec(ctx, sql)
}

// DropManagedUser removes an account. Refuses a built-in/system account
// even if a caller somehow names one directly — ListUsers already excludes
// them from what an operator can select in the first place, this is
// defense in depth against that being bypassed.
func (c Client) DropManagedUser(ctx context.Context, user, host string) error {
	if builtinAccounts[user] {
		return fmt.Errorf("%s is a system account and cannot be managed here", user)
	}
	sql := fmt.Sprintf("DROP USER IF EXISTS %s@%s;", quoteLiteral(user), quoteLiteral(host))
	return c.Exec(ctx, sql)
}

// managedPrivileges is the curated set of privileges the web UI's grant
// form can assign — ordinary data/schema privileges an application account
// legitimately needs, deliberately excluding server-wide administrative
// ones (SUPER, RELOAD, SHUTDOWN, FILE, PROCESS, REPLICATION...) that have
// no business being handed out through a per-database grant screen.
var managedPrivileges = map[string]bool{
	"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true,
	"CREATE": true, "DROP": true, "ALTER": true, "INDEX": true,
	"REFERENCES": true, "LOCK TABLES": true, "CREATE TEMPORARY TABLES": true,
}

// GrantPrivileges grants a set of privileges on one database to one
// account. privileges must be non-empty and every entry must be in
// managedPrivileges (or the single value "ALL", meaning every managed
// privilege) — anything else is rejected rather than silently dropped, so
// a typo doesn't grant something narrower than the caller thought without
// telling them.
func (c Client) GrantPrivileges(ctx context.Context, user, host, dbName string, privileges []string) error {
	if err := ValidateIdentifier(dbName); err != nil {
		return err
	}
	if len(privileges) == 0 {
		return fmt.Errorf("at least one privilege is required")
	}
	list := privileges
	if len(privileges) == 1 && strings.EqualFold(privileges[0], "ALL") {
		list = nil
		for p := range managedPrivileges {
			list = append(list, p)
		}
	}
	for _, p := range list {
		if !managedPrivileges[strings.ToUpper(p)] {
			return fmt.Errorf("privilege %q is not permitted here", p)
		}
	}
	sql := fmt.Sprintf("GRANT %s ON %s.* TO %s@%s;",
		strings.Join(list, ", "), quoteIdent(dbName), quoteLiteral(user), quoteLiteral(host))
	return c.Exec(ctx, sql)
}

// RevokeAllPrivileges removes every privilege an account has on one
// database, leaving the account (and its access to any other database)
// otherwise untouched.
func (c Client) RevokeAllPrivileges(ctx context.Context, user, host, dbName string) error {
	if err := ValidateIdentifier(dbName); err != nil {
		return err
	}
	sql := fmt.Sprintf("REVOKE ALL PRIVILEGES, GRANT OPTION ON %s.* FROM %s@%s;",
		quoteIdent(dbName), quoteLiteral(user), quoteLiteral(host))
	return c.Exec(ctx, sql)
}

// TableInfo summarises one table for the database browser's table list.
type TableInfo struct {
	Name       string
	Engine     string
	Rows       int64
	SizeMB     int64
	HasPrimary bool
}

// ListTables lists every table in one database, with an approximate row
// count (InnoDB's own estimate — information_schema.TABLES.TABLE_ROWS is
// not exact for InnoDB, but an exact COUNT(*) per table would mean a full
// scan of every table just to render a list; the row browser itself uses a
// real COUNT(*) scoped to one table instead, where that cost is paid once
// for a table an operator actually opened).
func (c Client) ListTables(ctx context.Context, dbName string) ([]TableInfo, error) {
	if err := ValidateIdentifier(dbName); err != nil {
		return nil, err
	}
	sql := fmt.Sprintf(`SELECT t.TABLE_NAME, COALESCE(t.ENGINE,''), COALESCE(t.TABLE_ROWS,0),
	COALESCE(t.data_length + t.index_length, 0),
	EXISTS(SELECT 1 FROM information_schema.KEY_COLUMN_USAGE k
	       WHERE k.TABLE_SCHEMA=%s AND k.TABLE_NAME=t.TABLE_NAME AND k.CONSTRAINT_NAME='PRIMARY')
FROM information_schema.TABLES t
WHERE t.TABLE_SCHEMA=%s AND t.TABLE_TYPE='BASE TABLE'
ORDER BY t.TABLE_NAME;`, quoteLiteral(dbName), quoteLiteral(dbName))
	out, err := c.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	var tables []TableInfo
	for _, line := range strings.Split(out, "\n") {
		// TrimRight("\r"), not TrimSpace: TrimSpace also strips a
		// trailing tab, silently merging the row's last field into
		// nothing when that field is legitimately empty (COLUMN_KEY,
		// most commonly) — found live: every non-indexed column of a
		// real WordPress table vanished from the column list because of
		// exactly this.
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 5 {
			continue
		}
		rows, _ := strconv.ParseInt(f[2], 10, 64)
		sizeBytes, _ := strconv.ParseInt(f[3], 10, 64)
		tables = append(tables, TableInfo{
			Name: f[0], Engine: f[1], Rows: rows, SizeMB: sizeBytes / (1 << 20),
			HasPrimary: f[4] == "1",
		})
	}
	return tables, nil
}

// ColumnInfo describes one column, enough to render and safely edit it.
type ColumnInfo struct {
	Name       string
	Type       string
	Nullable   bool
	PrimaryKey bool
}

// TableColumns describes every column of one table, in their real column
// order.
func (c Client) TableColumns(ctx context.Context, dbName, table string) ([]ColumnInfo, error) {
	if err := ValidateIdentifier(dbName); err != nil {
		return nil, err
	}
	sql := fmt.Sprintf(`SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, COLUMN_KEY
FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA=%s AND TABLE_NAME=%s
ORDER BY ORDINAL_POSITION;`, quoteLiteral(dbName), quoteLiteral(table))
	out, err := c.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	cols := parseColumns(out)
	if len(cols) == 0 {
		return nil, fmt.Errorf("table %s.%s not found (or has no columns)", dbName, table)
	}
	return cols, nil
}

// parseColumns turns the client's tab-separated COLUMN_NAME/COLUMN_TYPE/
// IS_NULLABLE/COLUMN_KEY output into ColumnInfo values. Kept separate from
// the query itself so the parsing can be tested against fixed sample
// output without a database — see TestParseColumnsKeepsEmptyTrailingField
// for the real bug this split exists to guard against.
func parseColumns(raw string) []ColumnInfo {
	var cols []ColumnInfo
	for _, line := range strings.Split(raw, "\n") {
		// TrimRight("\r"), not TrimSpace: TrimSpace also strips a
		// trailing tab, silently merging the row's last field into
		// nothing when that field is legitimately empty (COLUMN_KEY,
		// most commonly) — found live: every non-indexed column of a
		// real WordPress table vanished from the column list because of
		// exactly this.
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			continue
		}
		cols = append(cols, ColumnInfo{
			Name: f[0], Type: f[1], Nullable: f[2] == "YES", PrimaryKey: f[3] == "PRI",
		})
	}
	return cols
}

// TableRows returns one page of a table's rows (as column-name -> value,
// value already stringified — this is a browsing/editing grid, not a
// typed query API) together with the table's total row count.
//
// Rows are fetched via JSON_ARRAYAGG/JSON_OBJECT and decoded from a single
// JSON blob (QueryRaw, --raw) rather than this package's usual
// tab-separated parsing: real row content — a WordPress post body, in
// particular — routinely contains embedded tabs and newlines, which the
// server itself already knows how to encode correctly inside JSON, so
// there is no hand-rolled escaping to get subtly wrong here. NULL becomes
// Go nil, everything else becomes its JSON-native representation (numbers
// as json.Number so a large BIGINT round-trips exactly, not through
// float64).
func (c Client) TableRows(ctx context.Context, dbName, table string, cols []ColumnInfo, page, pageSize int) (rows []map[string]any, total int64, err error) {
	if err := ValidateIdentifier(dbName); err != nil {
		return nil, 0, err
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 500 {
		pageSize = 100
	}
	qualified := quoteIdent(dbName) + "." + quoteIdent(table)

	countOut, err := c.Query(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s;", qualified))
	if err != nil {
		return nil, 0, err
	}
	total, _ = strconv.ParseInt(strings.TrimSpace(countOut), 10, 64)

	// Deliberately unqualified column names here, not "t.col": this ORDER
	// BY sits inside the INNER (SELECT * FROM ... ORDER BY ... LIMIT ...)
	// subquery, where the "t" alias does not exist yet — that alias only
	// applies to the subquery's own *result*, in the outer FROM (...) t.
	// Found live: exactly this table-alias scoping mistake produced
	// "Unknown column 't.option_id' in 'order clause'" against a real
	// WordPress database's wp_options table the first time this ran.
	orderBy := ""
	if pk := primaryKeyColumns(cols); len(pk) > 0 {
		quoted := make([]string, len(pk))
		for i, p := range pk {
			quoted[i] = quoteIdent(p)
		}
		orderBy = "ORDER BY " + strings.Join(quoted, ", ")
	}

	pairs := make([]string, len(cols))
	for i, col := range cols {
		pairs[i] = quoteLiteral(col.Name) + ", t." + quoteIdent(col.Name)
	}
	sql := fmt.Sprintf(`SELECT COALESCE(JSON_ARRAYAGG(JSON_OBJECT(%s)), JSON_ARRAY())
FROM (SELECT * FROM %s %s LIMIT %d OFFSET %d) t;`,
		strings.Join(pairs, ", "), qualified, orderBy, pageSize, (page-1)*pageSize)

	raw, err := c.QueryRaw(ctx, sql)
	if err != nil {
		return nil, 0, err
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&rows); err != nil {
		return nil, 0, fmt.Errorf("decoding row data: %w", err)
	}
	return rows, total, nil
}

// UpdateRow updates one row, identified by its primary key, setting the
// given columns to new values. Refuses tables with no primary key — there
// is no other safe, generic way to name exactly one row — and refuses to
// touch any column that isn't a real column of this table (defense in
// depth: the web handler already only offers real columns to edit).
func (c Client) UpdateRow(ctx context.Context, dbName, table string, cols []ColumnInfo, pkValues map[string]string, changes map[string]string) error {
	if err := ValidateIdentifier(dbName); err != nil {
		return err
	}
	pk := primaryKeyColumns(cols)
	if len(pk) == 0 {
		return fmt.Errorf("table %s.%s has no primary key; editing rows is not possible without one", dbName, table)
	}
	known := make(map[string]bool, len(cols))
	for _, c := range cols {
		known[c.Name] = true
	}
	if len(changes) == 0 {
		return fmt.Errorf("no changes given")
	}
	var setParts []string
	for col, val := range changes {
		if !known[col] {
			return fmt.Errorf("%q is not a column of %s.%s", col, dbName, table)
		}
		setParts = append(setParts, quoteIdent(col)+" = "+quoteLiteral(val))
	}
	var whereParts []string
	for _, p := range pk {
		v, ok := pkValues[p]
		if !ok {
			return fmt.Errorf("missing primary key value for %q", p)
		}
		whereParts = append(whereParts, quoteIdent(p)+" = "+quoteLiteral(v))
	}
	sql := fmt.Sprintf("UPDATE %s.%s SET %s WHERE %s LIMIT 1;",
		quoteIdent(dbName), quoteIdent(table), strings.Join(setParts, ", "), strings.Join(whereParts, " AND "))
	return c.Exec(ctx, sql)
}

func primaryKeyColumns(cols []ColumnInfo) []string {
	var pk []string
	for _, c := range cols {
		if c.PrimaryKey {
			pk = append(pk, c.Name)
		}
	}
	return pk
}
