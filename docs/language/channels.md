# Channels

A channel definition binds an event delivery to a primitive. Trusted config
fixes the primitive and the message shape; a workflow supplies only the
author-declared parameters. Event data never chooses what runs.

Channels are trusted plugin, machine-owned, or selected trusted-project
configuration: an `exec` or `shell` channel runs a process, so an untrusted
repository checkout may not declare one.

## Primitives

| `type` | Delivery |
|---|---|
| `unix_socket` | Dial `path`, write `body` as one framed message. |
| `exec` | Run an exec action. |
| `shell` | Run a shell action. |

<!-- fixture: channels/unix-socket.toml -->
```toml
[delivery]
kind = "channel"
type = "unix_socket"
path = { from = "inputs.path" }
body = { json = { from = "event" } }

[delivery.input_schema]
path = { type = "string", required = true }
```

An `exec` channel names a plugin-owned executable through `bin`, or a literal
OS command through `command` — see [`actions.md`](actions.md).

A `shell` channel exists for delivery that is genuinely imperative: splitting a
keystroke burst, polling a readiness predicate, retrying with backoff. Its
`script` is literal and its `bind` table carries the event data and
capabilities it needs, so the event is never part of the command.

<!-- fixture: channels/terminal-capability.toml -->
```toml
[terminal_submit]
kind = "channel"
type = "shell"
script = '''
set -u
sh -c "$send_text" terminal-send-text "$message"
sleep 1
sh -c "$send_keys" terminal-send-keys Enter
'''
timeout = "45s"

[terminal_submit.bind]
send_text = { terminal = "send_text" }
send_keys = { terminal = "send_keys" }
capture   = { terminal = "capture" }
message   = { expr = "'[' + event.type + '] ' + (event.body != '' ? event.body : event.summary)" }
```

## Selecting events

A channel definition only fixes delivery; a workflow's own
`[[<id>.event.channel]]` binding (see [`workflows.md`](workflows.md#event-channels))
selects one and lists the event-type globs it relays with `include`:

```toml
[[coding.event.channel]]
name    = "runtime"
uses    = "delivery"
include = ["plect.instruction", "plect.node.result"]
```

## Parameters

`[<id>.input_schema]` declares the channel's parameters per key: a `type`, a
`required` flag, and an optional `default`. This is deliberately not the full
JSON Schema document effects and resources carry — only a channel input's
presence is checked before delivery, and a `default` is what makes an optional
parameter usable at all.

## Timeout

`timeout` bounds one delivery attempt. It reads author-declared parameters
only; event data is excluded, so the event being delivered can never choose its
own deadline. An empty `timeout` leaves the deadline to the caller's retry
policy.

## Validation rules

- `type` is `unix_socket`, `exec`, or `shell`.
- A `unix_socket` channel declares `path` and `body`, and no process fields.
- An `exec` channel declares exactly one of `bin` and `command`.
- A `shell` channel declares `script`, containing no interpolation.
- `command` is never a computed value.
- `timeout` projects `inputs.*` only.
- A terminal capability requires some effect in the plan to declare that verb.
