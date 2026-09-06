package persistence

import (
	"database/sql"
	"time"
)

// timeLayout is the on-disk text encoding for every timestamp column this
// package writes: UTC RFC3339 with exactly nine fractional digits (e.g.
// "2026-09-06T08:50:42.821423717Z"). A fixed width — never the trailing-zero-
// trimmed time.RFC3339Nano — is what makes lexical order equal time order;
// see TestFormatTime_LexicalOrderMatchesTimeOrder.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// formatTime encodes t for a NOT NULL timestamp column that domain logic
// always populates (created_at, updated_at, a judge's created_at, a
// reservation's reserved_at). Callers never pass a zero Time here; a column
// that can genuinely be unset uses formatTimeNull instead.
func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

// parseTime is formatTime's inverse.
func parseTime(s string) (time.Time, error) {
	return time.Parse(timeLayout, s)
}

// formatTimeNull encodes t for a nullable timestamp column: a zero Time (an
// unset "omitzero" field in the domain model) becomes SQL NULL rather than
// a formatted zero date, so the column stays a plain marker of "never set"
// instead of an arbitrary-looking timestamp.
func formatTimeNull(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: formatTime(t), Valid: true}
}

// parseTimeNull is formatTimeNull's inverse.
func parseTimeNull(s sql.NullString) (time.Time, error) {
	if !s.Valid {
		return time.Time{}, nil
	}
	return parseTime(s.String)
}

// nullString converts a possibly-empty Go string to a nullable column value:
// empty means "absent" (SQL NULL), matching the same real-absence-is-NULL
// convention formatTimeNull uses for timestamps.
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
