package adapter

import (
	"log/slog"
	"sync"

	"github.com/slack-go/slack"
)

// maxPendingStreamChunks bounds out-of-order buffering per stream_key: past
// this, a stalled or dropped chunk would otherwise buffer forever.
const maxPendingStreamChunks = 32

type Streamer interface {
	StartStream(channelID, threadTS, teamID, recipientUserID, text string) (ts string, err error)
	AppendStream(channelID, ts, text string) error
	StopStream(channelID, ts, text string) error
}

// StartStream seeds a streaming message; chat.startStream's undocumented
// markdown_text seeds the placeholder it creates (verified empirically).
func (a *Adapter) StartStream(channelID, threadTS, teamID, recipientUserID, text string) (string, error) {
	opts := []slack.MsgOption{slack.MsgOptionTS(threadTS)}
	if teamID != "" {
		opts = append(opts, slack.MsgOptionRecipientTeamID(teamID))
	}
	if recipientUserID != "" {
		opts = append(opts, slack.MsgOptionRecipientUserID(recipientUserID))
	}
	if text != "" {
		opts = append(opts, slack.MsgOptionMarkdownText(text))
	}
	_, ts, err := a.api.StartStream(channelID, opts...)
	return ts, err
}

// AppendStream appends one delta to an in-progress streaming message.
func (a *Adapter) AppendStream(channelID, ts, text string) error {
	_, _, err := a.api.AppendStream(channelID, ts, slack.MsgOptionMarkdownText(text))
	return err
}

// StopStream finalizes a streaming message. chat.stopStream's own
// markdown_text appends rather than replaces (verified empirically): text
// must be only the unposted remainder, never the full accumulated text.
func (a *Adapter) StopStream(channelID, ts, text string) error {
	var opts []slack.MsgOption
	if text != "" {
		opts = append(opts, slack.MsgOptionMarkdownText(text))
	}
	_, _, err := a.api.StopStream(channelID, ts, opts...)
	return err
}

type streamChunk struct {
	text  string
	final bool
}

// streamState is one stream_key's progress. failed is set only on a
// StartStream failure; a later Append/StopStream error is returned to the
// caller instead, since a native message exists by then and a fallback
// post alongside it would break "exactly one Slack thread message". Every
// field here is mutated only after the Slack (or fallback) call for the
// chunk it represents has actually succeeded — see Deliver — so a chunk
// whose call failed stays exactly as it was for a caller's retry.
type streamState struct {
	mu        sync.Mutex
	started   bool
	failed    bool
	ts        string
	nextIndex int64
	pending   map[int64]streamChunk
	text      string // committed fallback text; see applyFallback
}

// StreamManager renders one plect.message_delta sequence per stream_key as
// a single live-updating Slack message.
type StreamManager struct {
	streamer        Streamer
	poster          ThreadPoster
	teamID          string
	recipientUserID string
	logger          *slog.Logger

	mu    sync.Mutex
	state map[string]*streamState
}

func NewStreamManager(streamer Streamer, poster ThreadPoster, teamID, recipientUserID string, logger *slog.Logger) *StreamManager {
	return &StreamManager{
		streamer:        streamer,
		poster:          poster,
		teamID:          teamID,
		recipientUserID: recipientUserID,
		logger:          logger,
		state:           make(map[string]*streamState),
	}
}

// Deliver processes one chunk for streamKey, ordered by index. A chunk at
// or after st.nextIndex is (re-)buffered unconditionally, so redelivering
// the same index — a caller's retry after this returned an error — always
// re-attempts it; an index already behind st.nextIndex is a duplicate of
// an already-applied chunk and is dropped. Draining stops at the first
// failure, leaving that chunk (and anything after it) pending rather than
// skipping over or forgetting it, so a stream never silently completes
// short of its real content.
func (m *StreamManager) Deliver(channelID, threadTS, streamKey string, index int64, text string, final bool) error {
	st := m.stateFor(streamKey)

	st.mu.Lock()
	defer st.mu.Unlock()

	if index >= st.nextIndex {
		st.pending[index] = streamChunk{text: text, final: final}
	}

	for {
		idx, c, ok := nextChunk(st)
		if !ok {
			return nil
		}
		if err := m.apply(channelID, threadTS, st, c); err != nil {
			m.logger.Warn("stream delivery failed, will retry on redelivery",
				"component", "slack-adapter", "event", "stream_deliver_error",
				"stream_key", streamKey, "error", err)
			return err
		}
		delete(st.pending, idx)
		st.nextIndex = idx + 1
		if c.final {
			m.forget(streamKey)
			return nil
		}
	}
}

func (m *StreamManager) stateFor(streamKey string) *streamState {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.state[streamKey]
	if !ok {
		st = &streamState{pending: make(map[int64]streamChunk)}
		m.state[streamKey] = st
	}
	return st
}

func (m *StreamManager) forget(streamKey string) {
	m.mu.Lock()
	delete(m.state, streamKey)
	m.mu.Unlock()
}

// nextChunk selects the next candidate for apply: the chunk at
// st.nextIndex if buffered, or, once the gap ahead of it has stalled past
// maxPendingStreamChunks, the lowest buffered index instead. It never
// mutates st — only a successful apply (via Deliver) removes a chunk or
// advances st.nextIndex, which is what makes a failed chunk retryable.
func nextChunk(st *streamState) (int64, streamChunk, bool) {
	if c, ok := st.pending[st.nextIndex]; ok {
		return st.nextIndex, c, true
	}
	if len(st.pending) < maxPendingStreamChunks {
		return 0, streamChunk{}, false
	}
	lowest := int64(-1)
	for idx := range st.pending {
		if lowest == -1 || idx < lowest {
			lowest = idx
		}
	}
	return lowest, st.pending[lowest], true
}

// apply performs the Slack call(s) for one chunk. Caller holds st.mu.
func (m *StreamManager) apply(channelID, threadTS string, st *streamState, c streamChunk) error {
	if st.failed {
		return m.applyFallback(channelID, threadTS, st, c)
	}

	if !st.started {
		ts, err := m.streamer.StartStream(channelID, threadTS, m.teamID, m.recipientUserID, c.text)
		if err != nil {
			st.failed = true
			return m.applyFallback(channelID, threadTS, st, c)
		}
		st.started = true
		st.ts = ts
		if !c.final {
			return nil
		}
		// The seed text above already carries this chunk (see StopStream).
		return m.streamer.StopStream(channelID, ts, "")
	}

	if c.final {
		return m.streamer.StopStream(channelID, st.ts, c.text)
	}
	return m.streamer.AppendStream(channelID, st.ts, c.text)
}

// applyFallback buffers c into the fallback text and, on final, posts it
// once. The merge is a local value until PostToThread actually succeeds:
// a failed post leaves st.text unchanged, so retrying the same final chunk
// recomputes the identical merge instead of appending c.text a second time.
func (m *StreamManager) applyFallback(channelID, threadTS string, st *streamState, c streamChunk) error {
	merged := st.text + c.text
	if !c.final {
		st.text = merged
		return nil
	}
	if _, err := m.poster.PostToThread(channelID, threadTS, merged); err != nil {
		return err
	}
	st.text = merged
	return nil
}
