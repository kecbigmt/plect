# codex

Codex TUI and `codex exec` launch tasks, initial-prompt/terminal-submit
readiness composition, and the headless exec worker/enqueue pair. Split
out of the former `session/runtime` plugin per
`docs/design/plugin-boundary-contracts.md`; `gh-guard` moved to the
`github` plugin, and `tmux` is a separate, independently selectable plugin
this one composes through `{ terminal = "..." }` — never a direct
dependency.

## Contents

- `config/tasks/codex.toml` — the interactive Codex TUI task. Launch
  keystrokes go through `{ terminal = "send_text" }`/`{ terminal = "send_keys" }`;
  process-tree pid discovery starts from `{ terminal = "pid" }`, and
  TUI-readiness polling reads `{ terminal = "capture" }` — no direct
  multiplexer dependency remains.
- `config/tasks/codex_initial_prompt.toml` — sends a session's initial prompt via
  `{ terminal = "..." }` once the CLI's input box is visible, or on every
  `plect up` when `repeat = "true"`.
- `config/channels/terminal_submit.toml` — an event channel that types a
  later event into the session's terminal via `{ terminal = "..." }`, for a
  runtime with no structured delivery transport. This plugin owns the
  burst-split/retry/readiness composition (see
  `docs/adr/2026-08-17-plugin-boundary-contracts.md`'s Codex Terminal
  Submit) because it describes the Codex TUI contract, not the
  multiplexer's.
- `config/tasks/exec_runtime.toml` + `config/channels/exec_delivery.toml` — the
  headless exec shape: starts `codex-exec-worker` via `{ terminal = "..." }` instead of the interactive TUI, which drains a per-session queue
  directory serially into `codex exec`/`codex exec resume`. A later event
  is delivered by appending to that queue (`codex-exec-enqueue`) rather
  than by typing into a pane, so the submit-race and boot-race classes the
  interactive shape exists to solve do not apply here — there is no input
  box to wedge.
- `scripts/codex-agent-activity` — both halves of the turn-boundary activity
  fingerprint (setting `silence_expected` once a turn ends, and withholding it
  inside a turn): the hook the `codex`/`exec_runtime` effects register, and the
  `probe` verb those tasks declare as their `[health].activity`. For
  `exec_runtime`, `probe` also takes the worker's `state_dir` and folds in its
  current turn's own log file (name and size): unlike the interactive TUI,
  whose pane fingerprint (see the `tmux` plugin) covers a single long turn
  that crosses no hook boundary, `codex exec`'s output never reaches the
  pane, so this is the only mid-turn evidence this shape produces.
- `scripts/codex-exec-worker` — the worker script `exec_runtime.toml`
  launches, resolved through `bin = "<name>"` so it needs no `PATH` entry of
  its own.
- `scripts/codex-exec-enqueue` — the enqueue script `channels/
  exec_delivery.toml` runs, named through that channel's `bin` so it needs no
  `PATH` entry of its own.

## Parameters

Author-declared values a workflow sets to steer these configs without
replacing them (the parameterization rung of
`docs/design/task-nesting.md`'s customization ladder):

| Config | Parameter | Meaning |
|---|---|---|
| `tasks/codex.toml`, `tasks/exec_runtime.toml` | `launch_env` | JSON object of environment variables exported on the launch line. Keys must be valid environment variable names; values are shell-quoted. |
| `tasks/exec_runtime.toml` | `state_root` | Directory the worker's per-session queue and state live under. Empty = a temporary directory. |
| `tasks/codex.toml`, `tasks/exec_runtime.toml` | `launch_timeout` | How long each launch-detection wait gives itself before giving up, as a `"<seconds>s"` token. Default `120s`. On timeout, or any other non-zero setup exit after the process was started, whatever was launched is terminated before the node fails, so a retry never collides with it. |
| `channels/exec_delivery.toml` | `enqueue_timeout` | Per-attempt delivery deadline. Default `5s`. |
| `channels/exec_delivery.toml` | `message_envelope` | Format of the queued message. Placeholders: `{type}`, `{body}`, `{summary}`, `{body_or_summary}`, `{url}`, `{url_suffix}`. Default `[{type}] {body_or_summary}{url_suffix}`. |

These are set on the node or channel binding that selects the declaration, as
values over the workflow surface's own roots. A user-owned workflow names a
plugin's declaration by its catalog address — the alias you enabled this plugin
under, then its plugin path, then the declaration's id:

```toml
[[my_workflow.nodes]]
id   = "agent"
uses = "official.codex.exec_runtime"

[my_workflow.nodes.inputs]
launch_env = '{"PLECT_TEAM_CONTEXT":"acme"}'
state_root = "/var/lib/plect/codex-exec"

[[my_workflow.event.channel]]
name    = "runtime"
uses    = "official.codex.exec_delivery"
include = ["plect.instruction", "resource.*"]

[my_workflow.event.channel.inputs]
queue_dir        = { from = "nodes.agent.outputs.queue_dir" }
enqueue_timeout  = "30s"
message_envelope = "{type}: {body_or_summary}"
```

`docs/language/workflows.md` specifies that surface and
`docs/language/declarations.md` the reference grammar; substitute your own alias
for `official`.

## Install

```bash
plect catalog add official git+https://github.com/kecbigmt/plecture --subdir plugins --revision <tag-or-commit>
plect plugin add official/codex
```

A session using this plugin also needs a `[terminal]`-declaring task in the
workflow (e.g. `official/tmux`'s `tmux` task) — this plugin's own tasks
never declare `[terminal]` themselves.

## Not included

- The write guard for `gh` — see the `github` plugin's `gh_guard` task;
  this plugin's tasks accept only a generic `path_prepend` input, never a
  GitHub-specific switch.
