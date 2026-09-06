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

// foreignKeyInfo groups PRAGMA foreign_key_list's rows by their shared
// constraint id and keeps them ordered by seq: a composite foreign key
// (FOREIGN KEY (a, b) REFERENCES other(x, y)) reports one row per column
// pair, all sharing one id, and seq gives the pairing between each local
// and referenced column. Flattening those rows into independent
// column-pair entries — dropping which pairs belong to the same
// constraint and in what order — would make one two-column composite key
// indistinguishable from two unrelated single-column keys to the same
// table. The id's own numeric value is not part of this, since it is
// itself assigned by declaration order across the table's other foreign
// keys and free to differ between schema.sql and generated SQL.
type foreignKeyInfo struct {
	columns  []fkColumnPair
	toTable  string
	onUpdate string
	onDelete string
}

type fkColumnPair struct {
	fromColumn string
	toColumn   string
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
	masked := strings.ToUpper(maskStringLiterals(createSQL.String))
	where := strings.LastIndex(masked, "WHERE")
	if where == -1 {
		return ""
	}
	return normalizeSQLFragment(createSQL.String[where+len("WHERE"):])
}

// maskStringLiterals masks string-literal content so callers can search
// for keywords and count parens without a literal's own contents (which
// are free to contain "CHECK", "WHERE", or a stray paren) being mistaken
// for syntax; the result stays the same length, so an index found in it
// still locates the same character in s.
func maskStringLiterals(s string) string {
	b := []byte(s)
	inString := false
	for i := 0; i < len(b); i++ {
		switch {
		case inString && b[i] == '\'':
			if i+1 < len(b) && b[i+1] == '\'' {
				// A doubled '' is SQL's escape for a literal quote inside
				// a string, not the string's closing quote.
				b[i] = 'x'
				b[i+1] = 'x'
				i++
				continue
			}
			inString = false
		case inString:
			b[i] = 'x'
		case b[i] == '\'':
			inString = true
		}
	}
	return string(b)
}

func tableForeignKeys(t *testing.T, ctx context.Context, handle *sql.DB, table string) []foreignKeyInfo {
	t.Helper()
	rows, err := handle.QueryContext(ctx, "PRAGMA foreign_key_list("+table+")")
	if err != nil {
		t.Fatalf("foreign_key_list(%s): %v", table, err)
	}
	defer rows.Close()

	type rawRow struct {
		id, seq                         int
		refTable, from, to              string
		onUpdate, onDelete, matchClause string
	}
	var rawByID []rawRow
	seen := map[int]bool{}
	var order []int
	for rows.Next() {
		var r rawRow
		if err := rows.Scan(&r.id, &r.seq, &r.refTable, &r.from, &r.to, &r.onUpdate, &r.onDelete, &r.matchClause); err != nil {
			t.Fatalf("scan foreign_key_list(%s): %v", table, err)
		}
		rawByID = append(rawByID, r)
		if !seen[r.id] {
			seen[r.id] = true
			order = append(order, r.id)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate foreign_key_list(%s): %v", table, err)
	}

	var fks []foreignKeyInfo
	for _, id := range order {
		var group []rawRow
		for _, r := range rawByID {
			if r.id == id {
				group = append(group, r)
			}
		}
		sort.Slice(group, func(i, j int) bool { return group[i].seq < group[j].seq })

		fk := foreignKeyInfo{toTable: group[0].refTable, onUpdate: group[0].onUpdate, onDelete: group[0].onDelete}
		for _, r := range group {
			fk.columns = append(fk.columns, fkColumnPair{fromColumn: r.from, toColumn: r.to})
		}
		fks = append(fks, fk)
	}
	// Sorted by the constraint's own first column pair rather than by id:
	// id is a declaration-order artifact (see foreignKeyInfo's doc comment)
	// and is not itself part of what two schemas need to agree on.
	sort.Slice(fks, func(i, j int) bool { return fks[i].columns[0].fromColumn < fks[j].columns[0].fromColumn })
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

// extractChecks captures each CHECK expression by tracking paren depth
// rather than matching to the first ")", since one is free to contain its
// own nested parentheses, as schema.sql's own sessions-table example does.
func extractChecks(createTableSQL string) []string {
	masked := strings.ToUpper(maskStringLiterals(createTableSQL))
	var checks []string
	for i := 0; i < len(masked); {
		rel := strings.Index(masked[i:], "CHECK")
		if rel == -1 {
			break
		}
		idx := i + rel
		before := idx == 0 || !isIdentByte(masked[idx-1])
		after := idx+5 >= len(masked) || !isIdentByte(masked[idx+5])
		if !before || !after {
			i = idx + 5
			continue
		}
		j := idx + 5
		for j < len(masked) && unicode.IsSpace(rune(masked[j])) {
			j++
		}
		if j >= len(masked) || masked[j] != '(' {
			i = idx + 5
			continue
		}
		depth := 0
		end := -1
		for k := j; k < len(masked); k++ {
			switch masked[k] {
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
// whitespace while expressing the same constraint. It tracks single-quoted
// string literals and leaves their contents untouched: SQL keywords and
// identifiers are case-insensitive, but a string literal's case (and
// internal whitespace) is data, not layout, and folding CHECK (status =
// 'Active') and CHECK (status = 'active') together would hide two
// constraints that accept different values as the same one.
func normalizeSQLFragment(s string) string {
	var b strings.Builder
	inString := false
	pendingSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			b.WriteByte(c)
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					// A doubled '' is SQL's escape for a literal quote
					// inside a string, not the string's closing quote.
					b.WriteByte('\'')
					i++
					continue
				}
				inString = false
			}
			continue
		}
		switch {
		case c == '\'':
			if pendingSpace {
				b.WriteByte(' ')
				pendingSpace = false
			}
			inString = true
			b.WriteByte(c)
		case c == '`' || c == '"' || c == '[' || c == ']':
			// Identifier-quoting punctuation outside a string literal;
			// dropping it lets `col` and col compare equal.
		case unicode.IsSpace(rune(c)):
			pendingSpace = b.Len() > 0
		default:
			if pendingSpace {
				b.WriteByte(' ')
				pendingSpace = false
			}
			b.WriteByte(byte(unicode.ToLower(rune(c))))
		}
	}
	return b.String()
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
	diff += diffSlice(name, "foreign keys", len(want.foreignKeys), len(got.foreignKeys), func(i int) bool { return foreignKeysEqual(want.foreignKeys[i], got.foreignKeys[i]) },
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

// foreignKeyInfo embeds a slice (columns), so it cannot use Go's built-in
// ==; a composite key's column pairs must also match in both membership
// and order, since seq encodes which local column pairs with which
// referenced column.
func foreignKeysEqual(a, b foreignKeyInfo) bool {
	if a.toTable != b.toTable || a.onUpdate != b.onUpdate || a.onDelete != b.onDelete {
		return false
	}
	if len(a.columns) != len(b.columns) {
		return false
	}
	for i := range a.columns {
		if a.columns[i] != b.columns[i] {
			return false
		}
	}
	return true
}

func foreignKeyString(fk foreignKeyInfo) string {
	pairs := make([]string, len(fk.columns))
	for i, c := range fk.columns {
		pairs[i] = c.fromColumn + "->" + c.toColumn
	}
	return "(" + strings.Join(pairs, ", ") + ") -> " + fk.toTable + " on_update=" + fk.onUpdate + " on_delete=" + fk.onDelete
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
	want.foreignKeys = []foreignKeyInfo{{columns: []fkColumnPair{{fromColumn: "parent_id", toColumn: "name"}}, toTable: "sessions", onDelete: "SET NULL"}}

	cases := map[string]foreignKeyInfo{
		"different referenced table":   {columns: []fkColumnPair{{fromColumn: "parent_id", toColumn: "name"}}, toTable: "other_table", onDelete: "SET NULL"},
		"different on_delete behavior": {columns: []fkColumnPair{{fromColumn: "parent_id", toColumn: "name"}}, toTable: "sessions", onDelete: "CASCADE"},
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

// TestDiffTable_DistinguishesCompositeForeignKeyFromSeparateOnes proves a
// two-column composite foreign key does not compare equal to two
// unrelated single-column foreign keys that happen to reference the same
// pair of columns: grouping and column order (seq) is part of what a
// composite key means, not an artifact this check can discard.
func TestDiffTable_DistinguishesCompositeForeignKeyFromSeparateOnes(t *testing.T) {
	base := tableSchema{columns: []columnInfo{{name: "id", sqlType: "INTEGER", pk: 1}}}

	composite := base
	composite.foreignKeys = []foreignKeyInfo{
		{columns: []fkColumnPair{{fromColumn: "a", toColumn: "x"}, {fromColumn: "b", toColumn: "y"}}, toTable: "other"},
	}

	separate := base
	separate.foreignKeys = []foreignKeyInfo{
		{columns: []fkColumnPair{{fromColumn: "a", toColumn: "x"}}, toTable: "other"},
		{columns: []fkColumnPair{{fromColumn: "b", toColumn: "y"}}, toTable: "other"},
	}

	if diff := diffTable("t", composite, separate); diff == "" {
		t.Fatal("diffTable reported no difference between one composite foreign key and two separate single-column ones")
	}

	reordered := base
	reordered.foreignKeys = []foreignKeyInfo{
		{columns: []fkColumnPair{{fromColumn: "b", toColumn: "y"}, {fromColumn: "a", toColumn: "x"}}, toTable: "other"},
	}
	if diff := diffTable("t", composite, reordered); diff == "" {
		t.Fatal("diffTable reported no difference between a composite foreign key and the same columns in a different order")
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

// TestNormalizeSQLFragment_PreservesStringLiteralCaseAndWhitespace proves
// normalizeSQLFragment folds keyword/identifier case and whitespace layout
// but never touches what is inside a string literal — the regression a
// blanket strings.ToLower would reintroduce, silently treating CHECK
// (status = 'Active') and CHECK (status = 'active') as the same
// constraint.
func TestNormalizeSQLFragment_PreservesStringLiteralCaseAndWhitespace(t *testing.T) {
	got := normalizeSQLFragment("( STATUS  =  'Active  Now' )")
	want := "( status = 'Active  Now' )"
	if got != want {
		t.Errorf("normalizeSQLFragment(...) = %q, want %q", got, want)
	}
}

// TestExtractChecks_DistinguishesLiteralCase is the same regression as
// above, exercised through the full CHECK-extraction path rather than
// normalizeSQLFragment in isolation.
func TestExtractChecks_DistinguishesLiteralCase(t *testing.T) {
	upper := extractChecks("CREATE TABLE t (status TEXT, CHECK (status = 'Active'))")
	lower := extractChecks("CREATE TABLE t (status TEXT, CHECK (status = 'active'))")
	if len(upper) != 1 || len(lower) != 1 {
		t.Fatalf("extractChecks found %d and %d constraints, want exactly one each", len(upper), len(lower))
	}
	if upper[0] == lower[0] {
		t.Fatalf("extractChecks did not distinguish string literal case: both normalized to %q", upper[0])
	}
}

func TestExtractChecks_IgnoresSyntaxInsideStringLiterals(t *testing.T) {
	const createTable = `CREATE TABLE t (
		note TEXT,
		CHECK (note <> 'contains a ) paren and the word check')
	)`

	got := extractChecks(createTable)
	want := "(note <> 'contains a ) paren and the word check')"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("extractChecks(...) = %v, want exactly [%q]", got, want)
	}
}

func TestIndexPredicate_IgnoresWhereInsideStringLiteral(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	const ddl = `
		CREATE TABLE t (note TEXT);
		CREATE UNIQUE INDEX t_a_idx ON t(note) WHERE note <> 'anywhere';
		CREATE UNIQUE INDEX t_b_idx ON t(note) WHERE note <> 'elsewhere';
	`
	if _, err := db.write.ExecContext(ctx, ddl); err != nil {
		t.Fatalf("apply fixture DDL: %v", err)
	}

	a := indexPredicate(t, ctx, db.write, "t_a_idx")
	b := indexPredicate(t, ctx, db.write, "t_b_idx")

	if a != "note <> 'anywhere'" {
		t.Errorf("t_a_idx predicate = %q, want %q", a, "note <> 'anywhere'")
	}
	if b != "note <> 'elsewhere'" {
		t.Errorf("t_b_idx predicate = %q, want %q", b, "note <> 'elsewhere'")
	}
	if a == b {
		t.Fatalf("both predicates extracted identically: %q", a)
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
		CREATE TABLE parent (id INTEGER PRIMARY KEY, code TEXT);
		CREATE TABLE child (
			id INTEGER PRIMARY KEY,
			parent_id INTEGER REFERENCES parent(id) ON DELETE CASCADE,
			label TEXT NOT NULL,
			composite_a INTEGER,
			composite_b TEXT,
			CHECK (label <> ''),
			FOREIGN KEY (composite_a, composite_b) REFERENCES parent(id, code)
		);
		CREATE UNIQUE INDEX child_label_idx ON child(label) WHERE label <> 'draft';
	`
	if _, err := db.write.ExecContext(ctx, ddl); err != nil {
		t.Fatalf("apply fixture DDL: %v", err)
	}

	schema := introspect(t, ctx, db.write)["child"]

	wantForeignKeys := []foreignKeyInfo{
		{columns: []fkColumnPair{{fromColumn: "composite_a", toColumn: "id"}, {fromColumn: "composite_b", toColumn: "code"}}, toTable: "parent", onUpdate: "NO ACTION", onDelete: "NO ACTION"},
		{columns: []fkColumnPair{{fromColumn: "parent_id", toColumn: "id"}}, toTable: "parent", onUpdate: "NO ACTION", onDelete: "CASCADE"},
	}
	if len(schema.foreignKeys) != len(wantForeignKeys) {
		t.Fatalf("foreignKeys = %+v, want %+v", schema.foreignKeys, wantForeignKeys)
	}
	for i := range wantForeignKeys {
		if !foreignKeysEqual(schema.foreignKeys[i], wantForeignKeys[i]) {
			t.Errorf("foreignKeys[%d] = %+v, want %+v", i, schema.foreignKeys[i], wantForeignKeys[i])
		}
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
