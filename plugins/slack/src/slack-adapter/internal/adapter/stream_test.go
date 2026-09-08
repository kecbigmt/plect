package adapter

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// recordingStreamer records StartStream/AppendStream/StopStream calls and
// lets tests inject a standing failure for each, cleared by setting the
// field back to nil to simulate a transient error clearing on retry.
type recordingStreamer struct {
	mu sync.Mutex

	startCalls  []startCall
	appendCalls []appendCall
	stopCalls   []stopCall

	startErr  error
	appendErr error
	stopErr   error

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
	return f.appendErr
}

func (f *recordingStreamer) StopStream(channelID, ts, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls = append(f.stopCalls, stopCall{channelID, ts, text})
	return f.stopErr
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

// TestStreamManager_AppendFailure_PreservesChunkForRetry is the regression
// case for a review finding: nextIndex/pending used to advance before the
// Slack call was attempted, so a failed AppendStream silently dropped that
// chunk and a caller's retry of the same index returned success without
// resending it.
func TestStreamManager_AppendFailure_PreservesChunkForRetry(t *testing.T) {
	streamer := &recordingStreamer{}
	poster := &recordingPoster{}
	mgr := NewStreamManager(streamer, poster, "T1", "U1", testLogger())

	if err := mgr.Deliver("C1", "111.0", "msg-1", 0, "Hello", false); err != nil {
		t.Fatalf("chunk 0: %v", err)
	}

	streamer.appendErr = errors.New("temporary network error")
	if err := mgr.Deliver("C1", "111.0", "msg-1", 1, ", world", false); err == nil {
		t.Fatal("Deliver should surface the append failure, not silently succeed")
	}
	if len(streamer.appendCalls) != 1 {
		t.Fatalf("AppendStream calls = %d, want 1 (the failed attempt)", len(streamer.appendCalls))
	}

	streamer.appendErr = nil
	if err := mgr.Deliver("C1", "111.0", "msg-1", 1, ", world", false); err != nil {
		t.Fatalf("retry of chunk 1: %v", err)
	}
	if len(streamer.appendCalls) != 2 {
		t.Fatalf("AppendStream calls = %d, want 2 (the retry resent the same chunk)", len(streamer.appendCalls))
	}

	if err := mgr.Deliver("C1", "111.0", "msg-1", 2, "!", true); err != nil {
		t.Fatalf("chunk 2 (final): %v", err)
	}
	if len(streamer.stopCalls) != 1 || streamer.stopCalls[0].text != "!" {
		t.Fatalf("StopStream calls = %+v, want one call with '!'", streamer.stopCalls)
	}
}

// TestStreamManager_FallbackPostFailure_RetriesWithoutDuplicatingText is the
// regression case for a review finding: Deliver forgot the stream_key as
// soon as it saw a final chunk, regardless of whether the fallback post
// actually succeeded, so a failed PostToThread on final was never retried.
func TestStreamManager_FallbackPostFailure_RetriesWithoutDuplicatingText(t *testing.T) {
	streamer := &recordingStreamer{startErr: errors.New("streaming not enabled for this app")}
	poster := &recordingPoster{}
	mgr := NewStreamManager(streamer, poster, "T1", "U1", testLogger())

	if err := mgr.Deliver("C1", "111.0", "msg-1", 0, "Hello", false); err != nil {
		t.Fatalf("chunk 0: %v", err)
	}

	poster.postErr = errors.New("temporary network error")
	if err := mgr.Deliver("C1", "111.0", "msg-1", 1, ", world!", true); err == nil {
		t.Fatal("Deliver should surface the fallback post failure, not silently succeed")
	}
	if len(poster.calls) != 1 {
		t.Fatalf("PostToThread attempts = %d, want 1 (the failed attempt)", len(poster.calls))
	}
	if got := len(mgr.state); got != 1 {
		t.Fatalf("stream state entries = %d, want 1 (kept for retry, not forgotten on failure)", got)
	}

	poster.postErr = nil
	if err := mgr.Deliver("C1", "111.0", "msg-1", 1, ", world!", true); err != nil {
		t.Fatalf("retry of the final chunk: %v", err)
	}
	if len(poster.calls) != 2 {
		t.Fatalf("PostToThread attempts = %d, want 2", len(poster.calls))
	}
	if got, want := poster.calls[1].Text, "Hello, world!"; got != want {
		t.Errorf("retried fallback post text = %q, want %q (not duplicated)", got, want)
	}
	if got := len(mgr.state); got != 0 {
		t.Errorf("stream state entries = %d, want 0 after the retry succeeds", got)
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
