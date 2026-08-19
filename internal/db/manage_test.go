package db

import "testing"

// quoteIdent and quoteLiteral are the two primitives every dynamic query in
// manage.go depends on for safety — get either wrong and every function
// built on top of it is exploitable, so they get direct, exhaustive tests
// rather than relying only on the higher-level functions' own tests to
// catch a regression here indirectly.

func TestQuoteIdentDoublesBackticks(t *testing.T) {
	cases := map[string]string{
		"users":        "`users`",
		"my`table":     "`my``table`",
		"``":           "``````",
		"weird col":    "`weird col`",
		"MixedCase123": "`MixedCase123`",
	}
	for in, want := range cases {
		if got := quoteIdent(in); got != want {
			t.Errorf("quoteIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoteIdentCannotBeEscapedOutOfIdentifierContext(t *testing.T) {
	// An identifier containing a backtick followed by SQL that would matter
	// if it broke out of quoting — after quoting, the doubled backtick must
	// still read as *inside* one continuous quoted identifier, never as
	// "end quote, then raw SQL, then a stray quote".
	evil := "x`; DROP TABLE users; --"
	got := quoteIdent(evil)
	want := "`x``; DROP TABLE users; --`"
	if got != want {
		t.Fatalf("quoteIdent(%q) = %q, want %q", evil, got, want)
	}
}

func TestQuoteLiteralEscapesBackslashAndQuote(t *testing.T) {
	cases := map[string]string{
		"hello":      "'hello'",
		"it's":       `'it\'s'`,
		`back\slash`: `'back\\slash'`,
		"":           "''",
		"a'; DROP--": `'a\'; DROP--'`,
	}
	for in, want := range cases {
		if got := quoteLiteral(in); got != want {
			t.Errorf("quoteLiteral(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoteLiteralOrderMatters(t *testing.T) {
	// Escaping the quote before the backslash would double-escape and
	// corrupt the value; escaping backslash first, then quote, is the only
	// order that round-trips correctly.
	in := `\'`
	got := quoteLiteral(in)
	want := `'\\\''`
	if got != want {
		t.Fatalf("quoteLiteral(%q) = %q, want %q", in, got, want)
	}
}

func TestGrantPrivilegesValidatesBeforeExecuting(t *testing.T) {
	c := Client{}
	if err := c.GrantPrivileges(nil, "u", "localhost", "bad;db", []string{"SELECT"}); err == nil {
		t.Error("accepted an unsafe database name")
	}
	if err := c.GrantPrivileges(nil, "u", "localhost", "ok_db", nil); err == nil {
		t.Error("accepted an empty privilege list")
	}
	if err := c.GrantPrivileges(nil, "u", "localhost", "ok_db", []string{"SUPER"}); err == nil {
		t.Error("accepted a non-managed privilege (SUPER)")
	}
	if err := c.GrantPrivileges(nil, "u", "localhost", "ok_db", []string{"SELECT; DROP DATABASE mysql"}); err == nil {
		t.Error("accepted a privilege string with SQL injected into it")
	}
}

func TestRevokeAllPrivilegesValidatesIdentifier(t *testing.T) {
	c := Client{}
	if err := c.RevokeAllPrivileges(nil, "u", "localhost", "bad;db"); err == nil {
		t.Error("accepted an unsafe database name")
	}
}

func TestCreateManagedUserRejectsWeakPassword(t *testing.T) {
	c := Client{}
	if err := c.CreateManagedUser(nil, "newuser", "localhost", "short"); err == nil {
		t.Error("accepted a password under 12 characters")
	}
}

func TestDropManagedUserRefusesBuiltinAccounts(t *testing.T) {
	c := Client{}
	for _, name := range []string{"root", "mysql.sys", "debian-sys-maint"} {
		if err := c.DropManagedUser(nil, name, "localhost"); err == nil {
			t.Errorf("DropManagedUser(%q) should have been refused", name)
		}
	}
}

func TestListTablesValidatesIdentifier(t *testing.T) {
	c := Client{}
	if _, err := c.ListTables(nil, "bad;db"); err == nil {
		t.Error("accepted an unsafe database name")
	}
}

func TestTableColumnsValidatesIdentifier(t *testing.T) {
	c := Client{}
	if _, err := c.TableColumns(nil, "bad;db", "t"); err == nil {
		t.Error("accepted an unsafe database name")
	}
}

func TestTableRowsValidatesIdentifier(t *testing.T) {
	c := Client{}
	if _, _, err := c.TableRows(nil, "bad;db", "t", nil, 1, 100); err == nil {
		t.Error("accepted an unsafe database name")
	}
}

func TestUpdateRowRequiresPrimaryKey(t *testing.T) {
	c := Client{}
	cols := []ColumnInfo{{Name: "id", PrimaryKey: false}, {Name: "name"}}
	err := c.UpdateRow(nil, "ok_db", "t", cols, map[string]string{"id": "1"}, map[string]string{"name": "x"})
	if err == nil {
		t.Error("expected an error for a table with no primary key")
	}
}

func TestUpdateRowRejectsUnknownColumn(t *testing.T) {
	c := Client{}
	cols := []ColumnInfo{{Name: "id", PrimaryKey: true}, {Name: "name"}}
	err := c.UpdateRow(nil, "ok_db", "t", cols, map[string]string{"id": "1"}, map[string]string{"not_a_real_column": "x"})
	if err == nil {
		t.Error("expected an error for a column that doesn't exist on the table")
	}
}

func TestUpdateRowRequiresAllPrimaryKeyValues(t *testing.T) {
	c := Client{}
	cols := []ColumnInfo{{Name: "a", PrimaryKey: true}, {Name: "b", PrimaryKey: true}, {Name: "v"}}
	err := c.UpdateRow(nil, "ok_db", "t", cols, map[string]string{"a": "1"}, map[string]string{"v": "x"})
	if err == nil {
		t.Error("expected an error when a composite primary key is only partially supplied")
	}
}

func TestUpdateRowValidatesIdentifier(t *testing.T) {
	c := Client{}
	cols := []ColumnInfo{{Name: "id", PrimaryKey: true}}
	err := c.UpdateRow(nil, "bad;db", "t", cols, map[string]string{"id": "1"}, map[string]string{"id": "2"})
	if err == nil {
		t.Error("accepted an unsafe database name")
	}
}

func TestPrimaryKeyColumns(t *testing.T) {
	cols := []ColumnInfo{
		{Name: "a", PrimaryKey: true},
		{Name: "b", PrimaryKey: false},
		{Name: "c", PrimaryKey: true},
	}
	got := primaryKeyColumns(cols)
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("primaryKeyColumns = %v, want [a c]", got)
	}
}

// TestParseColumnsKeepsEmptyTrailingField is a regression test for a real
// bug found live against a real WordPress database: mysql --batch's
// tab-separated output for a column with no key (COLUMN_KEY, the last
// field selected) ends in a trailing tab with nothing after it —
// "option_value\tlongtext\tNO\t" — and TrimSpace treats that trailing tab
// as whitespace and strips it, leaving only 3 tab-separated fields where 4
// were sent. Every non-indexed column of every real table was silently
// dropped from the column list (and, downstream, from TableRows' row
// data) until this was caught.
func TestParseColumnsKeepsEmptyTrailingField(t *testing.T) {
	raw := "option_id\tbigint unsigned\tNO\tPRI\n" +
		"option_name\tvarchar(191)\tNO\tUNI\n" +
		"option_value\tlongtext\tNO\t\n" + // <- no key: trailing tab, empty field
		"autoload\tvarchar(20)\tNO\tMUL"
	cols := parseColumns(raw)
	if len(cols) != 4 {
		t.Fatalf("parseColumns returned %d columns, want 4: %+v", len(cols), cols)
	}
	ov := cols[2]
	if ov.Name != "option_value" || ov.Type != "longtext" || ov.PrimaryKey {
		t.Errorf("column 3 = %+v, want {Name:option_value Type:longtext Nullable:false PrimaryKey:false}", ov)
	}
}

func TestParseColumnsSkipsBlankLines(t *testing.T) {
	raw := "id\tint\tNO\tPRI\n\nname\tvarchar(50)\tYES\t"
	cols := parseColumns(raw)
	if len(cols) != 2 {
		t.Fatalf("parseColumns returned %d columns, want 2: %+v", len(cols), cols)
	}
	if !cols[1].Nullable {
		t.Errorf("column 2 Nullable = false, want true (IS_NULLABLE was YES)")
	}
}

func TestListDatabasesParsesAndSkipsSystemSchemas(t *testing.T) {
	// parseSchemaSizes-style test would need a real client; instead confirm
	// the systemSchemas filter itself is complete for the names this
	// package actually excludes.
	for _, name := range []string{"information_schema", "performance_schema", "mysql", "sys"} {
		if !systemSchemas[name] {
			t.Errorf("systemSchemas is missing %q", name)
		}
	}
}
