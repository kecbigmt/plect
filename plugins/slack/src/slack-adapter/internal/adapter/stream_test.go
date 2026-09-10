package adapter

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

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

	if err := mgr.Deliver("C1", "111.0", "msg-1", 1, "world", false); err != nil {
		t.Fatalf("chunk 1: %v", err)
	}
	if len(streamer.startCalls) != 0 || len(streamer.appendCalls) != 0 {
		t.Fatalf("out-of-order chunk reached Slack before its gap closed: start=%v append=%v",
			streamer.startCalls, streamer.appendCalls)
	}

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

	for i := int64(1); i <= maxPendingStreamChunks; i++ {
		if err := mgr.Deliver("C1", "111.0", "msg-1", i, "x", false); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}

	if len(streamer.startCalls) != 1 {
		t.Fatalf("StartStream calls = %d, want 1 (flush proceeded despite the missing index 0)", len(streamer.startCalls))
	}
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

func TestStreamManager_StopFailure_PreservesFinalChunkForRetryWithoutRestarting(t *testing.T) {
	streamer := &recordingStreamer{}
	poster := &recordingPoster{}
	mgr := NewStreamManager(streamer, poster, "T1", "U1", testLogger())

	if err := mgr.Deliver("C1", "111.0", "msg-1", 0, "Hello", false); err != nil {
		t.Fatalf("chunk 0: %v", err)
	}

	streamer.stopErr = errors.New("temporary network error")
	if err := mgr.Deliver("C1", "111.0", "msg-1", 1, "!", true); err == nil {
		t.Fatal("Deliver should surface the stop failure, not silently succeed")
	}
	if len(streamer.stopCalls) != 1 {
		t.Fatalf("StopStream calls = %d, want 1 (the failed attempt)", len(streamer.stopCalls))
	}
	if got := len(mgr.state); got != 1 {
		t.Fatalf("stream state entries = %d, want 1 (kept for retry, not forgotten on failure)", got)
	}

	streamer.stopErr = nil
	if err := mgr.Deliver("C1", "111.0", "msg-1", 1, "!", true); err != nil {
		t.Fatalf("retry of the final chunk: %v", err)
	}
	if len(streamer.startCalls) != 1 {
		t.Fatalf("StartStream calls = %d, want 1 (the retry must not start a second stream)", len(streamer.startCalls))
	}
	if len(streamer.stopCalls) != 2 || streamer.stopCalls[1].text != "!" {
		t.Fatalf("StopStream calls = %+v, want a second call with '!'", streamer.stopCalls)
	}
	if got := len(mgr.state); got != 0 {
		t.Errorf("stream state entries = %d, want 0 after the retry succeeds", got)
	}
}

func TestStreamManager_ForgetsLiveStateAfterFinal(t *testing.T) {
	streamer := &recordingStreamer{}
	poster := &recordingPoster{}
	mgr := NewStreamManager(streamer, poster, "T1", "U1", testLogger())

	if err := mgr.Deliver("C1", "111.0", "msg-1", 0, "Hello", true); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if got := len(mgr.state); got != 0 {
		t.Errorf("stream state entries = %d, want 0 after final", got)
	}
}

// A message_id is unique within a session, so a repeat delivery to the same
// thread after it already finalized is always a duplicate.
func TestStreamManager_DuplicateAfterFinal_IsDroppedNotReposted(t *testing.T) {
	streamer := &recordingStreamer{}
	poster := &recordingPoster{}
	mgr := NewStreamManager(streamer, poster, "T1", "U1", testLogger())

	if err := mgr.Deliver("C1", "111.0", "msg-1", 0, "Hello", true); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if err := mgr.Deliver("C1", "111.0", "msg-1", 0, "Hello", true); err != nil {
		t.Fatalf("Deliver (duplicate): %v", err)
	}
	if len(streamer.startCalls) != 1 {
		t.Fatalf("StartStream calls = %d, want 1 (the duplicate must not start a second message)", len(streamer.startCalls))
	}
	if len(streamer.stopCalls) != 1 {
		t.Fatalf("StopStream calls = %d, want 1", len(streamer.stopCalls))
	}
}

// The same duplicate-drop applies to the fallback-post path: a workspace
// that can't stream still must show exactly one message per message_id.
func TestStreamManager_DuplicateAfterFallbackFinal_IsDroppedNotReposted(t *testing.T) {
	streamer := &recordingStreamer{startErr: errors.New("streaming not enabled for this app")}
	poster := &recordingPoster{}
	mgr := NewStreamManager(streamer, poster, "T1", "U1", testLogger())

	if err := mgr.Deliver("C1", "111.0", "msg-1", 0, "Hello", true); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if err := mgr.Deliver("C1", "111.0", "msg-1", 0, "Hello", true); err != nil {
		t.Fatalf("Deliver (duplicate): %v", err)
	}
	if len(poster.calls) != 1 {
		t.Fatalf("PostToThread calls = %d, want 1 (the duplicate must not post a second message)", len(poster.calls))
	}
}

func TestStreamManager_IdenticalStreamKeysInDifferentThreadsRemainIndependent(t *testing.T) {
	streamer := &recordingStreamer{}
	poster := &recordingPoster{}
	mgr := NewStreamManager(streamer, poster, "T1", "U1", testLogger())

	if err := mgr.Deliver("C1", "111.0", "msg-1", 0, "first", false); err != nil {
		t.Fatalf("first thread's initial chunk: %v", err)
	}
	if err := mgr.Deliver("C1", "222.0", "msg-1", 0, "second", false); err != nil {
		t.Fatalf("second thread's initial chunk: %v", err)
	}
	if err := mgr.Deliver("C1", "111.0", "msg-1", 1, " one", true); err != nil {
		t.Fatalf("first thread's final chunk: %v", err)
	}
	if err := mgr.Deliver("C1", "222.0", "msg-1", 1, " two", true); err != nil {
		t.Fatalf("second thread's final chunk: %v", err)
	}

	if got := len(streamer.startCalls); got != 2 {
		t.Fatalf("StartStream calls = %d, want 2", got)
	}
	if streamer.startCalls[0].threadTS != "111.0" || streamer.startCalls[0].text != "first" {
		t.Errorf("first StartStream call = %+v, want first thread's initial text", streamer.startCalls[0])
	}
	if streamer.startCalls[1].threadTS != "222.0" || streamer.startCalls[1].text != "second" {
		t.Errorf("second StartStream call = %+v, want second thread's initial text", streamer.startCalls[1])
	}
	if got := len(streamer.stopCalls); got != 2 {
		t.Fatalf("StopStream calls = %d, want 2", got)
	}
	if streamer.stopCalls[0].ts != "ts-1" || streamer.stopCalls[0].text != " one" {
		t.Errorf("first StopStream call = %+v, want first thread's final text", streamer.stopCalls[0])
	}
	if streamer.stopCalls[1].ts != "ts-2" || streamer.stopCalls[1].text != " two" {
		t.Errorf("second StopStream call = %+v, want second thread's final text", streamer.stopCalls[1])
	}
}

func TestStreamManager_RestartCompletesAndSuppressesTrailingMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "streams.json")
	streamer := &recordingStreamer{}
	poster := &recordingPoster{}
	beforeRestart := NewStreamManagerWithStatePath(streamer, poster, "T1", "U1", testLogger(), path)

	if err := beforeRestart.Deliver("C1", "111.0", "msg-1", 0, "Hello", false); err != nil {
		t.Fatalf("initial chunk: %v", err)
	}

	afterRestart := NewStreamManagerWithStatePath(streamer, poster, "T1", "U1", testLogger(), path)
	if err := afterRestart.Deliver("C1", "111.0", "msg-1", 1, ", world", true); err != nil {
		t.Fatalf("final chunk after restart: %v", err)
	}
	if err := afterRestart.Deliver("C1", "111.0", "msg-1", 0, "Hello, world", true); err != nil {
		t.Fatalf("trailing message after restart: %v", err)
	}

	if got := len(streamer.startCalls); got != 1 {
		t.Fatalf("StartStream calls = %d, want 1 (restart must resume the existing message)", got)
	}
	if got := len(streamer.stopCalls); got != 1 {
		t.Fatalf("StopStream calls = %d, want 1", got)
	}
	if got := streamer.stopCalls[0]; got.ts != "ts-1" || got.text != ", world" {
		t.Errorf("StopStream call = %+v, want existing stream ts with final text", got)
	}
	if got := len(poster.calls); got != 0 {
		t.Errorf("PostToThread calls = %d, want 0", got)
	}
}

// Regression coverage for stateOrFinalized's atomicity: run with `-race`.
func TestStreamManager_ConcurrentDeliverToSameKey_PostsExactlyOnce(t *testing.T) {
	streamer := &recordingStreamer{}
	poster := &recordingPoster{}
	mgr := NewStreamManager(streamer, poster, "T1", "U1", testLogger())

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			if err := mgr.Deliver("C1", "111.0", "msg-race", 0, "Hello", true); err != nil {
				t.Errorf("Deliver: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := len(streamer.startCalls); got != 1 {
		t.Fatalf("StartStream calls = %d, want 1 (exactly one Slack message per message_id)", got)
	}
	if got := len(streamer.stopCalls); got != 1 {
		t.Fatalf("StopStream calls = %d, want 1", got)
	}
}
