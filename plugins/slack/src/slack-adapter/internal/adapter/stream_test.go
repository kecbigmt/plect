package adapter

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// recordingStreamer records StartStream/AppendStream/StopStream calls and
// lets tests inject a failure for the next StartStream call.
type recordingStreamer struct {
	mu sync.Mutex

	startCalls  []startCall
	appendCalls []appendCall
	stopCalls   []stopCall

	startErr error

	nextTS int
}

type startCall struct {
	channelID, threadTS, teamID, recipientUserID, text string
}

type appendCall struct {
	channelID, ts, text string
}

type stopCall struct {
	channelID, ts, text string
}

func (f *recordingStreamer) StartStream(channelID, threadTS, teamID, recipientUserID, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls = append(f.startCalls, startCall{channelID, threadTS, teamID, recipientUserID, text})
	if f.startErr != nil {
		return "", f.startErr
	}
	f.nextTS++
	return fmt.Sprintf("ts-%d", f.nextTS), nil
}

func (f *recordingStreamer) AppendStream(channelID, ts, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appendCalls = append(f.appendCalls, appendCall{channelID, ts, text})
	return nil
}

func (f *recordingStreamer) StopStream(channelID, ts, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls = append(f.stopCalls, stopCall{channelID, ts, text})
	return nil
}

func TestStreamManager_InOrderChunks_StartsAppendsAndStops(t *testing.T) {
	streamer := &recordingStreamer{}
	poster := &recordingPoster{}
	mgr := NewStreamManager(streamer, poster, "T1", "U1", testLogger())

	if err := mgr.Deliver("C1", "111.0", "msg-1", 0, "Hello", false); err != nil {
		t.Fatalf("chunk 0: %v", err)
	}
	if err := mgr.Deliver("C1", "111.0", "msg-1", 1, ", world", false); err != nil {
		t.Fatalf("chunk 1: %v", err)
	}
	if err := mgr.Deliver("C1", "111.0", "msg-1", 2, "!", true); err != nil {
		t.Fatalf("chunk 2 (final): %v", err)
	}

	if len(streamer.startCalls) != 1 {
		t.Fatalf("StartStream calls = %d, want 1", len(streamer.startCalls))
	}
	start := streamer.startCalls[0]
	if start.channelID != "C1" || start.threadTS != "111.0" || start.teamID != "T1" || start.recipientUserID != "U1" || start.text != "Hello" {
		t.Errorf("StartStream call = %+v, want C1/111.0/T1/U1/Hello", start)
	}

	if len(streamer.appendCalls) != 1 {
		t.Fatalf("AppendStream calls = %d, want 1", len(streamer.appendCalls))
	}
	if got := streamer.appendCalls[0]; got.text != ", world" {
		t.Errorf("AppendStream text = %q, want %q", got.text, ", world")
	}

	if len(streamer.stopCalls) != 1 {
		t.Fatalf("StopStream calls = %d, want 1", len(streamer.stopCalls))
	}
	if got := streamer.stopCalls[0]; got.text != "!" {
		t.Errorf("StopStream text = %q, want %q (only the final delta)", got.text, "!")
	}

	if len(poster.calls) != 0 {
		t.Errorf("PostToThread calls = %d, want 0 (native streaming succeeded)", len(poster.calls))
	}
}

func TestStreamManager_SingleChunkFinal_SeedsStartThenStopsWithNoFurtherText(t *testing.T) {
	streamer := &recordingStreamer{}
	poster := &recordingPoster{}
	mgr := NewStreamManager(streamer, poster, "T1", "U1", testLogger())

	if err := mgr.Deliver("C1", "111.0", "msg-1", 0, "Hello", true); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	if len(streamer.startCalls) != 1 || streamer.startCalls[0].text != "Hello" {
		t.Fatalf("StartStream calls = %+v, want one call seeded with Hello", streamer.startCalls)
	}
	if len(streamer.stopCalls) != 1 || streamer.stopCalls[0].text != "" {
		t.Fatalf("StopStream calls = %+v, want one call with empty text (avoid double-posting the seed)", streamer.stopCalls)
	}
	if len(streamer.appendCalls) != 0 {
		t.Errorf("AppendStream calls = %d, want 0", len(streamer.appendCalls))
	}
}

func TestStreamManager_OutOfOrderChunks_BufferUntilGapCloses(t *testing.T) {
	streamer := &recordingStreamer{}
	poster := &recordingPoster{}
	mgr := NewStreamManager(streamer, poster, "T1", "U1", testLogger())

	// index 1 arrives before index 0: nothing should reach Slack yet.
	if err := mgr.Deliver("C1", "111.0", "msg-1", 1, "world", false); err != nil {
		t.Fatalf("chunk 1: %v", err)
	}
	if len(streamer.startCalls) != 0 || len(streamer.appendCalls) != 0 {
		t.Fatalf("out-of-order chunk reached Slack before its gap closed: start=%v append=%v",
			streamer.startCalls, streamer.appendCalls)
	}

	// index 0 arrives: both should now flush in order.
	if err := mgr.Deliver("C1", "111.0", "msg-1", 0, "hello ", false); err != nil {
		t.Fatalf("chunk 0: %v", err)
	}
	if len(streamer.startCalls) != 1 || streamer.startCalls[0].text != "hello " {
		t.Fatalf("StartStream calls = %+v, want one call seeded with 'hello '", streamer.startCalls)
	}
	if len(streamer.appendCalls) != 1 || streamer.appendCalls[0].text != "world" {
		t.Fatalf("AppendStream calls = %+v, want one call appending 'world'", streamer.appendCalls)
	}
}

func TestStreamManager_BufferBoundExceeded_FlushesDespiteGap(t *testing.T) {
	streamer := &recordingStreamer{}
	poster := &recordingPoster{}
	mgr := NewStreamManager(streamer, poster, "T1", "U1", testLogger())

	// index 0 never arrives. Deliver chunks 1..maxPendingStreamChunks so the
	// buffer bound is hit and the manager gives up waiting on the gap.
	for i := int64(1); i <= maxPendingStreamChunks; i++ {
		if err := mgr.Deliver("C1", "111.0", "msg-1", i, "x", false); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}

	if len(streamer.startCalls) != 1 {
		t.Fatalf("StartStream calls = %d, want 1 (flush proceeded despite the missing index 0)", len(streamer.startCalls))
	}
	// The flushed run starts at index 1 (the lowest buffered index), one
	// chunk becomes the seed and the rest become appends.
	if got, want := len(streamer.appendCalls), maxPendingStreamChunks-1; got != want {
		t.Fatalf("AppendStream calls = %d, want %d", got, want)
	}
}

func TestStreamManager_StartFailure_FallsBackToOnePostOnFinal(t *testing.T) {
	streamer := &recordingStreamer{startErr: errors.New("streaming not enabled for this app")}
	poster := &recordingPoster{}
	mgr := NewStreamManager(streamer, poster, "T1", "U1", testLogger())

	if err := mgr.Deliver("C1", "111.0", "msg-1", 0, "Hello", false); err != nil {
		t.Fatalf("chunk 0: %v", err)
	}
	if err := mgr.Deliver("C1", "111.0", "msg-1", 1, ", world", false); err != nil {
		t.Fatalf("chunk 1: %v", err)
	}
	if len(poster.calls) != 0 {
		t.Fatalf("PostToThread calls = %d, want 0 before final", len(poster.calls))
	}

	if err := mgr.Deliver("C1", "111.0", "msg-1", 2, "!", true); err != nil {
		t.Fatalf("chunk 2 (final): %v", err)
	}

	if len(streamer.appendCalls) != 0 || len(streamer.stopCalls) != 0 {
		t.Errorf("AppendStream/StopStream should never be called once StartStream failed: append=%v stop=%v",
			streamer.appendCalls, streamer.stopCalls)
	}
	if len(poster.calls) != 1 {
		t.Fatalf("PostToThread calls = %d, want 1", len(poster.calls))
	}
	if got, want := poster.calls[0].Text, "Hello, world!"; got != want {
		t.Errorf("fallback post text = %q, want %q (full accumulated text)", got, want)
	}
}

func TestStreamManager_ForgetsStateAfterFinal(t *testing.T) {
	streamer := &recordingStreamer{}
	poster := &recordingPoster{}
	mgr := NewStreamManager(streamer, poster, "T1", "U1", testLogger())

	if err := mgr.Deliver("C1", "111.0", "msg-1", 0, "Hello", true); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if got := len(mgr.state); got != 0 {
		t.Errorf("stream state entries = %d, want 0 after final", got)
	}

	// A later message reusing the same stream_key starts fresh.
	if err := mgr.Deliver("C1", "111.0", "msg-1", 0, "Second message", true); err != nil {
		t.Fatalf("Deliver (second message): %v", err)
	}
	if len(streamer.startCalls) != 2 {
		t.Fatalf("StartStream calls = %d, want 2 (reused stream_key started a new message)", len(streamer.startCalls))
	}
}
