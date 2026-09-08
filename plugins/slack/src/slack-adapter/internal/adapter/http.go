package adapter

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// /subscribe absorbs the gap between claude reporting alive and
// channel-server actually binding the MCP socket. Defaults ≈ 5s. Tests
// tighten these via the package-level vars.
var (
	subscribeConnectAttempts = 25
	subscribeConnectInterval = 200 * time.Millisecond
)

type subscribeRequest struct {
	ThreadTS       string `json:"thread_ts"`
	ChannelID      string `json:"channel_id"`
	SocketPath     string `json:"socket_path"`
	SessionName    string `json:"session_name"`
	CatchUpThrough string `json:"catch_up_through,omitempty"`
}

type notifyRequest struct {
	SessionName         string `json:"session_name"`
	ChangeType          string `json:"change_type"`
	Summary             string `json:"summary"`
	URL                 string `json:"url"`
	NotifyChannelServer bool   `json:"notify_channel_server"`
	NotifySlack         bool   `json:"notify_slack"`
}

type notifyResponse struct {
	ChannelServerDelivered bool   `json:"channel_server_delivered"`
	SlackDelivered         bool   `json:"slack_delivered"`
	Reason                 string `json:"reason,omitempty"`
}

type infoResponse struct {
	Workspace string `json:"workspace"`
	ChannelID string `json:"channel_id"`
}

type createThreadRequest struct {
	ChannelID string `json:"channel_id"`
	Text      string `json:"text"`
}

type createThreadResponse struct {
	ThreadTS  string `json:"thread_ts"`
	ChannelID string `json:"channel_id"`
	Permalink string `json:"permalink"`
}

type postMessageRequest struct {
	ChannelID string `json:"channel_id"`
	ThreadTS  string `json:"thread_ts"`
	Text      string `json:"text"`
	Mention   bool   `json:"mention,omitempty"`
}

// streamRequest's index/final arrive as strings, not a JSON number/bool:
// they originate as plect.message_delta event metadata, which is always
// map[string]string (see contracts/event), and the channel passes inputs
// through verbatim rather than requiring every composing workflow to
// convert them first.
type streamRequest struct {
	ChannelID string `json:"channel_id"`
	ThreadTS  string `json:"thread_ts"`
	StreamKey string `json:"stream_key"`
	Text      string `json:"text"`
	Index     string `json:"index"`
	Final     string `json:"final"`
}

type setStatusRequest struct {
	ChannelID       string   `json:"channel_id"`
	ThreadTS        string   `json:"thread_ts"`
	Status          string   `json:"status"`
	LoadingMessages []string `json:"loading_messages,omitempty"`
}

// HandleInfo handles GET /info to return workspace and channel information.
func (a *Adapter) HandleInfo(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(infoResponse{
		Workspace: a.workspace,
		ChannelID: a.cfg.ChannelID,
	})
}

// HandleCreateThread handles POST /threads to create a new Slack thread.
func (a *Adapter) HandleCreateThread(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body createThreadRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if body.Text == "" {
		body.Text = "Claude Code session started."
	}
	channelID := body.ChannelID
	if channelID == "" {
		channelID = a.cfg.ChannelID
	}
	if channelID == "" {
		http.Error(w, "channel_id is required", http.StatusBadRequest)
		return
	}

	threader := a.threader
	if threader == nil {
		threader = a
	}
	ts, permalink, err := threader.CreateThread(channelID, body.Text)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(createThreadResponse{
		ThreadTS:  ts,
		ChannelID: channelID,
		Permalink: permalink,
	})
}

// HandlePostMessage handles POST /messages to post a message to an existing thread.
func (a *Adapter) HandlePostMessage(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body postMessageRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if body.ThreadTS == "" || body.Text == "" {
		http.Error(w, "thread_ts and text are required", http.StatusBadRequest)
		return
	}

	channelID := body.ChannelID
	if channelID == "" {
		channelID = a.cfg.ChannelID
	}
	if channelID == "" {
		http.Error(w, "channel_id is required", http.StatusBadRequest)
		return
	}

	text := body.Text
	if body.Mention {
		text = a.cfg.MentionPrefix() + text
	}

	_, err := a.PostToThread(channelID, body.ThreadTS, text)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (a *Adapter) HandleStream(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body streamRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if body.ThreadTS == "" || body.StreamKey == "" {
		http.Error(w, "thread_ts and stream_key are required", http.StatusBadRequest)
		return
	}
	channelID := body.ChannelID
	if channelID == "" {
		channelID = a.cfg.ChannelID
	}
	if channelID == "" {
		http.Error(w, "channel_id is required", http.StatusBadRequest)
		return
	}
	index, err := strconv.ParseInt(body.Index, 10, 64)
	if err != nil {
		http.Error(w, "index must be an integer", http.StatusBadRequest)
		return
	}
	final, err := strconv.ParseBool(body.Final)
	if body.Final != "" && err != nil {
		http.Error(w, "final must be a boolean", http.StatusBadRequest)
		return
	}

	if err := a.streamManager.Deliver(channelID, body.ThreadTS, body.StreamKey, index, body.Text, final); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// HandleSetStatus lets a caller report what a session is doing right now
// without posting a message. `status` is only an on/off flag (empty
// clears); custom text belongs in `loading_messages`, the only part of a
// channel thread's shimmer status that actually renders.
func (a *Adapter) HandleSetStatus(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body setStatusRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if body.ThreadTS == "" {
		http.Error(w, "thread_ts is required", http.StatusBadRequest)
		return
	}
	channelID := body.ChannelID
	if channelID == "" {
		channelID = a.cfg.ChannelID
	}
	if channelID == "" {
		http.Error(w, "channel_id is required", http.StatusBadRequest)
		return
	}
	if err := validateLoadingMessages(body.LoadingMessages); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body.LoadingMessages = clipLoadingMessages(body.LoadingMessages)

	var err error
	if body.Status == "" {
		err = a.statusManager.Clear(channelID, body.ThreadTS)
	} else {
		err = a.statusManager.Set(channelID, body.ThreadTS, body.Status, body.LoadingMessages)
	}
	if err != nil {
		writeStatusError(a.logger, w, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// HandleSubscribe routes POST (register) / DELETE (unregister) on /subscribe.
func (a *Adapter) HandleSubscribe(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodPost:
		a.handleSubscribePost(w, req)
	case http.MethodDelete:
		a.handleSubscribeDelete(w, req)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *Adapter) handleSubscribePost(w http.ResponseWriter, req *http.Request) {
	var body subscribeRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.ThreadTS == "" || body.ChannelID == "" || body.SocketPath == "" {
		http.Error(w, "thread_ts, channel_id, and socket_path are required", http.StatusBadRequest)
		return
	}

	// Connect before registering so a successful POST guarantees both
	// directions of routing. Otherwise the subscription would silently
	// drop channel-server → Slack replies.
	if err := a.connectWithRetry(body.SocketPath, body.ChannelID, body.ThreadTS); err != nil {
		a.logger.Warn("subscribe rejected: channel-server unreachable",
			"thread_ts", body.ThreadTS, "socket_path", body.SocketPath, "error", err)
		http.Error(w, "channel-server unreachable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	sub := a.broker.Subscribe(Subscriber{
		ThreadTS:    body.ThreadTS,
		ChannelID:   body.ChannelID,
		SocketPath:  body.SocketPath,
		SessionName: body.SessionName,
	})
	a.logger.Info("subscribed", "thread_ts", sub.ThreadTS, "socket_path", sub.SocketPath, "session_name", sub.SessionName)

	sub = a.catchUpThread(sub, body.CatchUpThrough)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sub)
}

func (a *Adapter) connectWithRetry(socketPath, channelID, threadTS string) error {
	var lastErr error
	for i := range subscribeConnectAttempts {
		err := a.socketPool.Connect(socketPath, channelID, threadTS)
		if err == nil {
			return nil
		}
		lastErr = err
		if i == subscribeConnectAttempts-1 {
			break
		}
		time.Sleep(subscribeConnectInterval)
	}
	return lastErr
}

func (a *Adapter) handleSubscribeDelete(w http.ResponseWriter, req *http.Request) {
	threadTS := req.URL.Query().Get("thread_ts")
	if threadTS == "" {
		http.Error(w, "thread_ts query parameter is required", http.StatusBadRequest)
		return
	}
	if removed, ok := a.broker.Unsubscribe(threadTS); !ok {
		a.logger.Debug("unsubscribe miss", "thread_ts", threadTS)
	} else {
		a.logger.Info("unsubscribed", "thread_ts", removed.ThreadTS)
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandleUnboundMentions handles GET /unbound-mentions: a raw feed of every
// unbound app mention as it occurs, one JSON item per line. It does not
// filter by channel itself — that is the `subscribe unbound-mentions`
// action's job — so this one resident connection can serve any number of
// actions, each with its own channel set, without the adapter tracking
// what each caller wants.
func (a *Adapter) HandleUnboundMentions(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ch, cancel := a.mentions.subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	enc := json.NewEncoder(w)
	for {
		select {
		case <-req.Context().Done():
			return
		case item := <-ch:
			if err := enc.Encode(item); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// HandleNotify handles POST /notify, routing a session-scoped notification
// to Slack and/or channel-server based on the subscriber map. The body
// carries session_name (the lookup key) plus the unformatted summary;
// presentation (emoji prefix, "[GitHub …]" channel framing) lives here so
// callers stay thin.
func (a *Adapter) HandleNotify(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body notifyRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.SessionName == "" || body.Summary == "" {
		http.Error(w, "session_name and summary are required", http.StatusBadRequest)
		return
	}

	resp := notifyResponse{}
	sub, ok := a.broker.BySession(body.SessionName)
	if !ok {
		resp.Reason = "no subscriber for session_name"
		a.logger.Info("notify miss", "session_name", body.SessionName, "change_type", body.ChangeType)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	resp.ChannelServerDelivered, resp.SlackDelivered = a.deliverFramed(
		sub, body.ChangeType, body.URL, body.Summary, body.NotifyChannelServer, body.NotifySlack)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// notifyEmoji returns the prefix emoji for a GitHub-style change notification.
func notifyEmoji(changeType, summary string) string {
	switch changeType {
	case "ci_status":
		if strings.Contains(summary, "SUCCESS") {
			return ":white_check_mark:"
		}
		return ":x:"
	case "review_decision":
		return ":eyes:"
	case "new_review_comments":
		return ":speech_balloon:"
	case "state":
		return ":arrows_counterclockwise:"
	case "new_comments":
		return ":memo:"
	case "new_commits":
		return ":hammer_and_wrench:"
	case "conflict":
		return ":warning:"
	case "conflict_resolved":
		return ":white_check_mark:"
	default:
		return ":bell:"
	}
}

// HandleSubscribers exposes the current subscription map for healthchecks.
func (a *Adapter) HandleSubscribers(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	subs := a.broker.List()
	if subs == nil {
		subs = []Subscriber{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(subs)
}
