# channel-server -- Source-independent MCP channel server

An MCP channel server launched as a Claude Code subprocess. It has no dependency on the message source (Slack, Discord, etc.) and exchanges messages with adapters over a Unix socket.

## How it works

1. Claude Code starts it via `--dangerously-load-development-channels server:channel-server`
2. channel-server connects to the Unix socket specified by `CHANNEL_SOCKET_PATH`
3. An adapter (e.g. slack-adapter) relays messages from the other end of the socket
4. Messages are pushed to Claude Code via the MCP `claude/channel` capability; approve/deny is relayed via `claude/channel/permission`
5. channel-server exposes no reply tool: the agent's turn-boundary hooks (`claude-agent-activity`, registered by the `runtime` task) publish the agent's own text as plect events directly, without going through this server

```
Claude Code
  └─ channel-server (MCP subprocess)
       │ Unix socket (CHANNEL_SOCKET_PATH)
       ▼
     adapter (slack-adapter, etc.)
```

## Environment variables

| Variable | Required | Description |
|------|------|------|
| `CHANNEL_SOCKET_PATH` | Yes | Path to the Unix socket (shared with the adapter) |

## Protocol

JSON messages are sent/received over the Unix socket, newline-delimited. Message types are defined in the `protocol/` package.

## Registering as a Claude Code MCP server

```bash
claude mcp add channel-server -- channel-server
```
