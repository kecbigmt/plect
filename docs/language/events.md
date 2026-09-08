# Events

plect's event bus is the append-only, per-session log every runtime,
provider, and consumer shares. A channel's `include` (`channels.md#selecting-events`)
and a workflow's `[tick].on` (`workflows.md#clocks`) select events by
type glob; this chapter documents the core, provider-neutral vocabulary those
globs can name.

## Agent messages

`plect.message` and `plect.message_chunk` are the runtime-neutral contract for
what an agent said: a one-shot completed message, or a message still
streaming. Core defines only this contract — which agent runtime produced a
message is metadata, never part of the type or a field a consumer branches
on.

| Type | Body | Metadata (required unless noted) |
|---|---|---|
| `plect.message` | The full text of one completed assistant message. | `message_id`, `turn_id` (optional), `role` (`assistant`), `source` |
| `plect.message_chunk` | The new text since the previous chunk. | `message_id`, `turn_id` (optional), `index` (0-based, monotonic per `message_id`), `final`, `source` |

- `message_id` identifies one message within its session: the runtime's own
  id when it has one, else a ULID the emitter mints. It is the join key
  across a `plect.message_chunk` sequence, and unique per `plect.message`.
- `turn_id` names the enclosing agent turn, carried when the runtime's own
  hooks expose one.
- `source` names which runtime produced the message (e.g. which agent CLI).
  It is informational only — a consumer never branches on it, matching
  core's policy of naming no provider.
- `summary` is the text's first line, capped at 120 characters. An emitter
  with empty text emits nothing.

For one `message_id` an emitter produces either a `plect.message_chunk`
sequence ending with `final = true`, or exactly one `plect.message` — never
both. A consumer that wants finished messages reads `plect.message` and chunk
sequences at their final chunk; a live view reads chunks as they arrive. A
runtime whose hooks expose no per-message streaming point emits
`plect.message` only.
