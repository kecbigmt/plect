package adapter

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	SlackBotToken     string   `toml:"slack_bot_token"`
	SlackAppToken     string   `toml:"slack_app_token"`
	ChannelID         string   `toml:"channel_id"`
	ListenAddr        string   `toml:"listen_addr"`
	AllowedUserIDs    []string `toml:"allowed_user_ids"`
	NotifyUserIDs     []string `toml:"notify_user_ids"`
	DeliverFullThread bool     `toml:"deliver_full_thread"`
	StatusTTL         string   `toml:"status_ttl"`
	// OnUnboundMention is left as an opaque command, not e.g. a workflow
	// name, because dispatch policy (which workflow, which channels) is
	// deployment-specific and this plugin must not encode it.
	OnUnboundMention string `toml:"on_unbound_mention"`

	// hasRetiredStatusLoadingMessages records whether config.toml still
	// declares the retired status_loading_messages key. BurntSushi/toml
	// silently ignores a key this struct doesn't declare, so ValidateStartup
	// checks this instead of letting the key quietly do nothing.
	hasRetiredStatusLoadingMessages bool
}

func LoadConfig() *Config {
	cfg := &Config{
		ListenAddr: "127.0.0.1:7890",
	}

	home, err := os.UserHomeDir()
	if err == nil {
		configPath := filepath.Join(home, ".config", "slack-adapter", "config.toml")
		if _, statErr := os.Stat(configPath); statErr == nil {
			// An absent config.toml is a normal all-env-vars setup, but a
			// present-and-unparsable one is a user mistake that would
			// otherwise silently fall back to an empty (deny-all) config —
			// warn so it isn't mistaken for "everything is configured".
			meta, decodeErr := toml.DecodeFile(configPath, cfg)
			if decodeErr != nil {
				slog.Warn("config.toml present but failed to parse; using defaults", "path", configPath, "error", decodeErr)
			} else {
				for _, key := range meta.Undecoded() {
					if key.String() == "status_loading_messages" {
						cfg.hasRetiredStatusLoadingMessages = true
					}
				}
			}
		}
	}

	// Env vars only fill a field config.toml left empty: config.toml is a
	// file the user already fully controls, so an explicit value there is
	// never silently overridden by whatever happens to be in the process
	// environment. This is the credential path a resident-supervised
	// [[services]] declaration's `required_env` gates on (see this
	// plugin's plugin.toml) — plect never writes secrets into catalog
	// content, so they arrive as environment instead.
	fillFromEnv(&cfg.SlackBotToken, "SLACK_BOT_TOKEN")
	fillFromEnv(&cfg.SlackAppToken, "SLACK_APP_TOKEN")
	fillFromEnv(&cfg.ChannelID, "SLACK_CHANNEL_ID")
	fillFromEnv(&cfg.OnUnboundMention, "SLACK_ADAPTER_ON_UNBOUND_MENTION")

	return cfg
}

func (c *Config) ValidateStartup() error {
	if c.SlackBotToken == "" {
		return errors.New("slack_bot_token must be set in config")
	}
	if c.hasRetiredStatusLoadingMessages {
		return errors.New("status_loading_messages is retired and no longer accepted; remove it from config.toml (see plugins/slack/src/slack-adapter/README.md)")
	}
	return nil
}

// StatusTTLDuration falls back to defaultStatusTTL (with a warning) on an
// unparsable value, so a config typo disables the TTL fallback rather than
// startup itself.
func (c *Config) StatusTTLDuration() time.Duration {
	if c.StatusTTL == "" {
		return defaultStatusTTL
	}
	d, err := time.ParseDuration(c.StatusTTL)
	if err != nil {
		slog.Warn("status_ttl invalid, using default", "value", c.StatusTTL, "default", defaultStatusTTL, "error", err)
		return defaultStatusTTL
	}
	return d
}

func fillFromEnv(field *string, envVar string) {
	if *field != "" {
		return
	}
	if v := os.Getenv(envVar); v != "" {
		*field = v
	}
}

func (c *Config) IsUserAllowed(userID string) bool {
	if len(c.AllowedUserIDs) == 0 {
		return false // deny all if no allowlist configured
	}
	for _, id := range c.AllowedUserIDs {
		if id == userID {
			return true
		}
	}
	return false
}

// StreamRecipientUserID returns who chat.startStream should name as
// recipient_user_id (required when streaming to a channel), reusing
// allowed_user_ids rather than adding a dedicated key: notify_user_ids can
// list people who are only pinged, not conversing. Empty (no allowlist, or
// several entries with no single answer) leaves native streaming unable to
// start, which StreamManager's fallback path already covers.
func (c *Config) StreamRecipientUserID() string {
	if len(c.AllowedUserIDs) != 1 {
		return ""
	}
	return c.AllowedUserIDs[0]
}

func (c *Config) IsMentionUserAllowed(userID string) bool {
	if len(c.AllowedUserIDs) == 0 {
		return true
	}
	return c.IsUserAllowed(userID)
}

// MentionPrefix returns a string of Slack user mentions (e.g. "<@U123> <@U456> ")
// for use as a message prefix. Returns empty string if no notify_user_ids are configured.
func (c *Config) MentionPrefix() string {
	if len(c.NotifyUserIDs) == 0 {
		return ""
	}
	var parts []string
	for _, id := range c.NotifyUserIDs {
		parts = append(parts, fmt.Sprintf("<@%s>", id))
	}
	return strings.Join(parts, " ") + " "
}
