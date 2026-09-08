# slack-adapter — Developer Guide

## Responsibility

Slack-specific message relay + subscription broker.

- Receives messages via Slack Socket Mode when an app token is configured
- Resolves `thread_ts → {channel_id, socket_path}` with an in-memory map (`Broker`)
- Persists subscriptions (and unsubscribed threads' delivery-watermark tombstones) to `$XDG_STATE_HOME/slack-adapter/subscribers.json` via atomic write and reloads them at startup (makes broker restarts transparent to plect)
- Forwards messages to channel-server; posts replies via the Slack API
- Shows/clears a bound thread's assistant shimmer status line (`StatusManager`, `assistant.threads.setStatus`) around inbound delivery and outbound replies, with a TTL fallback for a session that never posts back
- HTTP API: `/threads` (create a thread and return its permalink), `/messages` (post), `/status` (set/clear the shimmer status), `/stream` (render a `plect.message_delta` chunk sequence as one live-updating thread reply), `/subscribe` (register/unregister a subscription), `/subscribers` (list subscriptions), `/unbound-mentions` (stream every unbound app mention)
- The `subscribe unbound-mentions` CLI subcommand is a client of its own resident service's `/unbound-mentions` feed, not a separate integration: it never opens a second Socket Mode connection
- The `resource observe` CLI subcommand is `thread`'s (the resource observer's) `observe` action (the config language requires one); it never contacts the resident service, since the observer's `state_schema` is empty and there is nothing live to fetch

## Dependency rules

- Must not import `plect/app` code
- Imports the `plect/contracts/channel-protocol` module to share protocol types
- Does not depend on plect's state schema (`plect/contracts/state`) — removed in Phase 2
- Must not import channel-server's `server` package, in production or test
  code (docs/adr/2026-08-17-plugin-boundary-contracts.md: chat-delivery
  plugins do not import channel-server client packages). Tests that need a
  Unix-socket server to talk to use `internal/adapter/fakesocket_test.go`,
  a protocol-conformant fake owned by this module, built only on
  `contracts/channel-protocol`.
- Must not launch Claude Code directly

## Boundary with channel-server

- slack-adapter is channel-server's **client**: it connects to the Unix socket and sends messages
- channel-server is the Unix socket's **server**: it listens and waits for connections
- The protocol is defined in the `plect/contracts/channel-protocol` module, a shared contract owned by neither service

## Config file path

`~/.config/slack-adapter/config.toml`

## HTTP API

| Endpoint | Purpose | Caller |
|---|---|---|
| `GET /info` | Returns workspace name and default channel ID | plect task (`slack_thread`) |
| `POST /threads` | Creates a Slack thread and returns its permalink | plect task (`slack_thread`) |
| `POST /messages` | Posts a message to a thread | plect channel (`slack`), `claude-slack-notify.sh` |
| `POST /status` | Sets/clears a thread's shimmer status line without posting | future agent-hook wiring (not yet a caller) |
| `POST /stream` | Renders a `plect.message_delta` chunk as part of one live-updating thread reply | plect channel (`stream`) |
| `POST /subscribe` / `DELETE /subscribe?thread_ts=...` | Register/unregister a subscription | plect task (`slack_subscribe`) |
| `GET /subscribers` | Lists subscriptions (for the `[health].alive` probe) | plect task (`slack_subscribe`) |
| `GET /unbound-mentions` | Streams one JSON item per unbound app mention as it occurs | the `subscribe unbound-mentions` CLI subcommand (a separate process; see below) |
| `POST /notify` | Notifies Slack + channel-server, keyed by `session_name` | deprecated rollback path (`github-watcher serve --allow-legacy-notify` only) |

The `/notify` request body includes `change_type`; the broker inspects GitHub-derived `change_type` values (`ci_status`, `review_decision`, `state`, etc.) to build the emoji prefix and `[GitHub …]` framing. This is an exception to slack-adapter's source-agnostic principle, kept only as an explicit rollback path for when the current event bus / `[[event.channel]]` delivery isn't available.

## Testing

```bash
go test ./...
```

socket_client_test and socket_router_test run integration tests against
`internal/adapter/fakesocket_test.go`'s `newFakeSocketListener`, not the
real channel-server.

## Directory layout

```
cmd/slack-adapter/     entry point
internal/adapter/      Slack Socket Mode, HTTP API, state.json lookup, connection pool, config
slack-app-manifest.yml Slack App manifest (scopes, events)
```
