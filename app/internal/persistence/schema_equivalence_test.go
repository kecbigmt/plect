package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/pressly/goose/v3"
)

// TestSchemaSQL_MatchesMigrationHistory is the declaration-equivalence
// check the design requires: applying the embedded goose migration
// history to an empty database must produce the same table structure as
// schema.sql declares, excluding tables goose owns for its own bookkeeping
// (goose_db_version). It compares SQLite's own introspection rather than
// raw CREATE TABLE text, because Atlas-generated migration SQL and
// hand-written schema.sql are free to differ in quoting and clause order
// while still declaring the same structure. Columns, indexes (including a
// partial index's predicate), and foreign keys have a dedicated PRAGMA;
// CHECK constraints do not, so those are extracted from sqlite_master's
// own CREATE TABLE text instead of being skipped.
func TestSchemaSQL_MatchesMigrationHistory(t *testing.T) {
	ctx := context.Background()

	declared := schemaFromSQLFile(t, ctx, "schema.sql")
	migrated := schemaFromMigrationHistory(t, ctx)

	delete(migrated, goose.DefaultTablename)

	if diff := diffTableSets(declared, migrated); diff != "" {
		t.Fatalf("schema.sql and the migration history disagree:\n%s", diff)
	}
}

type tableSchema struct {
	columns     []columnInfo
	indexes     []indexInfo
	foreignKeys []foreignKeyInfo
	checks      []string
}

type columnInfo struct {
	name       string
	sqlType    string
	notNull    bool
	defaultVal sql.NullString
	pk         int
}

// wherePredicate is the partial index's normalized WHERE clause, or "" for
// a full index; a check limited to name/unique/columns would treat a
// partial index and a full index over the same columns as identical, but
// they enforce uniqueness (or accelerate lookups) over different row sets.
type indexInfo struct {
	name           string
	unique         bool
	origin         string
	cols           []string
	partial        bool
	wherePredicate string
}

// foreignKeyInfo omits SQLite's numeric constraint id/sequence columns:
// those numbers are assigned by declaration order, which is exactly the
// kind of textual-layout difference schema.sql and Atlas-generated SQL are
// free to vary on while still declaring the same relationship.
type foreignKeyInfo struct {
	fromColumn string
	toTable    string
	toColumn   string
	onUpdate   string
	onDelete   string
}

func schemaFromSQLFile(t *testing.T, ctx context.Context, path string) map[string]tableSchema {
	t.Helper()
	stmt, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	db := openTestDB(t)
	if _, err := db.write.ExecContext(ctx, string(stmt)); err != nil {
		t.Fatalf("apply %s: %v", path, err)
	}
	return introspect(t, ctx, db.write)
}

func schemaFromMigrationHistory(t *testing.T, ctx context.Context) map[string]tableSchema {
	t.Helper()
	db := openTestDB(t)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return introspect(t, ctx, db.write)
}

func introspect(t *testing.T, ctx context.Context, handle *sql.DB) map[string]tableSchema {
	t.Helper()
	rows, err := handle.QueryContext(ctx, `
		SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name
	`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()

	result := make(map[string]tableSchema)
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table names: %v", err)
	}

	for _, name := range names {
		result[name] = tableSchema{
			columns:     tableColumns(t, ctx, handle, name),
			indexes:     tableIndexes(t, ctx, handle, name),
			foreignKeys: tableForeignKeys(t, ctx, handle, name),
			checks:      tableChecks(t, ctx, handle, name),
		}
	}
	return result
}

func tableColumns(t *testing.T, ctx context.Context, handle *sql.DB, table string) []columnInfo {
	t.Helper()
	// table_info's argument cannot be bound as a query parameter; table
	// names here come only from sqlite_master, never from external input.
	rows, err := handle.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()

	var cols []columnInfo
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notNull bool
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		cols = append(cols, columnInfo{name: name, sqlType: ctype, notNull: notNull, defaultVal: dflt, pk: pk})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info(%s): %v", table, err)
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].name < cols[j].name })
	return cols
}

func tableIndexes(t *testing.T, ctx context.Context, handle *sql.DB, table string) []indexInfo {
	t.Helper()
	indexes := listIndexes(t, ctx, handle, table)

	// The per-index detail queries below (indexColumns, indexPredicate)
	// run only after this function's own listIndexes call has returned,
	// so its PRAGMA index_list cursor is already closed: db.write caps its
	// pool at one connection (see open.go), so a nested query issued
	// while that cursor is still open would deadlock waiting for a
	// connection this same, still-iterating cursor is holding.
	for i := range indexes {
		indexes[i].cols = indexColumns(t, ctx, handle, indexes[i].name)
		if indexes[i].partial {
			indexes[i].wherePredicate = indexPredicate(t, ctx, handle, indexes[i].name)
		}
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i].name < indexes[j].name })
	return indexes
}

func listIndexes(t *testing.T, ctx context.Context, handle *sql.DB, table string) []indexInfo {
	t.Helper()
	rows, err := handle.QueryContext(ctx, "PRAGMA index_list("+table+")")
	if err != nil {
		t.Fatalf("index_list(%s): %v", table, err)
	}
	defer rows.Close()

	var indexes []indexInfo
	for rows.Next() {
		var (
			seq     int
			name    string
			unique  bool
			origin  string
			partial bool
		)
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			t.Fatalf("scan index_list(%s): %v", table, err)
		}
		if origin == "pk" {
			// An implicit rowid-alias or inline PRIMARY KEY constraint
			// already shows up via table_info's pk column; including its
			// synthesized index here would double-count the same fact.
			continue
		}
		indexes = append(indexes, indexInfo{name: name, unique: unique, origin: origin, partial: partial})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate index_list(%s): %v", table, err)
	}
	return indexes
}

func indexColumns(t *testing.T, ctx context.Context, handle *sql.DB, index string) []string {
	t.Helper()
	rows, err := handle.QueryContext(ctx, "PRAGMA index_info("+index+")")
	if err != nil {
		t.Fatalf("index_info(%s): %v", index, err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var seqno, cid int
		var name sql.NullString
		if err := rows.Scan(&seqno, &cid, &name); err != nil {
			t.Fatalf("scan index_info(%s): %v", index, err)
		}
		cols = append(cols, name.String)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate index_info(%s): %v", index, err)
	}
	return cols
}

// indexPredicate reads a partial index's WHERE clause from
// sqlite_master.sql: no PRAGMA reports it, since PRAGMA index_list only
// says whether an index is partial, not what its predicate is.
func indexPredicate(t *testing.T, ctx context.Context, handle *sql.DB, index string) string {
	t.Helper()
	var createSQL sql.NullString
	err := handle.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?", index).Scan(&createSQL)
	if err != nil {
		t.Fatalf("read sqlite_master.sql for index %s: %v", index, err)
	}
	if !createSQL.Valid {
		// An index SQLite created implicitly for an inline UNIQUE column
		// constraint has no sqlite_master row of its own and can never be
		// partial, so there is no predicate to find.
		return ""
	}
	upper := strings.ToUpper(createSQL.String)
	where := strings.LastIndex(upper, "WHERE")
	if where == -1 {
		return ""
	}
	return normalizeSQLFragment(createSQL.String[where+len("WHERE"):])
}

func tableForeignKeys(t *testing.T, ctx context.Context, handle *sql.DB, table string) []foreignKeyInfo {
	t.Helper()
	rows, err := handle.QueryContext(ctx, "PRAGMA foreign_key_list("+table+")")
	if err != nil {
		t.Fatalf("foreign_key_list(%s): %v", table, err)
	}
	defer rows.Close()

	var fks []foreignKeyInfo
	for rows.Next() {
		var (
			id, seq                         int
			refTable, from, to              string
			onUpdate, onDelete, matchClause string
		)
		if err := rows.Scan(&id, &seq, &refTable, &from, &to, &onUpdate, &onDelete, &matchClause); err != nil {
			t.Fatalf("scan foreign_key_list(%s): %v", table, err)
		}
		fks = append(fks, foreignKeyInfo{fromColumn: from, toTable: refTable, toColumn: to, onUpdate: onUpdate, onDelete: onDelete})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate foreign_key_list(%s): %v", table, err)
	}
	sort.Slice(fks, func(i, j int) bool { return fks[i].fromColumn < fks[j].fromColumn })
	return fks
}

// tableChecks extracts CHECK constraint expressions from sqlite_master's
// own CREATE TABLE text: SQLite has no PRAGMA that reports them, unlike
// columns, indexes, and foreign keys, so skipping this table's CHECK
// clauses would leave the one constraint kind this check cannot verify
// through introspection alone.
func tableChecks(t *testing.T, ctx context.Context, handle *sql.DB, table string) []string {
	t.Helper()
	var createSQL string
	err := handle.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&createSQL)
	if err != nil {
		t.Fatalf("read sqlite_master.sql for table %s: %v", table, err)
	}
	return extractChecks(createSQL)
}

// extractChecks finds each standalone "CHECK" keyword in a CREATE TABLE
// statement and captures its parenthesized expression by tracking paren
// depth, rather than matching to the first ")" — a table- or column-level
// CHECK expression is free to contain its own nested parentheses (as
// schema.sql's own sessions-table example does).
func extractChecks(createTableSQL string) []string {
	upper := strings.ToUpper(createTableSQL)
	var checks []string
	for i := 0; i < len(upper); {
		rel := strings.Index(upper[i:], "CHECK")
		if rel == -1 {
			break
		}
		idx := i + rel
		before := idx == 0 || !isIdentByte(createTableSQL[idx-1])
		after := idx+5 >= len(createTableSQL) || !isIdentByte(createTableSQL[idx+5])
		if !before || !after {
			i = idx + 5
			continue
		}
		j := idx + 5
		for j < len(createTableSQL) && unicode.IsSpace(rune(createTableSQL[j])) {
			j++
		}
		if j >= len(createTableSQL) || createTableSQL[j] != '(' {
			i = idx + 5
			continue
		}
		depth := 0
		end := -1
		for k := j; k < len(createTableSQL); k++ {
			switch createTableSQL[k] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					end = k
				}
			}
			if end != -1 {
				break
			}
		}
		if end == -1 {
			// An unclosed paren means the statement was truncated or this
			// scan mis-tracked depth; either way there is nothing left to
			// safely extract, so stop rather than report a partial match.
			break
		}
		checks = append(checks, normalizeSQLFragment(createTableSQL[j:end+1]))
		i = end + 1
	}
	sort.Strings(checks)
	return checks
}

func isIdentByte(b byte) bool {
	return b == '_' || unicode.IsLetter(rune(b)) || unicode.IsDigit(rune(b))
}

// normalizeSQLFragment lets a CHECK or partial-index predicate compare
// equal across schema.sql and Atlas-generated SQL despite the two being
// free to differ in identifier quoting (backticks vs. none) and
// whitespace while expressing the same constraint.
func normalizeSQLFragment(s string) string {
	s = strings.NewReplacer("`", "", `"`, "", "[", "", "]", "").Replace(s)
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func diffTableSets(declared, migrated map[string]tableSchema) string {
	diff := ""
	seen := make(map[string]bool)
	for name, want := range declared {
		seen[name] = true
		got, ok := migrated[name]
		if !ok {
			diff += "table " + name + ": in schema.sql, missing from migration history\n"
			continue
		}
		diff += diffTable(name, want, got)
	}
	for name := range migrated {
		if !seen[name] {
			diff += "table " + name + ": in migration history, missing from schema.sql\n"
		}
	}
	return diff
}

func diffTable(name string, want, got tableSchema) string {
	diff := ""
	diff += diffSlice(name, "columns", len(want.columns), len(got.columns), func(i int) bool { return want.columns[i] == got.columns[i] },
		func(i int) string { return columnString(want.columns[i]) }, func(i int) string { return columnString(got.columns[i]) })
	diff += diffSlice(name, "secondary indexes", len(want.indexes), len(got.indexes), func(i int) bool { return indexesEqual(want.indexes[i], got.indexes[i]) },
		func(i int) string { return indexString(want.indexes[i]) }, func(i int) string { return indexString(got.indexes[i]) })
	diff += diffSlice(name, "foreign keys", len(want.foreignKeys), len(got.foreignKeys), func(i int) bool { return want.foreignKeys[i] == got.foreignKeys[i] },
		func(i int) string { return foreignKeyString(want.foreignKeys[i]) }, func(i int) string { return foreignKeyString(got.foreignKeys[i]) })
	diff += diffSlice(name, "CHECK constraints", len(want.checks), len(got.checks), func(i int) bool { return want.checks[i] == got.checks[i] },
		func(i int) string { return want.checks[i] }, func(i int) string { return got.checks[i] })
	return diff
}

// diffSlice is shared by every element kind diffTable compares (columns,
// indexes, foreign keys, CHECK constraints): each already sorts into a
// stable order during introspection, so a length mismatch and a positional
// mismatch are the same two checks regardless of what is being compared.
func diffSlice(table, label string, wantLen, gotLen int, equal func(i int) bool, wantString, gotString func(i int) string) string {
	diff := ""
	if wantLen != gotLen {
		diff += "table " + table + ": schema.sql has " + strconv.Itoa(wantLen) + " " + label + ", migration history has " + strconv.Itoa(gotLen) + "\n"
	}
	n := wantLen
	if gotLen < n {
		n = gotLen
	}
	for i := 0; i < n; i++ {
		if !equal(i) {
			diff += "table " + table + " " + label + " " + strconv.Itoa(i) + ": schema.sql=" + wantString(i) + " migration=" + gotString(i) + "\n"
		}
	}
	return diff
}

func columnString(c columnInfo) string {
	return c.name + " " + c.sqlType
}

// indexInfo embeds a slice (cols), so it cannot use Go's built-in ==; two
// indexes with the same name and column count but different actual
// columns, different uniqueness, or a differing (or missing) partial
// predicate must still compare unequal.
func indexesEqual(a, b indexInfo) bool {
	if a.name != b.name || a.unique != b.unique || a.origin != b.origin || a.partial != b.partial || a.wherePredicate != b.wherePredicate {
		return false
	}
	if len(a.cols) != len(b.cols) {
		return false
	}
	for i := range a.cols {
		if a.cols[i] != b.cols[i] {
			return false
		}
	}
	return true
}

func indexString(idx indexInfo) string {
	return idx.name + " unique=" + strconv.FormatBool(idx.unique) + " partial=" + strconv.FormatBool(idx.partial) +
		" where=" + strconv.Quote(idx.wherePredicate) + " cols=" + strconv.Itoa(len(idx.cols)) + ":" + fmt.Sprint(idx.cols)
}

func foreignKeyString(fk foreignKeyInfo) string {
	return fk.fromColumn + " -> " + fk.toTable + "." + fk.toColumn + " on_update=" + fk.onUpdate + " on_delete=" + fk.onDelete
}

// TestDiffTable_CatchesIndexDefinitionMismatch is the regression this
// declaration-equivalence check needs but the bootstrap persistence_smoke
// table alone can never exercise, since it has no secondary index: two
// tables can carry the same number of indexes while those indexes cover
// different columns, uniqueness, or partial predicates — a mismatch a
// same-count-only check would silently accept.
func TestDiffTable_CatchesIndexDefinitionMismatch(t *testing.T) {
	base := tableSchema{
		columns: []columnInfo{{name: "id", sqlType: "INTEGER", pk: 1}},
	}
	want := base
	want.indexes = []indexInfo{{name: "idx_note", unique: true, origin: "c", cols: []string{"note"}}}

	cases := map[string]indexInfo{
		"different columns":         {name: "idx_note", unique: true, origin: "c", cols: []string{"other_column"}},
		"different uniqueness":      {name: "idx_note", unique: false, origin: "c", cols: []string{"note"}},
		"gains a partial predicate": {name: "idx_note", unique: true, origin: "c", cols: []string{"note"}, partial: true, wherePredicate: "note <> ''"},
	}
	for name, mismatched := range cases {
		t.Run(name, func(t *testing.T) {
			got := base
			got.indexes = []indexInfo{mismatched}

			if diff := diffTable("t", want, got); diff == "" {
				t.Fatal("diffTable reported no difference for tables with the same index count but a mismatched index definition")
			}
		})
	}

	identical := base
	identical.indexes = []indexInfo{{name: "idx_note", unique: true, origin: "c", cols: []string{"note"}}}
	if diff := diffTable("t", want, identical); diff != "" {
		t.Fatalf("diffTable reported a difference for identical index definitions: %s", diff)
	}
}

// TestDiffTable_CatchesForeignKeyMismatch: same shape of gap as the index
// case above, for the constraint kind PRAGMA foreign_key_list reports.
func TestDiffTable_CatchesForeignKeyMismatch(t *testing.T) {
	base := tableSchema{columns: []columnInfo{{name: "id", sqlType: "INTEGER", pk: 1}}}
	want := base
	want.foreignKeys = []foreignKeyInfo{{fromColumn: "parent_id", toTable: "sessions", toColumn: "name", onDelete: "SET NULL"}}

	cases := map[string]foreignKeyInfo{
		"different referenced table":   {fromColumn: "parent_id", toTable: "other_table", toColumn: "name", onDelete: "SET NULL"},
		"different on_delete behavior": {fromColumn: "parent_id", toTable: "sessions", toColumn: "name", onDelete: "CASCADE"},
	}
	for name, mismatched := range cases {
		t.Run(name, func(t *testing.T) {
			got := base
			got.foreignKeys = []foreignKeyInfo{mismatched}

			if diff := diffTable("t", want, got); diff == "" {
				t.Fatal("diffTable reported no difference for tables with the same foreign key count but a mismatched definition")
			}
		})
	}
}

// TestDiffTable_CatchesCheckConstraintMismatch: same shape of gap again,
// for the one constraint kind with no PRAGMA at all (tableChecks parses
// sqlite_master.sql instead).
func TestDiffTable_CatchesCheckConstraintMismatch(t *testing.T) {
	base := tableSchema{columns: []columnInfo{{name: "id", sqlType: "INTEGER", pk: 1}}}
	want := base
	want.checks = []string{"(sequence > 0)"}

	got := base
	got.checks = []string{"(sequence >= 0)"}

	if diff := diffTable("t", want, got); diff == "" {
		t.Fatal("diffTable reported no difference for tables with the same CHECK constraint count but different expressions")
	}
}

// TestExtractChecks_HandlesNestedParens proves extractChecks does not stop
// at the first ")" inside a CHECK expression: schema.sql's own sessions
// table (docs/design/sqlite-persistence.md) nests a NOT (...) inside its
// CHECK, and a naive scan would truncate the expression there.
func TestExtractChecks_HandlesNestedParens(t *testing.T) {
	const createTable = "CREATE TABLE sessions (\n" +
		"    parent_session_name TEXT,\n" +
		"    root_session_name TEXT,\n" +
		"    CHECK (NOT (parent_session_name IS NOT NULL AND root_session_name IS NOT NULL)),\n" +
		"    CHECK (root_session_name IS NULL OR root_session_name <> parent_session_name)\n" +
		")"

	got := extractChecks(createTable)
	want := []string{
		"(not (parent_session_name is not null and root_session_name is not null))",
		"(root_session_name is null or root_session_name <> parent_session_name)",
	}
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("extractChecks found %d constraints, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("extractChecks()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestIntrospect_ExtractsRealConstraintsFromSQLite proves the PRAGMA and
// sqlite_master parsing in tableForeignKeys, tableIndexes, and
// tableChecks actually recover the right values from a real SQLite
// database, not just that diffTable notices when two already-parsed
// tableSchema values differ.
func TestIntrospect_ExtractsRealConstraintsFromSQLite(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	const ddl = `
		CREATE TABLE parent (id INTEGER PRIMARY KEY);
		CREATE TABLE child (
			id INTEGER PRIMARY KEY,
			parent_id INTEGER REFERENCES parent(id) ON DELETE CASCADE,
			label TEXT NOT NULL,
			CHECK (label <> '')
		);
		CREATE UNIQUE INDEX child_label_idx ON child(label) WHERE label <> 'draft';
	`
	if _, err := db.write.ExecContext(ctx, ddl); err != nil {
		t.Fatalf("apply fixture DDL: %v", err)
	}

	schema := introspect(t, ctx, db.write)["child"]

	if len(schema.foreignKeys) != 1 || schema.foreignKeys[0] != (foreignKeyInfo{fromColumn: "parent_id", toTable: "parent", toColumn: "id", onDelete: "CASCADE", onUpdate: "NO ACTION"}) {
		t.Errorf("foreignKeys = %+v, want one CASCADE-on-delete reference to parent.id", schema.foreignKeys)
	}
	if len(schema.checks) != 1 || schema.checks[0] != "(label <> '')" {
		t.Errorf("checks = %v, want exactly [\"(label <> '')\"]", schema.checks)
	}

	var idx *indexInfo
	for i := range schema.indexes {
		if schema.indexes[i].name == "child_label_idx" {
			idx = &schema.indexes[i]
		}
	}
	if idx == nil {
		t.Fatalf("indexes = %+v, want child_label_idx present", schema.indexes)
	}
	if !idx.partial || idx.wherePredicate != "label <> 'draft'" {
		t.Errorf("child_label_idx = %+v, want partial=true wherePredicate=%q", idx, "label <> 'draft'")
	}
}
