package event

import "testing"

func TestNormalizeOrder(t *testing.T) {
	cases := []struct {
		in      string
		want    Order
		wantErr bool
	}{
		{"", OrderAsc, false},
		{"asc", OrderAsc, false},
		{"desc", OrderDesc, false},
		{"ASC", "", true},
		{"newest", "", true},
	}
	for _, c := range cases {
		got, err := NormalizeOrder(c.in)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("NormalizeOrder(%q) = (%q, %v), want (%q, err=%v)", c.in, got, err, c.want, c.wantErr)
		}
	}
}

func TestCursorEncodeDecodeRoundTrip(t *testing.T) {
	c := Cursor{V: CursorVersion, Off: 8192, Ord: OrderAsc, StreamID: "01JXGEN"}
	got, err := DecodeCursor(c.Encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != c {
		t.Fatalf("round trip = %+v, want %+v", got, c)
	}
	// Token is opaque (not human-readable plain text), but stable.
	if c.Encode() == "" {
		t.Fatalf("encode produced empty token")
	}
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	if _, err := DecodeCursor("not base64!!"); err == nil {
		t.Fatalf("expected error for non-base64 token")
	}
	if _, err := DecodeCursor("Zm9vYmFy"); err == nil { // base64 of "foobar", not JSON
		t.Fatalf("expected error for non-JSON token")
	}
}

func TestCursorValidate(t *testing.T) {
	const streamID = "01JXGEN"
	base := Cursor{V: CursorVersion, Off: 100, Ord: OrderAsc, StreamID: streamID}

	if err := base.Validate(OrderAsc, streamID); err != nil {
		t.Fatalf("matching cursor should validate: %v", err)
	}

	// order mismatch: an asc cursor used for a desc request.
	if err := base.Validate(OrderDesc, streamID); err == nil {
		t.Fatalf("expected order-mismatch error")
	}

	// stream mismatch: the session was recreated under the cursor.
	if err := base.Validate(OrderAsc, "01JXOTHER"); err == nil {
		t.Fatalf("expected stale-stream error")
	}

	// version mismatch: token predates a format change.
	old := Cursor{V: CursorVersion - 1, Off: 100, Ord: OrderAsc, StreamID: streamID}
	if err := old.Validate(OrderAsc, streamID); err == nil {
		t.Fatalf("expected version-mismatch error")
	}
}

func TestSubtreeCursorEncodeDecodeRoundTrip(t *testing.T) {
	c := SubtreeCursor{V: CursorVersion, Root: "workspace-1", After: "01JXEVENTID", Ord: OrderAsc}
	got, err := DecodeSubtreeCursor(c.Encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != c {
		t.Fatalf("round trip = %+v, want %+v", got, c)
	}
	if c.Encode() == "" {
		t.Fatalf("encode produced empty token")
	}
}

func TestSubtreeCursorValidate(t *testing.T) {
	const root = "workspace-1"
	base := SubtreeCursor{V: CursorVersion, Root: root, After: "01JX", Ord: OrderAsc}

	if err := base.Validate(root, OrderAsc); err != nil {
		t.Fatalf("matching cursor should validate: %v", err)
	}
	// root mismatch: a token must never be replayed against another subtree root.
	if err := base.Validate("workspace-2", OrderAsc); err == nil {
		t.Fatalf("expected root-mismatch error")
	}
	// order mismatch.
	if err := base.Validate(root, OrderDesc); err == nil {
		t.Fatalf("expected order-mismatch error")
	}
	// version mismatch.
	old := SubtreeCursor{V: CursorVersion - 1, Root: root, After: "01JX", Ord: OrderAsc}
	if err := old.Validate(root, OrderAsc); err == nil {
		t.Fatalf("expected version-mismatch error")
	}
}
