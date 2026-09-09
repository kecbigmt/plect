# slack

The Slack adapter service, thread binding, and the Slack delivery channel.
Split out of the former `session/runtime` plugin per
`docs/design/plugin-boundary-contracts.md`. Excludes agent runtimes and
channel-server sockets: `slack-adapter` connects to a Unix socket only in
its own test doubles (a protocol-conformant fake it owns —
`src/slack-adapter/internal/adapter/fakesocket_test.go` — built on
`contracts/channel-protocol`), never in production code or by importing
another plugin's package.

## Contents

- `config/workspaces/thread_workspace.toml` — declares the `thread_workspace`
  workspace provider: resolves a Slack thread permalink to a session id and
  acquires/releases a bare workspace directory for it, with no GitHub
  artifact standing in for the session. See "Workspace provider" below.
- `config/tasks/slack_thread.toml` — creates one Slack root message through
  `slack-adapter`. Outputs: `thread_ts`, `channel_id`, and `permalink`.
- `config/tasks/slack_subscribe.toml` — run-scoped effect that registers a
  thread ↔ runtime binding with `slack-adapter` (`POST /subscribe`, undone
  with `DELETE /subscribe` on cleanup), so an `app_mention` in the thread
  reaches the session over Socket Mode. Inputs: `base_url`, `thread_ts`,
  `channel_id`, and `socket_path` (an opaque string — wire it from whatever
  runtime task exposes its channel-server socket). Run scope, not session
  scope, because the socket path changes on every agent launch while the
  thread does not. Because this task's cleanup runs `DELETE /subscribe` on
  every `plect down`, `slack-adapter`'s own state directory (not this
  plugin's config) must live on storage that survives a redeploy — see
  "Persistence" in `src/slack-adapter/README.md`.
- `config/channels/slack.toml` — delivers a session event to its Slack
  thread by posting to the `slack-adapter` service's HTTP API. Inputs:
  `base_url` (e.g. `http://127.0.0.1:7890` for its default `listen_addr`),
  `channel_id`, and `thread_ts` (typically wired from whatever task created
  the thread via slack-adapter's `POST /threads`).
- `config/channels/status.toml` — same inputs and delivery mechanics as
  `slack.toml`, but posts to `POST /status` instead of `POST /messages`: the
  event's text becomes the thread's shimmer status line instead of a posted
  message. Carries no event-type logic of its own — the composing workflow's
  `include` list decides which events reach it. An event's body-or-summary
  becomes the sole `loading_messages` entry; an event whose body and summary
  are both empty clears the status instead.
- `config/channels/stream.toml` — accepts both `plect.message_delta` and
  `plect.message`, and renders either as one live-updating Slack thread
  reply via the `slack-adapter` service's `POST /stream`. Same
  `base_url`/`channel_id`/`thread_ts` inputs as `slack.toml`; `stream_key`,
  `text`, `index`, and `final` are read straight from the delivered event's
  own fields (`event.metadata.message_id`, `event.body`,
  `event.metadata.index`, `event.metadata.final`, the last two defaulting to
  `0`/`true` for a `plect.message`, which carries no chunk sequence of its
  own) rather than declared as inputs, since an `[[event.channel]].inputs`
  binding has no access to the event being delivered
  (`docs/language/values.md`) and only the channel's own action does. This
  channel runs unconditionally (an `exec` action always runs), so it cannot
  itself skip a `plect.message` whose deltas already rendered and closed the
  stream; slack-adapter's `StreamManager` is what keeps that from posting a
  second Slack message under the same `stream_key` — see its own comment
  (`src/slack-adapter/internal/adapter/stream.go`) and
  `src/slack-adapter/README.md`'s `POST /stream` section for the
  chat.startStream/appendStream/stopStream sequencing and the
  fallback-to-a-single-post behavior when a workspace rejects streaming.
- `config/resources/thread.toml` — declares the `thread` resource observer:
  a `[thread.query]` face whose `subscribe` means is the workflow
  population source for an unbound Slack mention (see "Query (population
  source)" below). Distinct from the `thread_workspace` provider above —
  same permalink, different responsibility.
- `src/slack-adapter/` — Slack-specific message relay + subscription
  broker. See `src/slack-adapter/CLAUDE.md`.

## Install

```bash
plect catalog add official git+https://github.com/kecbigmt/plecture --subdir plugins --revision <tag-or-commit>
plect plugin add official/slack
```

Building `slack-adapter` requires a Go toolchain (see the Package format
section of `docs/design/plugin-packaging.md`); `plect plugin add`/`update`
builds it automatically.

## Resident-supervised Services

`slack-adapter` is declared as a `[[services]]` in `plugin.toml`,
supervised by `plect serve` (start, crash-restart with backoff, stop with
the resident process). Outbound thread creation and message posting require
only `SLACK_BOT_TOKEN`; `SLACK_APP_TOKEN` is optional and only activates
Socket Mode inbound relay.

## Workspace provider

`config/workspaces/thread_workspace.toml` resolves a thread's Slack permalink —
either the root message's own permalink or a reply's permalink carrying
`?thread_ts=<root>&cid=<channel_id>` — to a session named
`slack/<channel_id>-<root thread_ts digits>`, so both forms of the same
thread's permalink resolve to the same session. `setup` creates a bare
directory under `config.workspace_dirs_root` and reports the same
`channel_id`, `thread_ts`, and `permalink` outputs `slack_thread` reports, so
the shipped `slack`/`status` channels and `slack_subscribe` effect bind from
a session this provider backs exactly as they would from one `slack_thread`
created. `cleanup` removes the directory. Dependencies: `plect` and a POSIX
shell — no git, and no `slack-adapter` HTTP call, so a workflow that wants a
repository checkout composes a separate effect for it.

Unlike `official.github.worktree`, this provider declares no
`subscribe`/`unsubscribe` hook: the slack plugin owns no watcher a session
could bind to or drop a binding from for a thread resource.

## Thread contract

`slack_thread` posts whatever `root_text` input it is handed as the root
message and reports the adapter permalink, `thread_ts`, and `channel_id` as
outputs. It carries no opinion about what the thread is for — review, or
anything else — and no PR-specific inputs: how the root text is framed is
the composing workflow's concern.

`examples/review-thread-workflow.toml` shows the review case: it composes
`root_text` from a PR title, URL, and head sha, then wires `slack_thread`
outputs into the `slack` channel bound with `include =
["plect.judge.recorded"]`. Its conclusion event is the `plect.judge.recorded`
event that plect appends when a judge records a done_when verdict; the body
is the recorded judge reason, so the Slack reply text is exactly the
conclusion the judge recorded, and metadata carries `instance`, `leaf_id`,
`action`, `revision`, `judge_session`, `judge_workflow`, and
`relation`. The posted text is the event body, falling back to the summary
only for legacy events without a body; progress, heartbeats, terminal events,
and GitHub watcher events do not match that binding.

The same example also shows the binding for the `status` channel, bound to
`plect.status_message` with the same three inputs as the `slack` binding
above (`base_url`, `channel_id`, `thread_ts`) — no per-event input, because
an `[event.channel.inputs]` binding resolves from session/node outputs only
and has no access to the event being delivered; only the channel's own
action does (see `channels/status.toml`).

It also shows the `stream` channel bound with `include = ["plect.message",
"plect.message_delta"]` and the same three inputs again. One such binding is
enough for every runtime a workflow composes, streaming or not: a
Claude-style delta-then-message sequence and a Codex-style message-only one
both post through it, and telling a redundant `plect.message` apart from
one that needs posting is `StreamManager`'s job
(`src/slack-adapter/internal/adapter/stream.go`), not the workflow's. A
workflow never needs a separate binding — or a separate channel — per
runtime, and `plect.message` is not reserved for the `slack` channel.

### Verified channel-thread rendering facts

Confirmed empirically against real Slack workspaces (bot scopes as shipped,
no `features.assistant_view`): in a bound **channel thread**,
`assistant.threads.setStatus`'s `status` string is never rendered — Slack
shows its own localized default text instead — and only `loading_messages`
entries render. A `loading_messages` entry sent right after a prior
status-only call on the same thread flashes once and reverts to the
default text; the same entry sent right after an explicit clear (`status:
""`) renders persistently. This is why `StatusManager.Set` always clears
before it sets, and why `status_text` is not a config option: only
`loading_messages`, supplied per-event via the `status` channel, ever
reaches the thread.

## Presentation-only exceptions

`slack-adapter`'s deprecated `/notify` rollback path (see
`src/slack-adapter/CLAUDE.md`) inspects a GitHub-derived `change_type` to
pick an emoji prefix, falling back to a generic prefix for anything else.
This is a display detail of an already-deprecated path, not a workflow
commitment the config layer makes, so it is left as is rather than
generalized.

## Query (population source)

`config/resources/thread.toml` declares the `thread` resource observer's
`[thread.query]` face
(`docs/adr/2026-09-05-standing-session-dispatch.md`, "Subscribe-only
observer sketch"): the item source a workflow's `[[workflow.populations]]`
entry binds to keep a session present for a Slack thread that received an
unbound `app_mention`. It declares only `subscribe` — a mention is not
enumerable, so there is no `poll` means, and a population entry using this
observer always sets `expire_after` rather than `poll_every`.

- **`slack-adapter subscribe unbound-mentions`** (see
  `src/slack-adapter/README.md`) is the subscribe means: it never opens a
  second Socket Mode connection, instead reading the resident adapter's own
  `/unbound-mentions` feed and converting each `unboundMentionItem` straight
  into the query's item shape: `resource` (the mention's permalink,
  required) plus `channel_id`, `thread_ts`, and `mention_ts` context.
- **`slack-adapter resource observe`** is `thread`'s `observe` action. The
  language requires every `resource_observer` to declare one, but this
  observer's `state_schema` is empty — a mention appearance is already the
  only fact `query.subscribe` reports — so it prints `{}` rather than
  fabricating a state key the schema does not declare.

## Not included

- Which agent runtime delivers into Slack, or vice versa — a workflow's
  concern, composed by wiring this plugin's channel and any agent-runtime
  plugin's own tasks/channels together. This plugin never connects to a
  channel-server socket or imports an agent-runtime plugin's package.
- slack-adapter's Slack App manifest, HTTP API surface, and subscription
  broker behavior — see `src/slack-adapter/CLAUDE.md`.
