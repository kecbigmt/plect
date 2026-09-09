package adapter

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/contracts/channel-protocol"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// withFastSubscribeRetry tightens the /subscribe connect retry loop so tests
// that exercise the unreachable-socket path don't take seconds. Returns a
// cleanup that restores the previous values.
func withFastSubscribeRetry(t *testing.T, attempts int, interval time.Duration) {
	t.Helper()
	origAttempts := subscribeConnectAttempts
	origInterval := subscribeConnectInterval
	subscribeConnectAttempts = attempts
	subscribeConnectInterval = interval
	t.Cleanup(func() {
		subscribeConnectAttempts = origAttempts
		subscribeConnectInterval = origInterval
	})
}

// startTestListener spins up a channel-server-shaped Unix socket listener at
// a temp path and returns the path. The listener accepts connections but does
// not respond — enough to make net.Dial succeed for the connect step.
func startTestListener(t *testing.T) string {
	t.Helper()
	path, _ := startCapturingListener(t)
	return path
}

// startCapturingListener returns the socket path plus a channel that
// receives MessagePayload envelopes the listener observes. RegisterPayload
// envelopes (sent on connect) are ignored. Buffer is 8; tests reading
// fewer than that won't deadlock the listener goroutine.
func startCapturingListener(t *testing.T) (string, <-chan protocol.MessagePayload) {
	t.Helper()
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "test.sock")
	ch := make(chan protocol.MessagePayload, 8)
	listener, err := newFakeSocketListener(socketPath, func(env protocol.Envelope, _ net.Conn) {
		if env.Type != protocol.MsgMessage {
			return
		}
		var msg protocol.MessagePayload
		if err := env.UnmarshalPayload(&msg); err != nil {
			return
		}
		select {
		case ch <- msg:
		default:
		}
	}, testLogger())
	if err != nil {
		t.Fatalf("NewSocketListener: %v", err)
	}
	go listener.Serve()
	t.Cleanup(func() { listener.Close() })
	return socketPath, ch
}

// recordingPoster captures PostToThread and SetThreadStatus calls so tests
// can verify the framing the broker produces for /notify (emoji prefix +
// summary) and the shimmer status set/cleared on a thread.
type recordingPoster struct {
	calls       []recordedPost
	statusCalls []recordedStatus
	statusErr   error
	postErr     error
}

type recordedPost struct {
	ChannelID, ThreadTS, Text string
}

type recordedStatus struct {
	ChannelID, ThreadTS, Status string
	LoadingMessages             []string
}

func (p *recordingPoster) PostToThread(channelID, threadTS, text string) (string, error) {
	p.calls = append(p.calls, recordedPost{channelID, threadTS, text})
	if p.postErr != nil {
		return "", p.postErr
	}
	return "ts-" + threadTS, nil
}

func (p *recordingPoster) SetThreadStatus(channelID, threadTS, status string, loadingMessages []string) error {
	p.statusCalls = append(p.statusCalls, recordedStatus{channelID, threadTS, status, loadingMessages})
	return p.statusErr
}

type recordingThreader struct {
	calls []recordedThread
}

type recordedThread struct {
	ChannelID, Text string
}

func (t *recordingThreader) CreateThread(channelID, text string) (string, string, error) {
	t.calls = append(t.calls, recordedThread{ChannelID: channelID, Text: text})
	return "1234.5678", "https://example.slack.com/archives/C123/p12345678", nil
}

func newTestAdapter(cfg *Config) *Adapter {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &Adapter{
		cfg:    cfg,
		broker: NewBroker("", logger),
		poster: &recordingPoster{},
		logger: logger,
	}
	a.statusManager = NewStatusManager(a.poster, cfg.StatusTTLDuration(), logger)
	a.streamManager = NewStreamManager(&recordingStreamer{}, a.poster, "T-team", cfg.StreamRecipientUserID(), logger)
	a.socketPool = NewSocketPool(a.poster, logger, nil, a.statusManager)
	a.mentions = newMentionStream()
	return a
}

func TestCreateThreadFetchesSlackPermalink(t *testing.T) {
	var sawPost, sawPermalink bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/chat.postMessage":
			sawPost = true
			if err := req.ParseForm(); err != nil {
				t.Errorf("ParseForm: %v", err)
			}
			if got := req.PostForm.Get("channel"); got != "C-review" {
				t.Errorf("postMessage channel = %q, want C-review", got)
			}
			if got := req.PostForm.Get("text"); got != "root text" {
				t.Errorf("postMessage text = %q, want root text", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true,"channel":"C-review","ts":"1234.5678"}`))
		case "/chat.getPermalink":
			sawPermalink = true
			if got := req.URL.Query().Get("channel"); got != "C-review" {
				t.Errorf("getPermalink channel = %q, want C-review", got)
			}
			if got := req.URL.Query().Get("message_ts"); got != "1234.5678" {
				t.Errorf("getPermalink message_ts = %q, want 1234.5678", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true,"channel":"C-review","permalink":"https://example.slack.com/archives/C-review/p12345678"}`))
		default:
			t.Errorf("unexpected Slack API path %s", req.URL.Path)
			http.NotFound(w, req)
		}
	}))
	defer server.Close()

	a := &Adapter{api: slack.New("xoxb-test", slack.OptionAPIURL(server.URL+"/"))}
	threadTS, permalink, err := a.CreateThread("C-review", "root text")
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if threadTS != "1234.5678" {
		t.Errorf("threadTS = %q, want 1234.5678", threadTS)
	}
	if permalink != "https://example.slack.com/archives/C-review/p12345678" {
		t.Errorf("permalink = %q, want Slack permalink", permalink)
	}
	if !sawPost || !sawPermalink {
		t.Fatalf("Slack API calls: post=%v permalink=%v, want both", sawPost, sawPermalink)
	}
}

func TestHandleCreateThread_UsesRequestChannelAndReturnsPermalink(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C-default"})
	threader := &recordingThreader{}
	a.threader = threader

	body, _ := json.Marshal(createThreadRequest{
		ChannelID: "C-review",
		Text:      "[AI review] Fix widgets — https://github.com/acme/widgets/pull/7",
	})
	req := httptest.NewRequest(http.MethodPost, "/threads", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleCreateThread(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if len(threader.calls) != 1 {
		t.Fatalf("CreateThread calls = %d, want 1", len(threader.calls))
	}
	if got := threader.calls[0].ChannelID; got != "C-review" {
		t.Errorf("CreateThread channel_id = %q, want C-review", got)
	}

	var resp createThreadResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ThreadTS != "1234.5678" {
		t.Errorf("thread_ts = %q, want 1234.5678", resp.ThreadTS)
	}
	if resp.ChannelID != "C-review" {
		t.Errorf("channel_id = %q, want C-review", resp.ChannelID)
	}
	if resp.Permalink != "https://example.slack.com/archives/C123/p12345678" {
		t.Errorf("permalink = %q, want Slack permalink", resp.Permalink)
	}
}

func TestHandleCreateThread_FallsBackToConfiguredChannel(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C-default"})
	threader := &recordingThreader{}
	a.threader = threader

	body, _ := json.Marshal(createThreadRequest{Text: "hello"})
	req := httptest.NewRequest(http.MethodPost, "/threads", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleCreateThread(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if got := threader.calls[0].ChannelID; got != "C-default" {
		t.Errorf("CreateThread channel_id = %q, want C-default", got)
	}
}

func TestHandleCreateThread_RejectsMissingChannelIDWithoutDefault(t *testing.T) {
	a := newTestAdapter(&Config{})
	a.threader = &recordingThreader{}

	body, _ := json.Marshal(createThreadRequest{Text: "hello"})
	req := httptest.NewRequest(http.MethodPost, "/threads", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleCreateThread(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleInfo(t *testing.T) {
	cfg := &Config{ChannelID: "C12345"}
	a := &Adapter{cfg: cfg, workspace: "exampleorg"}

	req := httptest.NewRequest(http.MethodGet, "/info", nil)
	w := httptest.NewRecorder()
	a.HandleInfo(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusOK)
	}

	var resp infoResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Workspace != "exampleorg" {
		t.Errorf("got workspace %q, want %q", resp.Workspace, "exampleorg")
	}
	if resp.ChannelID != "C12345" {
		t.Errorf("got channel_id %q, want %q", resp.ChannelID, "C12345")
	}
}

func TestHandleInfo_EmptyWorkspace(t *testing.T) {
	cfg := &Config{ChannelID: "C99999"}
	a := &Adapter{cfg: cfg}

	req := httptest.NewRequest(http.MethodGet, "/info", nil)
	w := httptest.NewRecorder()
	a.HandleInfo(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusOK)
	}

	var resp infoResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Workspace != "" {
		t.Errorf("got workspace %q, want empty", resp.Workspace)
	}
	if resp.ChannelID != "C99999" {
		t.Errorf("got channel_id %q, want %q", resp.ChannelID, "C99999")
	}
}

func TestHandlePostMessage_MentionLogic(t *testing.T) {
	tests := []struct {
		name          string
		notifyUserIDs []string
		mention       bool
		inputText     string
		wantText      string
	}{
		{
			name:          "mention false, text unchanged",
			notifyUserIDs: []string{"U111"},
			mention:       false,
			inputText:     "hello",
			wantText:      "hello",
		},
		{
			name:          "mention true with configured users",
			notifyUserIDs: []string{"U111"},
			mention:       true,
			inputText:     "hello",
			wantText:      "<@U111> hello",
		},
		{
			name:          "mention true but no users configured",
			notifyUserIDs: nil,
			mention:       true,
			inputText:     "hello",
			wantText:      "hello",
		},
		{
			name:          "mention true with multiple users",
			notifyUserIDs: []string{"U111", "U222"},
			mention:       true,
			inputText:     "done",
			wantText:      "<@U111> <@U222> done",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{NotifyUserIDs: tt.notifyUserIDs}
			text := tt.inputText
			if tt.mention {
				text = cfg.MentionPrefix() + text
			}
			if text != tt.wantText {
				t.Errorf("got %q, want %q", text, tt.wantText)
			}
		})
	}
}

func TestHandlePostMessage_MethodNotAllowed(t *testing.T) {
	cfg := &Config{ChannelID: "C12345"}
	a := &Adapter{cfg: cfg}

	req := httptest.NewRequest(http.MethodGet, "/messages", nil)
	w := httptest.NewRecorder()
	a.HandlePostMessage(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("got status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandlePostMessage_InvalidJSON(t *testing.T) {
	cfg := &Config{ChannelID: "C12345"}
	a := &Adapter{cfg: cfg}

	req := httptest.NewRequest(http.MethodPost, "/messages", bytes.NewBufferString("not json"))
	w := httptest.NewRecorder()
	a.HandlePostMessage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandlePostMessage_MissingFields(t *testing.T) {
	cfg := &Config{ChannelID: "C12345"}
	a := &Adapter{cfg: cfg}

	body, _ := json.Marshal(postMessageRequest{Text: ""})
	req := httptest.NewRequest(http.MethodPost, "/messages", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandlePostMessage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandlePostMessage_RejectsMissingChannelIDWithoutDefault(t *testing.T) {
	a := newTestAdapter(&Config{})

	body, _ := json.Marshal(postMessageRequest{ThreadTS: "1111.000", Text: "done"})
	req := httptest.NewRequest(http.MethodPost, "/messages", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandlePostMessage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleSubscribe_PostAddsSubscriberWhenSocketReady(t *testing.T) {
	socketPath := startTestListener(t)
	a := newTestAdapter(&Config{ChannelID: "C0"})

	body, _ := json.Marshal(subscribeRequest{
		ThreadTS:   "1700000010.100000",
		ChannelID:  "C123",
		SocketPath: socketPath,
	})
	req := httptest.NewRequest(http.MethodPost, "/subscribe", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleSubscribe(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var got Subscriber
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ThreadTS != "1700000010.100000" || got.ChannelID != "C123" {
		t.Fatalf("unexpected response: %+v", got)
	}
	if got.Since.IsZero() {
		t.Fatalf("Since should be populated")
	}
	if _, ok := a.broker.Find("1700000010.100000"); !ok {
		t.Fatalf("broker should hold the subscription")
	}
}

func TestHandleSubscribe_PostRejectsWhenSocketUnreachable(t *testing.T) {
	withFastSubscribeRetry(t, 3, 10*time.Millisecond)
	a := newTestAdapter(&Config{ChannelID: "C0"})

	body, _ := json.Marshal(subscribeRequest{
		ThreadTS:   "1700000011.100000",
		ChannelID:  "C123",
		SocketPath: filepath.Join(t.TempDir(), "absent.sock"),
	})
	req := httptest.NewRequest(http.MethodPost, "/subscribe", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleSubscribe(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if _, ok := a.broker.Find("1700000011.100000"); ok {
		t.Fatalf("broker should NOT hold a subscription when pre-connect failed")
	}
}

// If the channel-server socket comes up partway through the retry window,
// /subscribe should eventually succeed and register the subscriber.
func TestHandleSubscribe_PostRetriesUntilSocketReady(t *testing.T) {
	withFastSubscribeRetry(t, 50, 20*time.Millisecond)

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "deferred.sock")

	a := newTestAdapter(&Config{ChannelID: "C0"})

	go func() {
		time.Sleep(80 * time.Millisecond)
		listener, err := newFakeSocketListener(socketPath, func(env protocol.Envelope, conn net.Conn) {}, testLogger())
		if err != nil {
			t.Errorf("NewSocketListener: %v", err)
			return
		}
		go listener.Serve()
		t.Cleanup(func() { listener.Close() })
	}()

	body, _ := json.Marshal(subscribeRequest{
		ThreadTS:   "1700000012.100000",
		ChannelID:  "C123",
		SocketPath: socketPath,
	})
	req := httptest.NewRequest(http.MethodPost, "/subscribe", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleSubscribe(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if _, ok := a.broker.Find("1700000012.100000"); !ok {
		t.Fatalf("broker should hold the subscription after retry succeeded")
	}
}

// A second POST /subscribe naming the same socket_path (the exact shape
// that let a reply misroute to the channel) must not just replace the
// broker's map entry — the live channel-server connection has to be
// rebound to the new thread_ts too, or it would keep replying to the
// thread the connection was originally opened for.
func TestHandleSubscribe_SecondSubscribeOnSameSocketRebindsLiveConnection(t *testing.T) {
	var mu sync.Mutex
	registeredThreadTS := map[net.Conn]string{}

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "test.sock")
	listener, err := newFakeSocketListener(socketPath, func(env protocol.Envelope, conn net.Conn) {
		if env.Type != protocol.MsgRegister {
			return
		}
		var reg protocol.RegisterPayload
		if err := env.UnmarshalPayload(&reg); err != nil {
			return
		}
		mu.Lock()
		registeredThreadTS[conn] = reg.ThreadTS
		mu.Unlock()
	}, testLogger())
	if err != nil {
		t.Fatalf("NewSocketListener: %v", err)
	}
	defer listener.Close()
	go listener.Serve()

	a := newTestAdapter(&Config{ChannelID: "C0"})

	firstBody, _ := json.Marshal(subscribeRequest{
		ThreadTS:   "1111111111.100000",
		ChannelID:  "C-OLD",
		SocketPath: socketPath,
	})
	req := httptest.NewRequest(http.MethodPost, "/subscribe", bytes.NewBuffer(firstBody))
	w := httptest.NewRecorder()
	a.HandleSubscribe(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first subscribe: got status %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	secondBody, _ := json.Marshal(subscribeRequest{
		ThreadTS:   "2222222222.200000",
		ChannelID:  "C-NEW",
		SocketPath: socketPath,
	})
	req2 := httptest.NewRequest(http.MethodPost, "/subscribe", bytes.NewBuffer(secondBody))
	w2 := httptest.NewRecorder()
	a.HandleSubscribe(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("second subscribe: got status %d, want %d, body=%s", w2.Code, http.StatusOK, w2.Body.String())
	}

	// The listener processes each Register asynchronously, off the request
	// goroutine that already returned 200; wait for the rebind's Register
	// specifically, not just any Register, since the first one can still be
	// in flight when the second write lands.
	sawRebind := func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, ts := range registeredThreadTS {
			if ts == "2222222222.200000" {
				return true
			}
		}
		return false
	}
	deadline := time.After(2 * time.Second)
	for !sawRebind() {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for the second subscribe's Register to land")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(registeredThreadTS) != 1 {
		t.Fatalf("expected the connection to be reused, got %d distinct connections registered", len(registeredThreadTS))
	}
	for _, ts := range registeredThreadTS {
		if ts != "2222222222.200000" {
			t.Fatalf("live connection still registered for %q, want the second subscription's thread_ts", ts)
		}
	}
	if _, ok := a.broker.Find("1111111111.100000"); ok {
		t.Fatalf("stale broker entry should have been replaced")
	}
}

func TestHandleSubscribe_CatchUpThroughDeliversHistoryOnce(t *testing.T) {
	socketPath := startTestListener(t)
	a := newTestAdapter(&Config{ChannelID: "C0"})
	fetcher := &fakeThreadFetcher{
		messages: []slack.Message{
			slackMessage("1700000001.000001", "U-root", "Review thread opened"),
			slackMessage("1700000001.000002", "U-alice", "Looks ready."),
			slackMessage("1700000001.000003", "U-bob", "<@U-bot> please act on this"),
		},
		names: map[string]string{"U-root": "Plecture", "U-alice": "Alice", "U-bob": "Bob"},
	}
	a.threadFetcher = fetcher
	publisher := &recordingEventPublisher{}
	a.eventPublisher = publisher

	body, _ := json.Marshal(subscribeRequest{
		ThreadTS:       "1700000001.000001",
		ChannelID:      "C-review",
		SocketPath:     socketPath,
		SessionName:    "owner/repo-1",
		CatchUpThrough: "1700000001.000003",
	})
	req := httptest.NewRequest(http.MethodPost, "/subscribe", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleSubscribe(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if len(publisher.events) != 1 {
		t.Fatalf("published events = %d, want 1", len(publisher.events))
	}
	ev := publisher.events[0]
	if ev.SessionName != "owner/repo-1" || ev.Type != "user.emit" || ev.Source != "slack" || ev.Direction != "inbound" {
		t.Fatalf("unexpected event envelope: %+v", ev)
	}
	assertBodyContainsInOrder(t, ev.Body, []string{
		"Plecture: Review thread opened",
		"Alice: Looks ready.",
		"Bob: <@U-bot> please act on this",
	})

	var resp Subscriber
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.DeliveredThrough != "1700000001.000003" {
		t.Errorf("response delivered_through = %q, want 1700000001.000003", resp.DeliveredThrough)
	}
	sub, _ := a.broker.Find("1700000001.000001")
	if sub.DeliveredThrough != "1700000001.000003" {
		t.Errorf("broker DeliveredThrough = %q, want 1700000001.000003", sub.DeliveredThrough)
	}
}

// Models a runtime restart re-running the run-scoped slack_subscribe task
// with the same catch_up_through.
func TestHandleSubscribe_CatchUpThroughIsIdempotentAcrossResubscribe(t *testing.T) {
	socketPath := startTestListener(t)
	a := newTestAdapter(&Config{ChannelID: "C0"})
	fetcher := &fakeThreadFetcher{
		messages: []slack.Message{
			slackMessage("1700000001.000001", "U-root", "Review thread opened"),
			slackMessage("1700000001.000003", "U-bob", "<@U-bot> please act on this"),
		},
		names: map[string]string{"U-root": "Plecture", "U-bob": "Bob"},
	}
	a.threadFetcher = fetcher
	publisher := &recordingEventPublisher{}
	a.eventPublisher = publisher

	subscribeBody, _ := json.Marshal(subscribeRequest{
		ThreadTS:       "1700000001.000001",
		ChannelID:      "C-review",
		SocketPath:     socketPath,
		SessionName:    "owner/repo-1",
		CatchUpThrough: "1700000001.000003",
	})

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/subscribe", bytes.NewBuffer(subscribeBody))
		w := httptest.NewRecorder()
		a.HandleSubscribe(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("attempt %d: got status %d, want %d, body=%s", i, w.Code, http.StatusOK, w.Body.String())
		}
	}

	if len(publisher.events) != 1 {
		t.Fatalf("published events = %d, want 1 (second subscribe must not redeliver)", len(publisher.events))
	}
}

func TestHandleSubscribe_CatchUpThroughThenLaterMentionDeliversOnlyDelta(t *testing.T) {
	socketPath := startTestListener(t)
	a := newTestAdapter(&Config{ChannelID: "C0"})
	fetcher := &fakeThreadFetcher{
		messages: []slack.Message{
			slackMessage("1700000001.000001", "U-root", "Review thread opened"),
			slackMessage("1700000001.000003", "U-bob", "<@U-bot> please act on this"),
			slackMessage("1700000001.000006", "U-alice", "new reply"),
		},
		names: map[string]string{"U-root": "Plecture", "U-bob": "Bob", "U-alice": "Alice", "U-dana": "Dana"},
	}
	a.threadFetcher = fetcher
	publisher := &recordingEventPublisher{}
	a.eventPublisher = publisher

	subscribeBody, _ := json.Marshal(subscribeRequest{
		ThreadTS:       "1700000001.000001",
		ChannelID:      "C-review",
		SocketPath:     socketPath,
		SessionName:    "owner/repo-1",
		CatchUpThrough: "1700000001.000003",
	})
	req := httptest.NewRequest(http.MethodPost, "/subscribe", bytes.NewBuffer(subscribeBody))
	w := httptest.NewRecorder()
	a.HandleSubscribe(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if len(publisher.events) != 1 {
		t.Fatalf("published events after catch-up = %d, want 1", len(publisher.events))
	}

	a.handleAppMention(&slackevents.AppMentionEvent{
		User:            "U-dana",
		Text:            "<@U-bot> send the update",
		TimeStamp:       "1700000001.000008",
		ThreadTimeStamp: "1700000001.000001",
		Channel:         "C-review",
	})

	if len(publisher.events) != 2 {
		t.Fatalf("published events after mention = %d, want 2", len(publisher.events))
	}
	// The root re-sends on every delivery (selectDeliberationMessages'
	// existing behavior); only the already-caught-up reply must not repeat.
	deltaBody := publisher.events[1].Body
	if strings.Contains(deltaBody, "please act on this") {
		t.Fatalf("delta body should not redeliver the caught-up reply, got:\n%s", deltaBody)
	}
	assertBodyContainsInOrder(t, deltaBody, []string{
		"Plecture: Review thread opened",
		"Alice: new reply",
		"Dana: <@U-bot> send the update",
	})
}

func TestHandleSubscribe_CatchUpThroughOmittedIsUnchanged(t *testing.T) {
	socketPath := startTestListener(t)
	a := newTestAdapter(&Config{ChannelID: "C0"})
	fetcher := &fakeThreadFetcher{}
	a.threadFetcher = fetcher
	publisher := &recordingEventPublisher{}
	a.eventPublisher = publisher

	body, _ := json.Marshal(subscribeRequest{
		ThreadTS:    "1700000001.000001",
		ChannelID:   "C-review",
		SocketPath:  socketPath,
		SessionName: "owner/repo-1",
	})
	req := httptest.NewRequest(http.MethodPost, "/subscribe", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleSubscribe(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if fetcher.calls != 0 {
		t.Errorf("fetchThreadReplies calls = %d, want 0", fetcher.calls)
	}
	if len(publisher.events) != 0 {
		t.Errorf("published events = %d, want 0", len(publisher.events))
	}
	sub, _ := a.broker.Find("1700000001.000001")
	if sub.DeliveredThrough != "" {
		t.Errorf("DeliveredThrough = %q, want empty", sub.DeliveredThrough)
	}
}

// Models a clean plect down / up (or an ECS task replacement doing the same).
func TestHandleSubscribe_UnsubscribeThenResubscribeSameSessionRestoresWatermark(t *testing.T) {
	socketPath := startTestListener(t)
	a := newTestAdapter(&Config{ChannelID: "C0"})
	fetcher := &fakeThreadFetcher{
		messages: []slack.Message{
			slackMessage("1700000001.000001", "U-root", "Review thread opened"),
			slackMessage("1700000001.000003", "U-bob", "<@U-bot> please act on this"),
		},
		names: map[string]string{"U-root": "Plecture", "U-bob": "Bob"},
	}
	a.threadFetcher = fetcher
	publisher := &recordingEventPublisher{}
	a.eventPublisher = publisher

	subscribeBody, _ := json.Marshal(subscribeRequest{
		ThreadTS:       "1700000001.000001",
		ChannelID:      "C-review",
		SocketPath:     socketPath,
		SessionName:    "owner/repo-1",
		CatchUpThrough: "1700000001.000003",
	})
	req := httptest.NewRequest(http.MethodPost, "/subscribe", bytes.NewBuffer(subscribeBody))
	w := httptest.NewRecorder()
	a.HandleSubscribe(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("initial subscribe: got status %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if len(publisher.events) != 1 {
		t.Fatalf("published events after initial subscribe = %d, want 1", len(publisher.events))
	}

	delReq := httptest.NewRequest(http.MethodDelete, "/subscribe?thread_ts=1700000001.000001", nil)
	delW := httptest.NewRecorder()
	a.HandleSubscribe(delW, delReq)
	if delW.Code != http.StatusNoContent {
		t.Fatalf("unsubscribe: got status %d, want %d", delW.Code, http.StatusNoContent)
	}
	if _, ok := a.broker.Find("1700000001.000001"); ok {
		t.Fatalf("subscription should be removed after unsubscribe")
	}

	req2 := httptest.NewRequest(http.MethodPost, "/subscribe", bytes.NewBuffer(subscribeBody))
	w2 := httptest.NewRecorder()
	a.HandleSubscribe(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("resubscribe: got status %d, want %d, body=%s", w2.Code, http.StatusOK, w2.Body.String())
	}

	var resp Subscriber
	if err := json.NewDecoder(w2.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.DeliveredThrough != "1700000001.000003" {
		t.Errorf("resubscribe response delivered_through = %q, want restored 1700000001.000003", resp.DeliveredThrough)
	}
	if len(publisher.events) != 1 {
		t.Fatalf("published events after resubscribe = %d, want still 1 (no redelivery)", len(publisher.events))
	}
}

// Models plect destroy followed by a fresh plect up.
func TestHandleSubscribe_UnsubscribeThenResubscribeDifferentSessionDeliversFullHistory(t *testing.T) {
	socketPath := startTestListener(t)
	a := newTestAdapter(&Config{ChannelID: "C0"})
	fetcher := &fakeThreadFetcher{
		messages: []slack.Message{
			slackMessage("1700000001.000001", "U-root", "Review thread opened"),
			slackMessage("1700000001.000003", "U-bob", "<@U-bot> please act on this"),
		},
		names: map[string]string{"U-root": "Plecture", "U-bob": "Bob"},
	}
	a.threadFetcher = fetcher
	publisher := &recordingEventPublisher{}
	a.eventPublisher = publisher

	firstBody, _ := json.Marshal(subscribeRequest{
		ThreadTS:       "1700000001.000001",
		ChannelID:      "C-review",
		SocketPath:     socketPath,
		SessionName:    "owner/repo-1",
		CatchUpThrough: "1700000001.000003",
	})
	req := httptest.NewRequest(http.MethodPost, "/subscribe", bytes.NewBuffer(firstBody))
	w := httptest.NewRecorder()
	a.HandleSubscribe(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("initial subscribe: got status %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if len(publisher.events) != 1 {
		t.Fatalf("published events after initial subscribe = %d, want 1", len(publisher.events))
	}

	delReq := httptest.NewRequest(http.MethodDelete, "/subscribe?thread_ts=1700000001.000001", nil)
	delW := httptest.NewRecorder()
	a.HandleSubscribe(delW, delReq)
	if delW.Code != http.StatusNoContent {
		t.Fatalf("unsubscribe: got status %d, want %d", delW.Code, http.StatusNoContent)
	}

	secondBody, _ := json.Marshal(subscribeRequest{
		ThreadTS:       "1700000001.000001",
		ChannelID:      "C-review",
		SocketPath:     socketPath,
		SessionName:    "owner/repo-2",
		CatchUpThrough: "1700000001.000003",
	})
	req2 := httptest.NewRequest(http.MethodPost, "/subscribe", bytes.NewBuffer(secondBody))
	w2 := httptest.NewRecorder()
	a.HandleSubscribe(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("resubscribe: got status %d, want %d, body=%s", w2.Code, http.StatusOK, w2.Body.String())
	}

	var resp Subscriber
	if err := json.NewDecoder(w2.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.DeliveredThrough != "1700000001.000003" {
		t.Errorf("resubscribe response delivered_through = %q, want 1700000001.000003", resp.DeliveredThrough)
	}
	if len(publisher.events) != 2 {
		t.Fatalf("published events after different-session resubscribe = %d, want 2 (full history redelivered)", len(publisher.events))
	}
	if publisher.events[1].SessionName != "owner/repo-2" {
		t.Errorf("second delivery session_name = %q, want owner/repo-2", publisher.events[1].SessionName)
	}
}

func TestHandleSubscribe_PostRejectsMissingFields(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})

	body, _ := json.Marshal(subscribeRequest{ThreadTS: "1111.000"})
	req := httptest.NewRequest(http.MethodPost, "/subscribe", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleSubscribe(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleSubscribe_PostRejectsInvalidJSON(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})

	req := httptest.NewRequest(http.MethodPost, "/subscribe", bytes.NewBufferString("not json"))
	w := httptest.NewRecorder()
	a.HandleSubscribe(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// A malformed thread_ts can never match a real thread; persisting it turns
// a bug into a permanent misroute instead of a retriable delivery failure.
func TestHandleSubscribe_PostRejectsMalformedThreadTS(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})

	body, _ := json.Marshal(subscribeRequest{
		ThreadTS:   "1111.000",
		ChannelID:  "C123",
		SocketPath: "/run/x.sock",
	})
	req := httptest.NewRequest(http.MethodPost, "/subscribe", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleSubscribe(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d, body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if _, ok := a.broker.Find("1111.000"); ok {
		t.Fatalf("malformed thread_ts must not be persisted")
	}
}

func TestHandleSubscribe_PostRejectsMalformedCatchUpThrough(t *testing.T) {
	socketPath := startTestListener(t)
	a := newTestAdapter(&Config{ChannelID: "C0"})

	body, _ := json.Marshal(subscribeRequest{
		ThreadTS:       "1700000020.100000",
		ChannelID:      "C123",
		SocketPath:     socketPath,
		CatchUpThrough: "not-a-timestamp",
	})
	req := httptest.NewRequest(http.MethodPost, "/subscribe", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleSubscribe(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d, body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if _, ok := a.broker.Find("1700000020.100000"); ok {
		t.Fatalf("subscribe with a malformed catch_up_through must not be persisted")
	}
}

func TestHandleSubscribe_DeleteRemovesSubscriber(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})
	a.broker.Subscribe(Subscriber{ThreadTS: "1111.000", ChannelID: "C", SocketPath: "/x"})

	req := httptest.NewRequest(http.MethodDelete, "/subscribe?thread_ts=1111.000", nil)
	w := httptest.NewRecorder()
	a.HandleSubscribe(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusNoContent)
	}
	if _, ok := a.broker.Find("1111.000"); ok {
		t.Fatalf("subscription should be removed")
	}
}

func TestHandleSubscribe_DeleteMissingThreadTSIsBadRequest(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})

	req := httptest.NewRequest(http.MethodDelete, "/subscribe", nil)
	w := httptest.NewRecorder()
	a.HandleSubscribe(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleSubscribe_DeleteUnknownIsIdempotent(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})

	req := httptest.NewRequest(http.MethodDelete, "/subscribe?thread_ts=9999", nil)
	w := httptest.NewRecorder()
	a.HandleSubscribe(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusNoContent)
	}
}

func TestHandleSubscribe_DeleteBySessionRemovesEveryRegistration(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})
	a.broker.Subscribe(Subscriber{ThreadTS: "1111111111.100000+op.100000+ops_chat", ChannelID: "C", SocketPath: "/a", SessionName: "owner/repo-1"})
	a.broker.Subscribe(Subscriber{ThreadTS: "1111111111.100000", ChannelID: "C", SocketPath: "/b", SessionName: "owner/repo-1"})
	a.broker.Subscribe(Subscriber{ThreadTS: "2222222222.200000", ChannelID: "C", SocketPath: "/c", SessionName: "owner/repo-2"})

	req := httptest.NewRequest(http.MethodDelete, "/subscribe?session_name=owner/repo-1", nil)
	w := httptest.NewRecorder()
	a.HandleSubscribe(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusNoContent)
	}
	if _, ok := a.broker.BySession("owner/repo-1"); ok {
		t.Fatalf("every registration for the session should be removed")
	}
	if _, ok := a.broker.BySession("owner/repo-2"); !ok {
		t.Fatalf("other sessions' registrations must be untouched")
	}
}

func TestHandleSubscribe_DeleteBySessionUnknownIsIdempotent(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})

	req := httptest.NewRequest(http.MethodDelete, "/subscribe?session_name=unknown", nil)
	w := httptest.NewRecorder()
	a.HandleSubscribe(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusNoContent)
	}
}

func TestHandleSubscribe_MethodNotAllowed(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})

	req := httptest.NewRequest(http.MethodGet, "/subscribe", nil)
	w := httptest.NewRecorder()
	a.HandleSubscribe(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleSubscribers_ListsCurrent(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})
	a.broker.Subscribe(Subscriber{ThreadTS: "a", ChannelID: "C1", SocketPath: "/a"})
	a.broker.Subscribe(Subscriber{ThreadTS: "b", ChannelID: "C2", SocketPath: "/b"})

	req := httptest.NewRequest(http.MethodGet, "/subscribers", nil)
	w := httptest.NewRecorder()
	a.HandleSubscribers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusOK)
	}
	var subs []Subscriber
	if err := json.NewDecoder(w.Body).Decode(&subs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("subscribers length = %d, want 2", len(subs))
	}
}

func TestHandleSubscribe_PostPersistsSessionName(t *testing.T) {
	socketPath := startTestListener(t)
	a := newTestAdapter(&Config{ChannelID: "C0"})

	body, _ := json.Marshal(subscribeRequest{
		ThreadTS:    "1700000013.100000",
		ChannelID:   "C123",
		SocketPath:  socketPath,
		SessionName: "owner/repo-1",
	})
	req := httptest.NewRequest(http.MethodPost, "/subscribe", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleSubscribe(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	sub, ok := a.broker.BySession("owner/repo-1")
	if !ok {
		t.Fatalf("BySession lookup failed after subscribe")
	}
	if sub.ThreadTS != "1700000013.100000" {
		t.Errorf("ThreadTS = %q, want 1700000013.100000", sub.ThreadTS)
	}
}

// TestHandleNotify_DeliversToBothChannels verifies the routing actually
// produces the right framing on both wires: channel-server receives
// "[GitHub <type>] <url>: <summary>" and Slack gets "<emoji> <summary>".
func TestHandleNotify_DeliversToBothChannels(t *testing.T) {
	socketPath, msgs := startCapturingListener(t)
	a := newTestAdapter(&Config{ChannelID: "C0"})
	poster := a.poster.(*recordingPoster)

	a.broker.Subscribe(Subscriber{
		ThreadTS:    "1111.000",
		ChannelID:   "C123",
		SocketPath:  socketPath,
		SessionName: "owner/repo-1",
	})

	body, _ := json.Marshal(notifyRequest{
		SessionName:         "owner/repo-1",
		ChangeType:          "ci_status",
		Summary:             "CI status changed: PENDING -> SUCCESS",
		URL:                 "https://example.com/pr/1",
		NotifyChannelServer: true,
		NotifySlack:         true,
	})
	req := httptest.NewRequest(http.MethodPost, "/notify", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleNotify(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp notifyResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.ChannelServerDelivered || !resp.SlackDelivered {
		t.Fatalf("delivery flags = %+v, want both true", resp)
	}

	wantChannelText := "[GitHub ci_status] https://example.com/pr/1: CI status changed: PENDING -> SUCCESS"
	select {
	case msg := <-msgs:
		if msg.Text != wantChannelText {
			t.Errorf("channel-server text = %q, want %q", msg.Text, wantChannelText)
		}
		if msg.ThreadTS != "1111.000" {
			t.Errorf("channel-server thread_ts = %q, want 1111.000", msg.ThreadTS)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("channel-server never received the notification")
	}

	if len(poster.calls) != 1 {
		t.Fatalf("expected 1 Slack post, got %d", len(poster.calls))
	}
	wantSlackText := ":white_check_mark: CI status changed: PENDING -> SUCCESS"
	if poster.calls[0].Text != wantSlackText {
		t.Errorf("slack text = %q, want %q", poster.calls[0].Text, wantSlackText)
	}
	if poster.calls[0].ChannelID != "C123" || poster.calls[0].ThreadTS != "1111.000" {
		t.Errorf("slack target = (%q,%q), want (C123,1111.000)", poster.calls[0].ChannelID, poster.calls[0].ThreadTS)
	}
}

// When notify_slack=true but notify_channel_server=false, only Slack
// should fire. Locks down the per-flag gating.
func TestHandleNotify_SlackOnly(t *testing.T) {
	socketPath, msgs := startCapturingListener(t)
	a := newTestAdapter(&Config{ChannelID: "C0"})
	poster := a.poster.(*recordingPoster)
	a.broker.Subscribe(Subscriber{
		ThreadTS: "1111.000", ChannelID: "C123",
		SocketPath: socketPath, SessionName: "s1",
	})

	body, _ := json.Marshal(notifyRequest{
		SessionName: "s1", ChangeType: "new_comments", Summary: "hi",
		NotifyChannelServer: false, NotifySlack: true,
	})
	req := httptest.NewRequest(http.MethodPost, "/notify", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleNotify(w, req)

	var resp notifyResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.ChannelServerDelivered {
		t.Errorf("expected ChannelServerDelivered=false")
	}
	if !resp.SlackDelivered {
		t.Errorf("expected SlackDelivered=true")
	}
	if len(poster.calls) != 1 {
		t.Errorf("expected 1 slack post, got %d", len(poster.calls))
	}
	select {
	case msg := <-msgs:
		t.Errorf("channel-server should not have received anything, got %+v", msg)
	case <-time.After(100 * time.Millisecond):
	}
}

// When the subscriber's socket disappeared between subscribe and notify,
// the broker must evict it (lazy GC) so future Slack→claude attempts
// don't queue against a dead socket. Mirrors the eviction the
// Slack-inbound handleMessage path performs.
func TestHandleNotify_MissingSocketEvictsSubscriber(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})
	a.broker.Subscribe(Subscriber{
		ThreadTS: "1111.000", ChannelID: "C123",
		SocketPath:  filepath.Join(t.TempDir(), "vanished.sock"),
		SessionName: "s1",
	})

	body, _ := json.Marshal(notifyRequest{
		SessionName: "s1", ChangeType: "state", Summary: "x",
		NotifyChannelServer: true, NotifySlack: false,
	})
	req := httptest.NewRequest(http.MethodPost, "/notify", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleNotify(w, req)

	var resp notifyResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.ChannelServerDelivered {
		t.Errorf("delivery should fail on missing socket")
	}
	if _, ok := a.broker.Find("1111.000"); ok {
		t.Errorf("subscriber should have been evicted")
	}
}

func TestHandleNotify_UnknownSessionReturnsReason(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})

	body, _ := json.Marshal(notifyRequest{
		SessionName:         "missing",
		ChangeType:          "state",
		Summary:             "x",
		NotifyChannelServer: true,
		NotifySlack:         true,
	})
	req := httptest.NewRequest(http.MethodPost, "/notify", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleNotify(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp notifyResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Reason == "" {
		t.Errorf("expected reason for unknown session, got empty")
	}
	if resp.ChannelServerDelivered || resp.SlackDelivered {
		t.Errorf("expected nothing delivered, got %+v", resp)
	}
}

func TestHandleNotify_RejectsMissingFields(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})

	cases := []notifyRequest{
		{Summary: "x"},     // no session_name
		{SessionName: "s"}, // no summary
	}
	for _, c := range cases {
		body, _ := json.Marshal(c)
		req := httptest.NewRequest(http.MethodPost, "/notify", bytes.NewBuffer(body))
		w := httptest.NewRecorder()
		a.HandleNotify(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body=%+v: status = %d, want 400", c, w.Code)
		}
	}
}

func TestHandleNotify_MethodNotAllowed(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})
	req := httptest.NewRequest(http.MethodGet, "/notify", nil)
	w := httptest.NewRecorder()
	a.HandleNotify(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestHandleSetStatus_NonEmptyStatusSetsWithoutPosting(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})
	poster := a.poster.(*recordingPoster)

	body, _ := json.Marshal(setStatusRequest{
		ChannelID:       "C123",
		ThreadTS:        "1111.000",
		Status:          "is reviewing…",
		LoadingMessages: []string{"Checking CI…"},
	})
	req := httptest.NewRequest(http.MethodPost, "/status", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleSetStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if len(poster.calls) != 0 {
		t.Errorf("PostToThread calls = %d, want 0 (status update must not post a message)", len(poster.calls))
	}
	if len(poster.statusCalls) != 2 {
		t.Fatalf("SetThreadStatus calls = %d, want 2 (clear, then show)", len(poster.statusCalls))
	}
	got := poster.statusCalls[1]
	if got.ChannelID != "C123" || got.ThreadTS != "1111.000" || got.Status != "is reviewing…" {
		t.Errorf("show call = %+v, want C123/1111.000/is reviewing…", got)
	}
	if len(got.LoadingMessages) != 1 || got.LoadingMessages[0] != "Checking CI…" {
		t.Errorf("loading_messages = %v, want [Checking CI…]", got.LoadingMessages)
	}
}

func TestHandleSetStatus_EmptyStatusClears(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})
	poster := a.poster.(*recordingPoster)

	body, _ := json.Marshal(setStatusRequest{ChannelID: "C123", ThreadTS: "1111.000", Status: ""})
	req := httptest.NewRequest(http.MethodPost, "/status", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleSetStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if len(poster.statusCalls) != 1 {
		t.Fatalf("SetThreadStatus calls = %d, want 1", len(poster.statusCalls))
	}
	if got := poster.statusCalls[0].Status; got != "" {
		t.Errorf("status = %q, want empty (clear)", got)
	}
}

func TestHandleSetStatus_FallsBackToConfiguredChannel(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C-default"})
	poster := a.poster.(*recordingPoster)

	body, _ := json.Marshal(setStatusRequest{ThreadTS: "1111.000", Status: "is thinking…"})
	req := httptest.NewRequest(http.MethodPost, "/status", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleSetStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if poster.statusCalls[0].ChannelID != "C-default" {
		t.Errorf("channel_id = %q, want C-default", poster.statusCalls[0].ChannelID)
	}
}

func TestHandleSetStatus_ClipsLongLoadingMessage(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})
	poster := a.poster.(*recordingPoster)

	long := strings.Repeat("x", 200)
	body, _ := json.Marshal(setStatusRequest{
		ThreadTS:        "1111.000",
		Status:          "is thinking…",
		LoadingMessages: []string{long},
	})
	req := httptest.NewRequest(http.MethodPost, "/status", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleSetStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	got := poster.statusCalls[len(poster.statusCalls)-1].LoadingMessages[0]
	if r := []rune(got); len(r) != maxLoadingMessageLen {
		t.Errorf("relayed loading message length = %d, want %d (clipped)", len(r), maxLoadingMessageLen)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("relayed loading message = %q, want an ellipsis suffix", got)
	}
}

func TestHandleSetStatus_ShortLoadingMessageUnchanged(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})
	poster := a.poster.(*recordingPoster)

	exactly48 := strings.Repeat("x", 48)
	body, _ := json.Marshal(setStatusRequest{
		ThreadTS:        "1111.000",
		Status:          "is thinking…",
		LoadingMessages: []string{exactly48},
	})
	req := httptest.NewRequest(http.MethodPost, "/status", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleSetStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	got := poster.statusCalls[len(poster.statusCalls)-1].LoadingMessages[0]
	if got != exactly48 {
		t.Errorf("relayed loading message = %q, want unchanged 48-char entry", got)
	}
}

func TestHandleSetStatus_SlackAPIErrorMapsTo422(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})
	poster := a.poster.(*recordingPoster)
	poster.statusErr = slack.SlackErrorResponse{Err: "invalid_arguments"}

	body, _ := json.Marshal(setStatusRequest{ThreadTS: "1111.000", Status: "is thinking…"})
	req := httptest.NewRequest(http.MethodPost, "/status", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleSetStatus(w, req)

	if w.Code != 422 {
		t.Fatalf("got status %d, want 422, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid_arguments") {
		t.Errorf("body = %q, want it to contain the slack error name", w.Body.String())
	}
}

func TestHandleSetStatus_RejectsMissingChannelIDWithoutDefault(t *testing.T) {
	a := newTestAdapter(&Config{})

	body, _ := json.Marshal(setStatusRequest{ThreadTS: "1111.000", Status: "is thinking…"})
	req := httptest.NewRequest(http.MethodPost, "/status", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleSetStatus(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleSetStatus_RejectsMissingThreadTS(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})

	body, _ := json.Marshal(setStatusRequest{Status: "is thinking…"})
	req := httptest.NewRequest(http.MethodPost, "/status", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleSetStatus(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleSetStatus_RejectsTooManyLoadingMessages(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})
	poster := a.poster.(*recordingPoster)

	body, _ := json.Marshal(setStatusRequest{
		ThreadTS:        "1111.000",
		Status:          "is thinking…",
		LoadingMessages: make([]string, 11),
	})
	req := httptest.NewRequest(http.MethodPost, "/status", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleSetStatus(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
	if len(poster.statusCalls) != 0 {
		t.Errorf("SetThreadStatus calls = %d, want 0 (rejected before reaching Slack)", len(poster.statusCalls))
	}
}

func TestHandleSetStatus_InvalidJSON(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})

	req := httptest.NewRequest(http.MethodPost, "/status", bytes.NewBufferString("not json"))
	w := httptest.NewRecorder()
	a.HandleSetStatus(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleSetStatus_MethodNotAllowed(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	a.HandleSetStatus(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleStream_DeliversChunkToStreamManager(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})
	streamer := a.streamManager.streamer.(*recordingStreamer)

	body, _ := json.Marshal(streamRequest{
		ChannelID: "C123",
		ThreadTS:  "1111.000",
		StreamKey: "msg-1",
		Text:      "Hello",
		Index:     "0",
		Final:     "false",
	})
	req := httptest.NewRequest(http.MethodPost, "/stream", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleStream(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if len(streamer.startCalls) != 1 {
		t.Fatalf("StartStream calls = %d, want 1", len(streamer.startCalls))
	}
	if got := streamer.startCalls[0]; got.channelID != "C123" || got.threadTS != "1111.000" || got.text != "Hello" {
		t.Errorf("StartStream call = %+v, want C123/1111.000/Hello", got)
	}
}

func TestHandleStream_FallsBackToConfiguredChannel(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C-default"})
	streamer := a.streamManager.streamer.(*recordingStreamer)

	body, _ := json.Marshal(streamRequest{ThreadTS: "1111.000", StreamKey: "msg-1", Text: "Hello", Index: "0", Final: "true"})
	req := httptest.NewRequest(http.MethodPost, "/stream", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleStream(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if len(streamer.startCalls) != 1 || streamer.startCalls[0].channelID != "C-default" {
		t.Errorf("StartStream calls = %+v, want one call on C-default", streamer.startCalls)
	}
}

func TestHandleStream_EmptyFinalDefaultsToFalse(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})
	streamer := a.streamManager.streamer.(*recordingStreamer)

	body, _ := json.Marshal(streamRequest{ThreadTS: "1111.000", StreamKey: "msg-1", Text: "Hello", Index: "0"})
	req := httptest.NewRequest(http.MethodPost, "/stream", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleStream(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if len(streamer.stopCalls) != 0 {
		t.Errorf("StopStream calls = %d, want 0 (an omitted final must not finalize)", len(streamer.stopCalls))
	}
}

func TestHandleStream_RejectsMissingStreamKey(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})

	body, _ := json.Marshal(streamRequest{ThreadTS: "1111.000", Text: "Hello", Index: "0"})
	req := httptest.NewRequest(http.MethodPost, "/stream", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleStream(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleStream_RejectsMissingChannelIDWithoutDefault(t *testing.T) {
	a := newTestAdapter(&Config{})

	body, _ := json.Marshal(streamRequest{ThreadTS: "1111.000", StreamKey: "msg-1", Text: "Hello", Index: "0"})
	req := httptest.NewRequest(http.MethodPost, "/stream", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleStream(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleStream_RejectsNonIntegerIndex(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})

	body, _ := json.Marshal(streamRequest{ThreadTS: "1111.000", StreamKey: "msg-1", Text: "Hello", Index: "not-a-number"})
	req := httptest.NewRequest(http.MethodPost, "/stream", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleStream(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleStream_RejectsNonBooleanFinal(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})

	body, _ := json.Marshal(streamRequest{ThreadTS: "1111.000", StreamKey: "msg-1", Text: "Hello", Index: "0", Final: "not-a-bool"})
	req := httptest.NewRequest(http.MethodPost, "/stream", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	a.HandleStream(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleStream_InvalidJSON(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})

	req := httptest.NewRequest(http.MethodPost, "/stream", bytes.NewBufferString("not json"))
	w := httptest.NewRecorder()
	a.HandleStream(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleStream_MethodNotAllowed(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})

	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	w := httptest.NewRecorder()
	a.HandleStream(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleSubscribers_MethodNotAllowed(t *testing.T) {
	a := newTestAdapter(&Config{ChannelID: "C0"})
	req := httptest.NewRequest(http.MethodPost, "/subscribers", nil)
	w := httptest.NewRecorder()
	a.HandleSubscribers(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}
