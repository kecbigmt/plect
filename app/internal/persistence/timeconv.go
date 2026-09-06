package persistence

import "time"

// timeLayout is the on-disk text encoding for every timestamp column this
// package writes. RFC3339Nano round-trips through SQLite's TEXT storage
// class exactly and sorts lexically in append order.
const timeLayout = time.RFC3339Nano

// formatTime encodes t for a NOT NULL TEXT timestamp column. A zero Time
// (an unset "omitzero" field in the domain model) becomes an empty string
// rather than a formatted zero date, so the column stays a plain marker of
// "never set" instead of an arbitrary-looking timestamp.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeLayout)
}

// parseTime is formatTime's inverse: an empty string is the unset case,
// anything else must parse as a valid timestamp.
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(timeLayout, s)
}
