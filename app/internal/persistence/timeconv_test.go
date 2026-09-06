package persistence

import (
	"database/sql"
	"sort"
	"testing"
	"time"
)

// TestFormatTime_LexicalOrderMatchesTimeOrder proves the guarantee
// timeLayout's doc comment claims: a fixed nine-fractional-digit width
// means sorting formatTime's output as plain strings agrees with sorting
// the original times, unlike time.RFC3339Nano's trailing-zero-trimmed
// width (which would place "...:00Z" before "...:00.5Z" only sometimes,
// depending on incidental trailing zeros).
func TestFormatTime_LexicalOrderMatchesTimeOrder(t *testing.T) {
	base := time.Date(2026, 9, 6, 8, 50, 42, 0, time.UTC)
	times := []time.Time{
		base,
		base.Add(1 * time.Nanosecond),
		base.Add(500 * time.Millisecond),
		base.Add(1 * time.Second),
		base.Add(1500 * time.Millisecond),
		base.Add(-1 * time.Second),
		base.Add(2 * time.Second),
	}

	encoded := make([]string, len(times))
	for i, tm := range times {
		encoded[i] = formatTime(tm)
	}

	wantOrder := append([]time.Time(nil), times...)
	sort.Slice(wantOrder, func(i, j int) bool { return wantOrder[i].Before(wantOrder[j]) })
	wantEncoded := make([]string, len(wantOrder))
	for i, tm := range wantOrder {
		wantEncoded[i] = formatTime(tm)
	}

	gotEncoded := append([]string(nil), encoded...)
	sort.Strings(gotEncoded)

	for i := range wantEncoded {
		if gotEncoded[i] != wantEncoded[i] {
			t.Fatalf("lexical sort disagrees with time sort at index %d: got %q, want %q", i, gotEncoded[i], wantEncoded[i])
		}
	}
}

func TestFormatTimeNull_ZeroTimeIsNull(t *testing.T) {
	if got := formatTimeNull(time.Time{}); got.Valid {
		t.Fatalf("formatTimeNull(zero) = %+v, want an invalid (NULL) NullString", got)
	}
	nonZero := time.Date(2026, 9, 6, 8, 50, 42, 821423717, time.UTC)
	got := formatTimeNull(nonZero)
	if !got.Valid || got.String != formatTime(nonZero) {
		t.Fatalf("formatTimeNull(nonzero) = %+v, want a valid NullString matching formatTime", got)
	}
}

func TestParseTimeNull_InvalidIsZeroTime(t *testing.T) {
	got, err := parseTimeNull(sql.NullString{})
	if err != nil {
		t.Fatalf("parseTimeNull(invalid): %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("parseTimeNull(invalid) = %v, want the zero Time", got)
	}
}
