package event

import (
	"strconv"
	"strings"
)

// EncodeResumeToken and ParseResumeToken implement the event bus's internal
// SSE resume-id wire format "<streamID>:<seq>": pairing a stream's own
// sequence with the incarnation it is scoped to. A session recreate mints a
// stream whose sequence numbering restarts at 0, so a bare sequence alone
// cannot tell two incarnations apart — this is the internal protocol between
// the bus and its subscribers (Client, and a webui relay), never the
// browser-facing opaque Cursor.
func EncodeResumeToken(streamID string, seq int64) string {
	return streamID + ":" + strconv.FormatInt(seq, 10)
}

// ParseResumeToken parses a token written by EncodeResumeToken. ok is false
// for anything else (empty, malformed, or a pre-rotation bare integer), which
// callers treat as "no prior position" rather than guessing a stream.
func ParseResumeToken(v string) (streamID string, seq int64, ok bool) {
	id, seqStr, found := strings.Cut(v, ":")
	if !found {
		return "", 0, false
	}
	n, err := strconv.ParseInt(seqStr, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return id, n, true
}
