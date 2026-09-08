# channel-server — Developer Guide

## Responsibility

Generic message delivery to Claude Code. **It has no knowledge of message sources (Slack, Discord, etc.).**

- Receives messages from external adapters over a Unix socket
- Pushes messages to Claude Code via MCP `claude/channel`
- Relays approve/deny via `claude/channel/permission`. It exposes no reply
  tool: the agent's turn-boundary hooks (`claude-agent-activity`) publish the
  agent's own text as `plect.message` / `plect.message_delta` events
  directly, and delivery to the message source happens from there, not
  through this server.

## Dependency rules

- Must not import `slack-adapter` or `plect` code
- Must not call the Slack API directly
- Protocol types are defined in the `plect/contracts/channel-protocol` module and must stay source-independent

## Package exposure

- `server/` is a public package (not `internal/`) for this module's own `cmd/channel-server` entry point — not because another module imports it. `slack-adapter` used to import `server.NewSocketListener` in its tests; it now owns a protocol-conformant fake instead (`plugins/slack/src/slack-adapter/internal/adapter/fakesocket_test.go`), so no other module depends on this package.
- Protocol types have already been split out into the `plect/contracts/channel-protocol` module; they are not channel-server-specific.

## Protocol

Framing over the Unix socket is a 4-byte big-endian length prefix + JSON payload. Message types are defined in the `plect/contracts/channel-protocol` module.

| Message type | Direction | Purpose |
|---|---|---|
| `register` | adapter → server | Connection registration |
| `message` | adapter → server | Text message delivery |
| `reply` | server → adapter | Reply (defined for the wire protocol; this server no longer sends one — see Responsibility above) |
| `permission` | server → adapter | Permission prompt |

## Testing

```bash
go test ./...
```

## Directory layout

```
cmd/channel-server/   entry point (CHANNEL_SOCKET_PATH required)
server/               MCP server, Unix socket listener, MessageSender interface
```
