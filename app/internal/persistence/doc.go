// Package persistence owns the SQLite database that backs core's durable
// runtime state, per docs/design/sqlite-persistence.md. It opens the
// database with WAL, a bounded busy timeout, and foreign-key enforcement on
// every connection, applies the embedded goose migration history, and
// exposes a write-transaction helper that always starts with BEGIN
// IMMEDIATE so later slices never have to reason about a deferred
// transaction's read-then-write upgrade failure.
//
// Runtime state tables (sessions, task/node instances, done_when/judge
// state, populations, up-slot reservations) live in schema.sql; the
// migration history also records a since-retired persistence_smoke table
// that once proved the schema.sql -> Atlas -> goose -> SQLite and
// schema.sql -> sqlc -> Go pipelines end to end before any domain table
// existed to prove them instead.
//
// # Regenerating
//
// Migrations (schema.sql -> reviewed goose SQL):
//
//	atlas migrate diff <name> \
//	    --dir "file://internal/persistence/migrations" \
//	    --dir-format goose \
//	    --to "file://internal/persistence/schema.sql" \
//	    --dev-url "sqlite://file?mode=memory&cache=shared"
//
// Atlas Community Edition is pinned at v1.3.0. Fetch the **community**
// binary specifically (Apache-2.0) rather than the default MSA-licensed
// one, and verify it before running:
//
//	curl -sSL -o atlas https://release.ariga.io/atlas/atlas-community-<os>-<arch>-v1.3.0
//	echo "<checksum>  atlas" | sha256sum -c -
//
// v1.3.0 community-binary SHA-256 checksums, one per platform a developer
// might run this on (CI only ever needs linux-amd64; the rest are here so
// a developer on another platform is never left to trust an unverified
// download):
//
//	linux-amd64:   10d7913e3dce43ab99b8d71534a4cbadaf11a16dc293adf3b91d10e83a0ac70b
//	darwin-amd64:  650981a024301775ec964e5134e2d5712b7ef1b25fec4b2ec54bad762b4bdf6f
//	darwin-arm64:  4e5ffdc10b2b4fd3a06074aba72848150907d5316cacad7b19bae6c6ae3db991
//
// Query code (schema.sql + queries.sql -> sqlcgen):
//
//	go generate ./internal/persistence/...
//
// # Pinned tool versions
//
// github.com/mattn/go-sqlite3 v1.14.52, github.com/pressly/goose/v3
// v3.27.0, and sqlc v1.30.0 (a go.mod tool directive) are each held one or
// two minors behind their own latest tag deliberately: goose v3.28.0 and
// sqlc v1.31.x each declare `go 1.26.0` in their own go.mod, and adding
// either as-is would force this module's toolchain to 1.26 as a side
// effect of a library/tool pin rather than as its own deliberate decision.
// The pins above are each the newest tag still compatible with this
// module's `go 1.25.6`. Revisit them together with a real toolchain bump,
// not incidentally.
package persistence

//go:generate go tool sqlc generate --file sqlc.yaml
