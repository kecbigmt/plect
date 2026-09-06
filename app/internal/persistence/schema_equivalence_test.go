package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strconv"
	"testing"

	"github.com/pressly/goose/v3"
)

// TestSchemaSQL_MatchesMigrationHistory is the declaration-equivalence
// check the design requires: applying the embedded goose migration
// history to an empty database must produce the same table structure as
// schema.sql declares, excluding tables goose owns for its own bookkeeping
// (goose_db_version). It compares SQLite's own PRAGMA introspection output
// rather than raw CREATE TABLE text, because Atlas-generated migration SQL
// and hand-written schema.sql are free to differ in quoting and clause
// order while still declaring the same structure.
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
	columns []columnInfo
	indexes []indexInfo
}

type columnInfo struct {
	name       string
	sqlType    string
	notNull    bool
	defaultVal sql.NullString
	pk         int
}

type indexInfo struct {
	name   string
	unique bool
	origin string
	cols   []string
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
			columns: tableColumns(t, ctx, handle, name),
			indexes: tableIndexes(t, ctx, handle, name),
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
		indexes = append(indexes, indexInfo{name: name, unique: unique, origin: origin, cols: indexColumns(t, ctx, handle, name)})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate index_list(%s): %v", table, err)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i].name < indexes[j].name })
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
	if len(want.columns) != len(got.columns) {
		diff += "table " + name + ": schema.sql has " + strconv.Itoa(len(want.columns)) + " columns, migration history has " + strconv.Itoa(len(got.columns)) + "\n"
	}
	n := len(want.columns)
	if len(got.columns) < n {
		n = len(got.columns)
	}
	for i := 0; i < n; i++ {
		if want.columns[i] != got.columns[i] {
			diff += "table " + name + " column " + strconv.Itoa(i) + ": schema.sql=" + columnString(want.columns[i]) + " migration=" + columnString(got.columns[i]) + "\n"
		}
	}
	if len(want.indexes) != len(got.indexes) {
		diff += "table " + name + ": schema.sql has " + strconv.Itoa(len(want.indexes)) + " secondary indexes, migration history has " + strconv.Itoa(len(got.indexes)) + "\n"
	}
	n = len(want.indexes)
	if len(got.indexes) < n {
		n = len(got.indexes)
	}
	for i := 0; i < n; i++ {
		if !indexesEqual(want.indexes[i], got.indexes[i]) {
			diff += "table " + name + " index " + strconv.Itoa(i) + ": schema.sql=" + indexString(want.indexes[i]) + " migration=" + indexString(got.indexes[i]) + "\n"
		}
	}
	return diff
}

func columnString(c columnInfo) string {
	return c.name + " " + c.sqlType
}

// indexInfo embeds a slice (cols), so it cannot use Go's built-in ==;
// two indexes with the same name and column count but different actual
// columns, or the same columns under different uniqueness, must still
// compare unequal.
func indexesEqual(a, b indexInfo) bool {
	if a.name != b.name || a.unique != b.unique || a.origin != b.origin {
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
	return idx.name + " unique=" + strconv.FormatBool(idx.unique) + " cols=" + strconv.Itoa(len(idx.cols)) + ":" + fmt.Sprint(idx.cols)
}

// TestDiffTable_CatchesIndexDefinitionMismatch is the regression this
// declaration-equivalence check needs but the bootstrap persistence_smoke
// table alone can never exercise, since it has no secondary index: two
// tables can carry the same number of indexes while those indexes cover
// different columns or uniqueness, a mismatch a same-count-only check
// would silently accept.
func TestDiffTable_CatchesIndexDefinitionMismatch(t *testing.T) {
	base := tableSchema{
		columns: []columnInfo{{name: "id", sqlType: "INTEGER", pk: 1}},
	}

	cases := map[string]indexInfo{
		"different columns":    {name: "idx_note", unique: true, origin: "c", cols: []string{"other_column"}},
		"different uniqueness": {name: "idx_note", unique: false, origin: "c", cols: []string{"note"}},
	}
	want := base
	want.indexes = []indexInfo{{name: "idx_note", unique: true, origin: "c", cols: []string{"note"}}}

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
