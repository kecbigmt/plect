// Package legacyimport is the one-time JSON/JSONL-to-SQLite importer and
// cutover procedure described by docs/design/sqlite-persistence.md's
// "One-time importer inventory" section and the SQLite durable-storage
// decision record it implements (see that design doc's own opening line
// for the link). It reads a stopped-writer backup of plect's pre-cutover
// data directory (state.json, per-session
// events/<escaped-session>/{log.jsonl,.gen,.cursor.<consumer>}) and builds
// a validated SQLite database from it, preserving session identity, event
// ordering, consumer progress, and generation semantics.
//
// No live command reads these legacy formats any more (app/internal/state
// and app/internal/eventlog are both persistence-backed facades since the
// cutover), so this package owns re-deriving them from history rather than
// reusing any current code path.
//
// This pre-cutover importer validates sidecars but does not copy their data:
// the post-cutover sidecar importer runs against the active database directory
// after its schema migration, not against this stopped legacy backup.
package legacyimport
