package persistence

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing/fstest"
)

// RealMigrationsMinusLatestForTest returns the real embedded migration set
// with its highest-versioned file removed. A test uses it, together with
// SeedWithMigrationsForTest, to bring a real database to one migration
// short of the actual target self-consistently — by running every earlier
// migration's real Up script — rather than hand-editing goose's ledger out
// of step with the schema it describes. No production code calls this.
func RealMigrationsMinusLatestForTest() fs.FS {
	full := migrationsSourceFS()
	entries, err := fs.ReadDir(full, ".")
	if err != nil {
		panic(fmt.Sprintf("persistence: read embedded migrations: %v", err))
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) < 2 {
		panic(fmt.Sprintf("persistence: embedded migrations tree has %d .sql files, want at least 2 to drop the last one", len(names)))
	}
	sub := make(fstest.MapFS, len(names)-1)
	for _, name := range names[:len(names)-1] {
		data, err := fs.ReadFile(full, name)
		if err != nil {
			panic(fmt.Sprintf("persistence: read embedded migration %s: %v", name, err))
		}
		sub[name] = &fstest.MapFile{Data: data}
	}
	return sub
}

// SeedWithMigrationsForTest opens (creating if needed) the database at path
// and migrates it using migrations instead of the real embedded tree, so a
// test can seed an arbitrary real-or-partial schema version. No production
// code calls this; EnsureCurrent and EnsureCurrentAllowDevBuild always use
// the real tree.
func SeedWithMigrationsForTest(ctx context.Context, path string, migrations fs.FS) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("persistence: create database directory: %w", err)
	}
	db, err := Open(path)
	if err != nil {
		return err
	}
	db.migrations = migrations
	if err := db.Migrate(ctx); err != nil {
		db.Close()
		return err
	}
	return db.Close()
}
