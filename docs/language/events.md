# Events

plect's event bus is the append-only, per-session log every runtime,
provider, and consumer shares. A channel's `include` (`channels.md#selecting-events`)
and a workflow's `[tick].on` (`workflows.md#clocks`) select events by
type glob; this chapter documents the core, provider-neutral vocabulary those
globs can name.

## Agent messages

`plect.message` and `plect.message_delta` are the runtime-neutral contract
for what an agent said, shared by every harness (Claude Code, Codex, Hermes
Agent, pi, DeepSeek, ...) core has surveyed. Core defines only this contract
— which harness produced a message, and which of its own fields it can fill,
is metadata, never part of the type.

`plect.message` is canonical: an emitter sends exactly one per `message_id`,
once that message's visible text is non-empty. `plect.message_delta` is an
optional live preview of a message still being produced; a `plect.message`
for the same `message_id` always follows it.

| Field | Required | Meaning |
|---|---|---|
| `message_id` | yes | Identifies one message within its session: the harness's own id when it has one, else a deterministic synthetic id the emitter mints (e.g. `<session>/<turn>/<step>`). Unique within the session. |
| `message_id_origin` | yes | `native` or `synthetic`. |
| `role` | yes | `assistant` — today's only value; emitters filter every other role before publishing. |
| `source` | yes | The harness id (`claude`, `codex`, `hermes`, `pi`, `deepseek`, ...). Informational only — a consumer never branches on it. |
| `turn_id` | no | The harness's own opaque id for the enclosing prompt cycle, when it has one. |
| `turn_index` | no | Integer ordinal of the prompt cycle, when the harness numbers it. |
| `step_index` | no | Integer ordinal of the model call inside the prompt cycle, when the harness distinguishes turn from step. |
| `run_id` | no | Opaque id of the prompt cycle or delegation run, when it differs from `turn_id`. |
| `interim` | no | `true` when the emitter knows this is not the turn's final answer. Default `false`. |
| `stop_reason` | no | Why the model stopped, when known: `completed`, `max_tokens`, `aborted`, `error`, or `interrupted`. |
| `truncated` | no | `true` when the emitter itself shortened the text. Default `false`. |
| `model`, `provider` | no | The harness's own strings, when it stamps them. |
| `surface` | no | Where the conversation happens (`cli`, `rpc`, `slack`, ...). |
| `agent_id`, `parent_agent_id`, `depth`, `agent_role` | no | Subagent identity, when the harness exposes it. No defaults — a harness with no role label omits `agent_role` rather than guessing. |
| `source_seq` | no | The harness's own monotonic sequence for this event, for consumer-side dedupe across redelivery. |
| `raw` | no | A JSON-encoded object of harness-specific leftovers. Explicitly non-normative: a consumer must not depend on its shape. |

Body is the message's visible text (its text blocks concatenated).

| Field | Required | Meaning |
|---|---|---|
| `message_id`, `message_id_origin`, `source` | yes | As above — the same `message_id` a preceding delta sequence used. |
| `kind` | yes | `text` or `reasoning`. Reasoning deltas are opt-in per emitter; a consumer may ignore them. |
| `index` | yes | Emitter-assigned, 0-based, monotonic per (`message_id`, `kind`) — never the harness's own block index. |
| `final` | yes | `true` on the sequence's last delta for this `message_id`. |
| `ordering` | no | `strict` (default) or `best_effort`. `best_effort` marks a harness whose delta stream can drop deltas, so a gap in `index` means loss, not a bug. |
| `turn_id`, `turn_index`, `step_index`, `run_id`, `source_seq`, `raw` | no | As above. |
| `block_index` | no | The harness's own block index, when it has one distinct from `index`. |

Body is the delta's new text.

### Rules

1. `plect.message` is always emitted, exactly once per `message_id`, once its
   text is non-empty. `plect.message_delta` is an optional preview: a delta
   sequence never substitutes for the `plect.message` that follows it. A
   consumer keys on `message_id` and replaces any delta-built preview with
   the arriving `plect.message`.
2. `summary` is the text's first line, capped at 120 characters. An emitter
   with empty text emits nothing.
3. Existing history is not rewritten: `claude.reply`, `codex.reply`, and any
   `claude.message_display` events already stored stay as they are. A
   consumer may render them as legacy messages.
4. Where an emitter hooks into its harness (Claude Code and Codex: hooks;
   Hermes Agent: a plugin hook; pi: an extension; DeepSeek: a native plugin)
   is that emitter's own concern, not part of this contract.

### Per-harness mapping

Which optional fields a harness's emitter can fill, from the harness
survey behind this contract:

| Harness | Native `message_id` | Turn vocabulary | Streaming | Notable fields |
|---|---|---|---|---|
| Claude Code | yes | `turn_id` per user-prompt cycle | `index`/`final` per message | — |
| Codex | no (synthetic) | `turn_id` per user-prompt cycle | none — `plect.message` only | — |
| Hermes Agent | no (synthetic) | `turn_id` (may be absent), `iteration` as step | delta stream with no sequence number; lossy under backpressure | `interim` (`on_interim_message`), `truncated` (gateway 500-char cut), `agent_id`/`parent_agent_id`/`agent_role`, `surface` |
| pi | no (synthetic) | `turn_index` = one LLM call inside an agent run | typed `message_update` stream | `run_id` (the agent run), `surface` (`ctx.mode`) |
| DeepSeek | yes (`message.id`) | `(turn_index, step_index)` = prompt cycle and model call | delta stream keyed by envelope `seq`, `block_index` for stream position | `run_id` (`SubagentRunId`), `depth` (`delegationDepth`), `source_seq` (envelope `seq`), `stop_reason` (`turn/end.reason.kind`), `model`/`provider` (`source.{provider,model}`); no `agent_role` |
