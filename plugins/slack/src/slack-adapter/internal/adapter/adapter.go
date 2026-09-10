package adapter

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	protocol "github.com/kecbigmt/plecture/contracts/channel-protocol"
)

// Adapter connects Slack (via Socket Mode) to channel server instances (via Unix socket).
type Adapter struct {
	cfg            *Config
	api            *slack.Client
	sm             *socketmode.Client
	workspace      string
	teamID         string
	socketPool     *SocketPool
	broker         *Broker
	poster         ThreadPoster
	threader       ThreadCreator
	threadFetcher  threadFetcher
	eventPublisher eventPublisher
	statusManager  *StatusManager
	streamManager  *StreamManager
	logger         *slog.Logger

	permalinkResolver permalinkResolver
	mentionHook       mentionHookRunner
	mentions          *mentionStream
}

// SubscribersStatePath resolves $XDG_STATE_HOME/slack-adapter/subscribers.json,
// falling back to ~/.local/state/.../subscribers.json. Empty disables persistence.
func SubscribersStatePath() string {
	if stateHome := os.Getenv("XDG_STATE_HOME"); stateHome != "" {
		return filepath.Join(stateHome, "slack-adapter", "subscribers.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "slack-adapter", "subscribers.json")
}

// New creates a new Adapter.
func New(cfg *Config, logger *slog.Logger) *Adapter {
	options := []slack.Option{}
	if cfg.SlackAppToken != "" {
		options = append(options, slack.OptionAppLevelToken(cfg.SlackAppToken))
	}
	api := slack.New(cfg.SlackBotToken, options...)

	a := &Adapter{
		cfg:    cfg,
		api:    api,
		broker: NewBroker(SubscribersStatePath(), logger),
		logger: logger,
	}
	if cfg.SlackAppToken != "" {
		a.sm = socketmode.New(api)
	}
	a.poster = a
	a.threader = a
	a.threadFetcher = a
	a.eventPublisher = cliEventPublisher{}
	a.permalinkResolver = a
	a.mentionHook = cliMentionHookRunner{}
	a.mentions = newMentionStream()
	a.statusManager = NewStatusManager(a.poster, cfg.StatusTTLDuration(), logger)
	a.socketPool = NewSocketPool(a.poster, logger, a.captureOutbound, a.statusManager)
	// Cache workspace name from Slack API
	resp, err := api.AuthTest()
	if err != nil {
		logger.Warn("AuthTest failed, workspace info unavailable", "error", err)
	} else if resp.URL != "" {
		if u, err := url.Parse(resp.URL); err == nil {
			parts := strings.Split(u.Hostname(), ".")
			if len(parts) > 0 {
				a.workspace = parts[0]
				logger.Info("workspace resolved", "workspace", a.workspace)
			}
		}
		a.teamID = resp.TeamID
	}
	a.streamManager = NewStreamManager(a, a.poster, a.teamID, cfg.StreamRecipientUserID(), logger)

	// Pre-connect so restored subscribers can push replies immediately.
	for _, sub := range a.broker.List() {
		if err := a.socketPool.Connect(sub.SocketPath, sub.ChannelID, sub.ThreadTS); err != nil {
			logger.Warn("pre-connect to restored subscriber failed",
				"thread_ts", sub.ThreadTS, "socket_path", sub.SocketPath, "error", err)
		}
	}

	return a
}

// Close releases the adapter's background resources.
func (a *Adapter) Close() {
	if a.statusManager != nil {
		a.statusManager.Stop()
	}
}

// Run starts the Slack Socket Mode listener. Blocks until ctx is cancelled.
func (a *Adapter) Run(ctx context.Context) error {
	if a.sm == nil {
		<-ctx.Done()
		return nil
	}
	go a.handleSlackEvents(ctx)
	return a.sm.RunContext(ctx)
}

func (a *Adapter) handleSlackEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-a.sm.Events:
			if !ok {
				return
			}
			a.processEvent(evt)
		}
	}
}

func (a *Adapter) processEvent(evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeEventsAPI:
		a.sm.Ack(*evt.Request)
		eventsAPI, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			return
		}
		a.handleEventsAPI(eventsAPI)
	case socketmode.EventTypeConnecting:
		a.logger.Info("connecting to Slack")
	case socketmode.EventTypeConnected:
		a.logger.Info("connected to Slack")
	case socketmode.EventTypeConnectionError:
		a.logger.Error("connection error",
			"component", "slack-adapter",
			"event", "slack_connection_error",
			"error", fmt.Sprintf("%v", evt.Data),
		)
	default:
		a.logger.Debug("unhandled event type", "type", evt.Type)
	}
}

func (a *Adapter) handleEventsAPI(event slackevents.EventsAPIEvent) {
	switch event.Type {
	case slackevents.CallbackEvent:
		switch ev := event.InnerEvent.Data.(type) {
		case *slackevents.MessageEvent:
			a.handleMessage(ev)
		case *slackevents.AppMentionEvent:
			a.handleAppMention(ev)
		}
	}
}

func (a *Adapter) handleMessage(ev *slackevents.MessageEvent) {
	a.logger.Info("received message",
		"user", ev.User, "bot_id", ev.BotID, "sub_type", ev.SubType,
		"thread_ts", ev.ThreadTimeStamp, "channel", ev.Channel)

	// Ignore bot messages and subtypes (edits, deletes, etc.)
	if ev.BotID != "" || ev.SubType != "" {
		a.logger.Debug("dropping message", "reason", "bot_or_subtype", "bot_id", ev.BotID, "sub_type", ev.SubType)
		return
	}

	// Sender gating
	if !a.cfg.IsUserAllowed(ev.User) {
		a.logger.Debug("dropping message", "reason", "user_not_allowed", "user", ev.User)
		return
	}

	// Determine thread_ts for routing
	threadTS := ev.ThreadTimeStamp
	if threadTS == "" {
		a.logger.Debug("dropping message", "reason", "not_thread_reply")
		return // not a thread reply, ignore
	}

	// Resolve display name
	userName := ev.User
	info, err := a.api.GetUserInfo(ev.User)
	if err == nil {
		userName = info.Profile.DisplayName
		if userName == "" {
			userName = info.RealName
		}
	}

	// Source marks this as an authenticated interactive-user message (the user
	// passed IsUserAllowed above). It is what lets channel-server accept a
	// "y <id>" / "n <id>" reply as a permission verdict; content deliveries
	// (notify/bus) leave Source empty and so can never forge one.
	msg := protocol.MessagePayload{
		User:     userName,
		UserID:   ev.User,
		Text:     ev.Text,
		ThreadTS: threadTS,
		Source:   "slack",
	}

	sub, ok := a.broker.Find(threadTS)
	if !ok {
		a.logger.Debug("dropping message", "reason", "no_subscriber", "thread_ts", threadTS)
		return
	}

	if sub.ChannelID == "" {
		sub.ChannelID = ev.Channel
	}

	if err := a.deliverToChannelServer(sub, msg); err != nil {
		if _, perr := a.poster.PostToThread(ev.Channel, threadTS, ":warning: Failed to deliver the message. The session may have ended."); perr != nil {
			a.logger.Error("failed to post error notification to Slack",
				"component", "slack-adapter",
				"event", "slack_post_error",
				"error", perr,
			)
		}
		return
	}
	a.captureInbound(sub, msg)
}

// deliverToChannelServer sends msg over the subscriber's Unix socket,
// evicting the subscriber if the socket is gone (lazy GC). Both call
// sites — Slack-inbound and POST /notify — share this so eviction is
// symmetric across delivery paths.
func (a *Adapter) deliverToChannelServer(sub Subscriber, msg protocol.MessagePayload) error {
	if _, err := os.Stat(sub.SocketPath); err != nil {
		a.logger.Warn("subscriber socket missing, evicting", "thread_ts", sub.ThreadTS, "socket_path", sub.SocketPath)
		a.broker.Unsubscribe(sub.ThreadTS)
		return err
	}
	if err := a.socketPool.Send(sub.SocketPath, sub.ChannelID, msg); err != nil {
		a.logger.Error("channel-server send failed",
			"component", "slack-adapter",
			"event", "message_send_error",
			"thread_ts", sub.ThreadTS,
			"socket_path", sub.SocketPath,
			"error", err,
		)
		return err
	}
	return nil
}

// deliverFramed posts a GitHub-style change notification to a subscriber's
// channel-server (with "[GitHub <type>] <url>: <summary>") and/or Slack thread
// (with an emoji prefix). Shared by POST /notify and the bus github.* path so
// both produce the same wire format. The bool returns report what was delivered
// (for the /notify response).
func (a *Adapter) deliverFramed(sub Subscriber, changeType, url, summary string, toChannel, toSlack bool) (channelDelivered, slackDelivered bool) {
	if toChannel && sub.SocketPath != "" {
		text := fmt.Sprintf("[GitHub %s] %s: %s", changeType, url, summary)
		if err := a.deliverToChannelServer(sub, protocol.MessagePayload{Text: text, ThreadTS: sub.ThreadTS}); err == nil {
			channelDelivered = true
		}
	}
	if toSlack && sub.ThreadTS != "" && sub.ChannelID != "" {
		text := fmt.Sprintf("%s %s", notifyEmoji(changeType, summary), summary)
		if _, err := a.poster.PostToThread(sub.ChannelID, sub.ThreadTS, text); err != nil {
			a.logger.Error("notify: slack post failed",
				"component", "slack-adapter", "event", "slack_post_error",
				"session_name", sub.SessionName, "error", err)
		} else {
			slackDelivered = true
		}
	}
	return
}

// PostToThread posts a message to a Slack thread.
func (a *Adapter) PostToThread(channelID, threadTS, text string) (string, error) {
	_, ts, err := a.api.PostMessage(
		channelID,
		slack.MsgOptionText(text, false),
		slack.MsgOptionTS(threadTS),
	)
	return ts, err
}

func (a *Adapter) SetThreadStatus(channelID, threadTS, status string, loadingMessages []string) error {
	return a.api.SetAssistantThreadsStatus(slack.AssistantThreadsSetStatusParameters{
		ChannelID:       channelID,
		ThreadTS:        threadTS,
		Status:          status,
		LoadingMessages: loadingMessages,
	})
}

// CreateThread posts a new message to a channel and returns Slack's permalink.
func (a *Adapter) CreateThread(channelID, text string) (string, string, error) {
	_, ts, err := a.api.PostMessage(
		channelID,
		slack.MsgOptionText(text, false),
	)
	if err != nil {
		return "", "", err
	}
	permalink, err := a.api.GetPermalink(&slack.PermalinkParameters{
		Channel: channelID,
		Ts:      ts,
	})
	if err != nil {
		return "", "", err
	}
	return ts, permalink, nil
}
