package adapter

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

var slackTimestampShape = regexp.MustCompile(`^\d{10}\.\d{6}$`)

// isSlackTimestampShape reports whether ts has the shape Slack actually
// issues (10-digit seconds, 6-digit microseconds). A value with any other
// shape can never match a real thread, so persisting it turns a routing bug
// into a permanent misroute instead of a delivery failure a retry could
// recover from.
func isSlackTimestampShape(ts string) bool {
	return slackTimestampShape.MatchString(ts)
}

type threadFetcher interface {
	fetchThreadReplies(channelID, threadTS string) ([]slack.Message, error)
	userDisplayName(userID string) string
}

func (a *Adapter) handleAppMention(ev *slackevents.AppMentionEvent) {
	a.logger.Info("received app mention",
		"user", ev.User, "bot_id", ev.BotID,
		"thread_ts", ev.ThreadTimeStamp, "ts", ev.TimeStamp, "channel", ev.Channel)

	if ev.BotID != "" {
		a.logger.Debug("dropping app mention", "reason", "bot_message", "bot_id", ev.BotID)
		return
	}
	if !a.cfg.IsMentionUserAllowed(ev.User) {
		a.logger.Debug("dropping app mention", "reason", "user_not_allowed", "user", ev.User)
		return
	}

	threadTS := ev.ThreadTimeStamp
	if threadTS == "" {
		threadTS = ev.TimeStamp
	}
	if threadTS == "" {
		a.logger.Debug("dropping app mention", "reason", "missing_thread_ts")
		return
	}

	sub, ok := a.broker.Find(threadTS)
	if !ok {
		a.logger.Info("app mention skipped: unbound thread", "thread_ts", threadTS, "channel_id", ev.Channel)
		a.dispatchUnboundMention(ev, threadTS)
		return
	}
	if sub.ChannelID != "" && ev.Channel != "" && sub.ChannelID != ev.Channel {
		a.logger.Info("app mention skipped: unbound channel",
			"thread_ts", threadTS, "bound_channel_id", sub.ChannelID, "event_channel_id", ev.Channel)
		return
	}
	if sub.SessionName == "" {
		a.logger.Info("app mention skipped: subscription has no session_name", "thread_ts", threadTS, "channel_id", ev.Channel)
		return
	}
	channelID := sub.ChannelID
	if channelID == "" {
		channelID = ev.Channel
	}

	fetcher := a.threadFetcher
	if fetcher == nil {
		fetcher = a
	}
	replies, err := fetcher.fetchThreadReplies(channelID, threadTS)
	if err != nil {
		a.logger.Warn("app mention skipped: failed to fetch thread replies",
			"thread_ts", threadTS, "channel_id", channelID, "error", err)
		return
	}

	body, deliveredThrough := buildDeliberationTranscript(fetcher, replies, *ev, sub, a.cfg.DeliverFullThread)
	if body == "" {
		a.logger.Info("app mention skipped: empty transcript", "thread_ts", threadTS, "channel_id", channelID)
		return
	}
	mentionUser := fetcher.userDisplayName(ev.User)
	if mentionUser == "" {
		mentionUser = ev.User
	}
	if err := a.publishEvent(sub.SessionName, "user.emit", "slack", "inbound",
		"Slack deliberation from "+mentionUser, body,
		map[string]string{
			"thread_ts":  threadTS,
			"channel_id": channelID,
			"user_id":    ev.User,
			"user":       mentionUser,
		}); err != nil {
		return
	}
	a.broker.MarkDelivered(threadTS, deliveredThrough)
}

func (a *Adapter) fetchThreadReplies(channelID, threadTS string) ([]slack.Message, error) {
	if a.api == nil {
		return nil, errors.New("slack API client unavailable")
	}
	var all []slack.Message
	cursor := ""
	for {
		msgs, hasMore, nextCursor, err := a.api.GetConversationReplies(&slack.GetConversationRepliesParameters{
			ChannelID: channelID,
			Timestamp: threadTS,
			Cursor:    cursor,
			Inclusive: true,
			Oldest:    threadTS,
			Limit:     200,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, msgs...)
		if !hasMore || nextCursor == "" {
			break
		}
		cursor = nextCursor
	}
	return all, nil
}

func (a *Adapter) userDisplayName(userID string) string {
	if userID == "" {
		return ""
	}
	if a.api == nil {
		return userID
	}
	info, err := a.api.GetUserInfo(userID)
	if err != nil {
		return userID
	}
	if info.Profile.DisplayName != "" {
		return info.Profile.DisplayName
	}
	if info.RealName != "" {
		return info.RealName
	}
	if info.Name != "" {
		return info.Name
	}
	return userID
}

func buildDeliberationTranscript(fetcher threadFetcher, replies []slack.Message, mention slackevents.AppMentionEvent, sub Subscriber, fullThread bool) (string, string) {
	messages := selectDeliberationMessages(replies, sub.ThreadTS, mention.TimeStamp, sub.DeliveredThrough, fullThread)
	lines := make([]string, 0, len(messages)+1)
	deliveredThrough := mention.TimeStamp
	for _, msg := range messages {
		if msg.Timestamp != "" && compareSlackTS(msg.Timestamp, deliveredThrough) > 0 {
			deliveredThrough = msg.Timestamp
		}
		lines = append(lines, formatTranscriptLine(msg.Timestamp, displayNameForMessage(fetcher, msg), msg.Text))
	}
	mentionUser := fetcher.userDisplayName(mention.User)
	if mentionUser == "" {
		mentionUser = mention.User
	}
	lines = append(lines, formatTranscriptLine(mention.TimeStamp, mentionUser, mention.Text))
	return strings.Join(lines, "\n"), deliveredThrough
}

// catchUpThread never propagates a fetch or publish failure to the caller:
// leaving DeliveredThrough untouched on failure means a later re-subscribe
// (e.g. after a runtime restart re-runs the run-scoped slack_subscribe
// task) retries instead of silently losing the history.
func (a *Adapter) catchUpThread(sub Subscriber, through string) Subscriber {
	if through == "" {
		return sub
	}
	if sub.DeliveredThrough != "" && compareSlackTS(sub.DeliveredThrough, through) >= 0 {
		return sub
	}
	if sub.SessionName == "" {
		a.logger.Info("catch-up skipped: subscription has no session_name", "thread_ts", sub.ThreadTS)
		return sub
	}

	fetcher := a.threadFetcher
	if fetcher == nil {
		fetcher = a
	}
	replies, err := fetcher.fetchThreadReplies(sub.ChannelID, sub.ThreadTS)
	if err != nil {
		a.logger.Warn("catch-up skipped: failed to fetch thread replies",
			"thread_ts", sub.ThreadTS, "channel_id", sub.ChannelID, "error", err)
		return sub
	}

	messages := selectCatchUpMessages(replies, sub.ThreadTS, through, sub.DeliveredThrough)
	if len(messages) == 0 {
		a.logger.Info("catch-up skipped: empty transcript", "thread_ts", sub.ThreadTS, "channel_id", sub.ChannelID)
		return sub
	}

	body := buildCatchUpTranscript(fetcher, messages)
	if err := a.publishEvent(sub.SessionName, "user.emit", "slack", "inbound",
		"Slack thread history", body,
		map[string]string{
			"thread_ts":  sub.ThreadTS,
			"channel_id": sub.ChannelID,
		}); err != nil {
		return sub
	}
	if updated, ok := a.broker.MarkDelivered(sub.ThreadTS, through); ok {
		sub = updated
	}
	return sub
}

// selectCatchUpMessages includes through itself, unlike
// selectDeliberationMessages' mentionTS bound: there is no separate live
// mention event to append here, so the boundary message is just another
// reply.
func selectCatchUpMessages(replies []slack.Message, threadTS, through, watermark string) []slack.Message {
	sorted := sortedReplies(replies)
	out := make([]slack.Message, 0, len(sorted))
	for _, msg := range sorted {
		if msg.Timestamp == "" || !renderableMessage(msg) {
			continue
		}
		isRoot := msg.Timestamp == threadTS
		switch {
		case isRoot:
			out = append(out, msg)
		case compareSlackTS(msg.Timestamp, through) > 0:
			continue
		case watermark == "" || compareSlackTS(msg.Timestamp, watermark) > 0:
			out = append(out, msg)
		}
	}
	return out
}

func buildCatchUpTranscript(fetcher threadFetcher, messages []slack.Message) string {
	lines := make([]string, 0, len(messages))
	for _, msg := range messages {
		lines = append(lines, formatTranscriptLine(msg.Timestamp, displayNameForMessage(fetcher, msg), msg.Text))
	}
	return strings.Join(lines, "\n")
}

func sortedReplies(replies []slack.Message) []slack.Message {
	sorted := append([]slack.Message(nil), replies...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return compareSlackTS(sorted[i].Timestamp, sorted[j].Timestamp) < 0
	})
	return sorted
}

func selectDeliberationMessages(replies []slack.Message, threadTS, mentionTS, watermark string, fullThread bool) []slack.Message {
	sorted := sortedReplies(replies)
	out := make([]slack.Message, 0, len(sorted))
	for _, msg := range sorted {
		if msg.Timestamp == "" || !renderableMessage(msg) {
			continue
		}
		isRoot := msg.Timestamp == threadTS
		switch {
		case isRoot:
			out = append(out, msg)
		case mentionTS != "" && compareSlackTS(msg.Timestamp, mentionTS) >= 0:
			continue
		case fullThread || watermark == "" || compareSlackTS(msg.Timestamp, watermark) > 0:
			out = append(out, msg)
		}
	}
	return out
}

func renderableMessage(msg slack.Message) bool {
	if msg.Hidden || msg.DeletedTimestamp != "" {
		return false
	}
	switch msg.SubType {
	case "", slack.MsgSubTypeBotMessage:
		return true
	default:
		return false
	}
}

func displayNameForMessage(fetcher threadFetcher, msg slack.Message) string {
	if msg.User != "" {
		if name := fetcher.userDisplayName(msg.User); name != "" {
			return name
		}
		return msg.User
	}
	if msg.BotProfile != nil && msg.BotProfile.Name != "" {
		return msg.BotProfile.Name
	}
	if msg.Username != "" {
		return msg.Username
	}
	if msg.BotID != "" {
		return msg.BotID
	}
	return "unknown"
}

func formatTranscriptLine(ts, author, text string) string {
	return fmt.Sprintf("[%s] %s: %s", formatSlackTimestamp(ts), author, text)
}

func formatSlackTimestamp(ts string) string {
	sec, nsec, ok := parseSlackTimestamp(ts)
	if !ok {
		return ts
	}
	return time.Unix(sec, nsec).UTC().Format(time.RFC3339Nano)
}

func compareSlackTS(a, b string) int {
	asec, afrac := splitSlackTimestamp(a)
	bsec, bfrac := splitSlackTimestamp(b)
	if len(asec) != len(bsec) {
		if len(asec) < len(bsec) {
			return -1
		}
		return 1
	}
	if asec != bsec {
		if asec < bsec {
			return -1
		}
		return 1
	}
	maxFracLen := max(len(afrac), len(bfrac))
	afrac = padRight(afrac, maxFracLen)
	bfrac = padRight(bfrac, maxFracLen)
	if afrac == bfrac {
		return 0
	}
	if afrac < bfrac {
		return -1
	}
	return 1
}

func parseSlackTimestamp(ts string) (int64, int64, bool) {
	secPart, fracPart := splitSlackTimestamp(ts)
	sec, err := strconv.ParseInt(secPart, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	fracPart = padRight(fracPart, 9)
	if len(fracPart) > 9 {
		fracPart = fracPart[:9]
	}
	nsec, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return sec, nsec, true
}

func splitSlackTimestamp(ts string) (string, string) {
	sec, frac, ok := strings.Cut(ts, ".")
	if !ok {
		return ts, ""
	}
	return sec, frac
}

func padRight(s string, length int) string {
	if len(s) >= length {
		return s
	}
	return s + strings.Repeat("0", length-len(s))
}
