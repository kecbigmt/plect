# claude

Claude Code launch task, initial-prompt submit/readiness composition, and
structured delivery via the channel-server daemon. Split out of the former
`session/runtime` plugin per `docs/design/plugin-boundary-contracts.md`;
`gh-guard` moved to the `github` plugin, and `tmux` is a separate,
independently selectable plugin this one composes through `{ terminal = "..." }` — never a direct dependency.

## Contents

- `config/tasks/runtime.toml` — the `runtime` effect. `setup` launches `claude`
  (fresh or `--resume`, with a same-session-id fallback if resume finds no
  persisted conversation) via `{ terminal = "send_text" }`/`{ terminal = "send_keys" }`, waits for it to come up by polling
  `~/.claude/sessions/*.json`, wires a channel-server MCP socket when
  `channel-server` is on `PATH`, and registers turn-boundary activity
  hooks. `[health].alive` self-heals a stale pid by re-deriving the live
  process from the pane's process tree, reached via
  `{ terminal = "pid" }` for the pane's own root process id, so a
  crash-and-relaunch or a manual `--resume` does not require a session
  down/up to keep event delivery working. `[health].activity` reads back the
  record those turn-boundary hooks write, so hook and probe are two halves of
  one fingerprint format.
- `config/tasks/claude_initial_prompt.toml` — sends a session's initial prompt via
  `{ terminal = "..." }` once the CLI's input box is visible, or on every
  `plect up` when `repeat = "true"`.
- `config/channels/delivery.toml` — the `delivery` channel: it delivers a session event to the running
  Claude Code process over its channel-server Unix socket.
- `scripts/claude-mcp-servers` — the author-fixed serialization step behind the
  `runtime` effect's `mcp_servers` parameter: it merges the declared registration
  records into the MCP config that task assembles, and fails the launch on a
  malformed record rather than dropping it.
- `scripts/claude-agent-activity` — both halves of the turn-boundary activity
  fingerprint (setting `silence_expected` once a turn ends, and withholding it
  inside a turn): the hook the `runtime` effect registers, and the `probe` verb
  that task declares as its `[health].activity`.
- `src/channel-server/` — generic message delivery to Claude Code, with no
  knowledge of message sources (Slack or otherwise). See
  `src/channel-server/CLAUDE.md`.

## Parameters

Author-declared values a workflow sets to steer these configs without
replacing them (the parameterization rung of
`docs/design/task-nesting.md`'s customization ladder):

| Config | Parameter | Meaning |
|---|---|---|
| `tasks/runtime.toml` | `launch_env` | JSON object of environment variables exported on the launch line. Keys must be valid environment variable names; values are shell-quoted. |
| `tasks/runtime.toml` | `mcp_servers` | JSON array of MCP server registration records — `{name, command, args?, env?}` — merged into the `--mcp-config` JSON alongside this task's own registrations. A record only ever reaches the agent's config file, never a command line; a name that collides with a registration the task already made, or a record missing `name`/`command`, fails the launch. |
| `tasks/runtime.toml` | `launch_timeout` | How long the launch poll waits for `claude`'s session file to register, as a `"<seconds>s"` token. Default `120s`. On timeout, or any other non-zero setup exit after the process was started, the pane's `claude` child is terminated before the node fails, so a retry never types into a still-live process. |

These are set on the node or channel binding that selects the declaration, as
values over the workflow surface's own roots. A user-owned workflow names a
plugin's declaration by its catalog address — the alias you enabled this plugin
under, then its plugin path, then the declaration's id:

```toml
[[my_workflow.nodes]]
id   = "agent"
uses = "official.claude.runtime"

[my_workflow.nodes.inputs]
launch_env   = '{"PLECT_TEAM_CONTEXT":"acme"}'
mcp_servers  = '[{"name":"kbn","command":"kbn-mcp","args":["--scoped"]}]'

[[my_workflow.event.channel]]
name    = "runtime"
uses    = "official.claude.delivery"
include = ["plect.instruction", "user.emit"]

[my_workflow.event.channel.inputs]
path = { from = "nodes.agent.outputs.socket_path" }
```

`docs/language/workflows.md` specifies that surface and
`docs/language/declarations.md` the reference grammar; substitute your own alias
for `official`.

## Install

```bash
plect catalog add official git+https://github.com/kecbigmt/plecture --subdir plugins --revision <tag-or-commit>
plect plugin add official/claude
```

Building `channel-server` requires a Go toolchain (see the Package format
section of `docs/design/plugin-packaging.md`); `plect plugin add`/`update`
builds it automatically.

A session using this plugin also needs a `[terminal]`-declaring task in the
workflow (e.g. `official/tmux`'s `tmux` task) — this plugin's own tasks
never declare `[terminal]` themselves.

## Resident-supervised Services

`channel-server` is declared as a `[[services]]` in `plugin.toml`,
supervised by `plect serve` (start, crash-restart with backoff, stop with
the resident process). Its declaration stays inert today by design: real
instances are per-session, launched by Claude Code itself via MCP
configuration with a session-specific `CHANNEL_SOCKET_PATH` the resident
process never has — see `plugin.toml`'s comment and
`docs/adr/2026-08-16-plugin-service-lifecycle.md`'s Consequences section.

## Not included

- Which agent CLI a session launches beyond Claude Code, and any model/
  effort defaults for other CLIs — a workflow's concern.
- The write guard for `gh` — see the `github` plugin's `gh_guard` task; this
  plugin's `runtime` effect accepts only a generic `path_prepend` input, never
  a GitHub-specific switch.
- A no-channel-server interactive Claude configuration is outside the
  supported surface — see `docs/migrations/` for the migration procedure.
