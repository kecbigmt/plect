package adapter

import (
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/slack-go/slack"
)

// maxPendingStreamChunks bounds out-of-order buffering per stream_key: past
// this, a stalled or dropped chunk would otherwise buffer forever.
const maxPendingStreamChunks = 32

// Streamer performs one native Slack streaming message's lifecycle.
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
// post alongside it would break "exactly one Slack thread message".
type streamState struct {
	mu        sync.Mutex
	started   bool
	failed    bool
	ts        string
	nextIndex int64
	pending   map[int64]streamChunk
	text      strings.Builder
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

// Deliver processes one chunk for streamKey, ordered by index.
func (m *StreamManager) Deliver(channelID, threadTS, streamKey string, index int64, text string, final bool) error {
	st := m.stateFor(streamKey)

	st.mu.Lock()
	defer st.mu.Unlock()

	st.pending[index] = streamChunk{text: text, final: final}
	ready := drainReady(st)

	var firstErr error
	done := false
	for _, c := range ready {
		if err := m.apply(channelID, threadTS, st, c); err != nil {
			m.logger.Warn("stream delivery failed",
				"component", "slack-adapter", "event", "stream_deliver_error",
				"stream_key", streamKey, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
		if c.final {
			done = true
		}
	}
	if done {
		m.forget(streamKey)
	}
	return firstErr
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

// drainReady returns the ordered run starting at st.nextIndex, or, once the
// buffer bound is hit, gives up on the gap and flushes everything buffered.
func drainReady(st *streamState) []streamChunk {
	var ready []streamChunk
	for {
		c, ok := st.pending[st.nextIndex]
		if !ok {
			break
		}
		delete(st.pending, st.nextIndex)
		ready = append(ready, c)
		st.nextIndex++
		if c.final {
			return ready
		}
	}
	if len(st.pending) >= maxPendingStreamChunks {
		ready = append(ready, flushPending(st)...)
	}
	return ready
}

func flushPending(st *streamState) []streamChunk {
	indices := make([]int64, 0, len(st.pending))
	for idx := range st.pending {
		indices = append(indices, idx)
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })

	var flushed []streamChunk
	for _, idx := range indices {
		c := st.pending[idx]
		delete(st.pending, idx)
		flushed = append(flushed, c)
		st.nextIndex = idx + 1
		if c.final {
			break
		}
	}
	return flushed
}

// apply performs the Slack call(s) for one already-ordered chunk.
func (m *StreamManager) apply(channelID, threadTS string, st *streamState, c streamChunk) error {
	st.text.WriteString(c.text)

	if st.failed {
		if c.final {
			return m.postFallback(channelID, threadTS, st)
		}
		return nil
	}

	if !st.started {
		ts, err := m.streamer.StartStream(channelID, threadTS, m.teamID, m.recipientUserID, c.text)
		if err != nil {
			st.failed = true
			if c.final {
				return m.postFallback(channelID, threadTS, st)
			}
			return nil
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

func (m *StreamManager) postFallback(channelID, threadTS string, st *streamState) error {
	_, err := m.poster.PostToThread(channelID, threadTS, st.text.String())
	return err
}
