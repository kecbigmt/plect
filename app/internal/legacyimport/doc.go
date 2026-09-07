// Package legacyimport is the one-time JSON/JSONL-to-SQLite importer and
// cutover procedure described by docs/design/sqlite-persistence.md's
// "One-time importer inventory" section and the SQLite durable-storage
// decision record it implements (see that design doc's own opening line
// for the link). It reads a stopped-writer backup of plect's pre-cutover
// data directory
// (state.json, per-session events/<escaped-session>/{log.jsonl,.gen,
// .cursor.<consumer>}) and builds a validated SQLite database from it,
// preserving session identity, event ordering, consumer progress, and
// generation/tombstone semantics.
//
// No live command reads these legacy formats any more (app/internal/state
// and app/internal/eventlog are both persistence-backed facades since the
// cutover), so this package owns re-deriving them from history rather than
// reusing any current code path.
//
// session_tombstones and pending_deliveries are deliberately absent from the
// destination schema (see app/internal/persistence/schema.sql's own
// comment): tombstone.json, chain_attempts.json, pending_delivery.json, and
// their lock files stay file-based after cutover exactly where they already
// are, so this package validates but never copies or reads them for data —
// only state.json and the events/ tree feed the database.
package legacyimport
