package event

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// CursorVersion is the current opaque-cursor format version. A token carrying a
// different version is rejected as expired: the format moved on and the token's
// fields can no longer be trusted.
//
// Version 2 (current): Off is a per-stream sequence number (SQLite-backed
// event log). Version 1 (retired): Off was a byte offset into a JSONL log
// file. A v1 token is rejected the same way any other version mismatch is —
// Validate never reinterprets its Off as a sequence, since the two numbers
// are not comparable.
const CursorVersion = 2

// Order is the listing direction for a page of events.
type Order string

const (
	OrderAsc  Order = "asc"  // oldest first; supports forward pagination via next_cursor
	OrderDesc Order = "desc" // newest first; v1 returns only the most recent page
)

// NormalizeOrder maps "" to the default (asc) and rejects unknown values.
func NormalizeOrder(s string) (Order, error) {
	switch Order(s) {
	case "", OrderAsc:
		return OrderAsc, nil
	case OrderDesc:
		return OrderDesc, nil
	default:
		return "", fmt.Errorf("invalid order %q: want asc or desc", s)
	}
}

// Cursor is the decoded form of an opaque pagination token. Clients never
// construct or interpret it — they pass a prior page's next_cursor back
// verbatim. It is a keyset position (a per-stream sequence number) wrapped
// with the context needed to detect misuse: the order it was issued for and
// the stream it came from. A raw sequence number has neither, so a stale or
// cross-order cursor would silently point at the wrong record; the wrapping
// lets the server reject it instead.
type Cursor struct {
	V        int    `json:"v"`         // format version; must equal CursorVersion
	Off      int64  `json:"off"`       // exclusive per-stream sequence number (keyset position)
	Ord      Order  `json:"ord"`       // order this cursor was issued for
	StreamID string `json:"stream_id"` // event stream id at issue time
}

// Encode renders the cursor as an opaque base64url(JSON) token.
func (c Cursor) Encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor parses a token produced by Cursor.Encode.
func DecodeCursor(token string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Cursor{}, fmt.Errorf("invalid cursor: %w", err)
	}
	var c Cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return Cursor{}, fmt.Errorf("invalid cursor: %w", err)
	}
	return c, nil
}

// Validate reports whether c may be used for a request in order ord against
// the stream identified by streamID. A version or stream-id mismatch means
// the cursor predates a format change or was issued for a session that has
// since been recreated under the same name, and must be discarded (restart
// from the beginning). An order mismatch means the caller asked for a
// different direction than the cursor was issued for.
func (c Cursor) Validate(ord Order, streamID string) error {
	if c.V != CursorVersion {
		return fmt.Errorf("cursor expired (version %d, want %d); restart from the beginning", c.V, CursorVersion)
	}
	if c.StreamID != streamID {
		return fmt.Errorf("cursor expired (stale event stream); restart from the beginning")
	}
	if c.Ord != ord {
		return fmt.Errorf("cursor was issued for order %q but request asks for %q", c.Ord, ord)
	}
	return nil
}

// SubtreeCursor is a ULID keyset position for paging the events of one session
// subtree (a root session plus its descendants) merged across their logs. It
// carries the root it was issued for. No generation: ordering is by id, immune
// to any one log's byte layout.
type SubtreeCursor struct {
	V     int    `json:"v"`   // format version; must equal CursorVersion
	Root  string `json:"r"`   // root session this cursor was issued for
	After string `json:"a"`   // last event id (ULID) returned; the next page starts after it
	Ord   Order  `json:"ord"` // order this cursor was issued for
}

// Encode renders the subtree cursor as an opaque base64url(JSON) token.
func (c SubtreeCursor) Encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeSubtreeCursor parses a token produced by SubtreeCursor.Encode.
func DecodeSubtreeCursor(token string) (SubtreeCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return SubtreeCursor{}, fmt.Errorf("invalid cursor: %w", err)
	}
	var c SubtreeCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return SubtreeCursor{}, fmt.Errorf("invalid cursor: %w", err)
	}
	return c, nil
}

// Validate reports whether c may be used for a request over the subtree rooted
// at root in order ord. A root mismatch means the token was issued for a
// different subtree and must never cross roots.
func (c SubtreeCursor) Validate(root string, ord Order) error {
	if c.V != CursorVersion {
		return fmt.Errorf("cursor expired (version %d, want %d); restart from the beginning", c.V, CursorVersion)
	}
	if c.Root != root {
		return fmt.Errorf("cursor was issued for a different subtree root; restart from the beginning")
	}
	if c.Ord != ord {
		return fmt.Errorf("cursor was issued for order %q but request asks for %q", c.Ord, ord)
	}
	return nil
}
