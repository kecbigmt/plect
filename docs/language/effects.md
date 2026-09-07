# Effects

An effect is a lifecycle-managed component: contracts, health, an optional
interactive capability, and composition.

It brings something up, keeps it observable, and takes it down. It does not
decide whether the work is finished — that is a [task document](tasks.md).

An effect may acquire part of a session environment, including a directory.
There is no workspace-provider lifecycle. A session-scoped checkout or
conversation-directory effect is an ordinary node whose outputs may be chosen
by its workflow's single `workdir` declaration. It does not reserve an output
name, and a workflow with no `workdir` is valid.

## Surface

| Field | Meaning |
|---|---|
| `scope` | `run` or `session`, defaulting to `run`. |
| `setup`, `cleanup` | The lifecycle actions. See [`actions.md`](actions.md). |
| `[cleanup.inputs_schema]` | Optional JSON Schema for plugin-owned destruction inputs. |
| `inputs_schema`, `outputs_schema` | The effect's contracts. |
| `[health]` | The `alive` and `activity` probes. |
| `[terminal]` | The interactive endpoint, if this effect owns one. |
| `inner`, `[inner.inputs]`, `[inner.env]`, `[outputs.bind]`, `locals_schema` | The nesting joint. |

That is the whole grammar. A completion predicate, the keys it reads, its
budget, and the workflows it spawns all belong to a task document.

```toml
[pane]
kind  = "effect"
scope = "run"

[pane.setup]
type = "shell"
script = '''
tmux has-session -t "$session_name" 2>/dev/null \
  || tmux new-session -d -s "$session_name"
printf '{"session_name":"%s"}\n' "$session_name"
'''

[pane.setup.bind]
session_name = { from = "session.name" }

[pane.cleanup]
type   = "shell"
script = 'tmux kill-session -t "$session_name" 2>/dev/null || true'

[pane.cleanup.bind]
session_name = { from = "self.outputs.session_name" }

[pane.health.alive]
type   = "shell"
script = 'tmux has-session -t "$session_name"'

[pane.health.alive.bind]
session_name = { from = "self.outputs.session_name" }

[pane.outputs_schema]
type     = "object"
required = ["session_name"]

[pane.outputs_schema.properties]
session_name = { type = "string" }

[pane_session]
kind = "workflow"

[[pane_session.nodes]]
uses = "pane"
```

A nested effect that declares no `scope` takes the innermost layer's scope.

## Cleanup inputs

Cleanup receives destruction-time intent separately from setup inputs.
`plect down` and `plect destroy` supply the core boolean `force` and an
invocation cleanup-input object. An effect declaring `[cleanup.inputs_schema]`
receives its validated plugin-owned keys as `cleanup.inputs.*`; an effect with
no cleanup input schema receives no such keys. An invocation key that no
cleanup effect declares is an error, and a key supplied to more than one effect
must have the same schema. Creation-time `inputs.*` never substitutes for this
object.

```toml
[checkout_effect]
kind  = "effect"
scope = "session"

[checkout_effect.cleanup]
type   = "shell"
script = 'checkout-tool remove --force="$force" --delete-branch="$delete_branch"'

[checkout_effect.cleanup.bind]
force         = { from = "force" }
delete_branch = { from = "cleanup.inputs.delete_branch", default = false }

[checkout_effect.cleanup.inputs_schema]
type = "object"

[checkout_effect.cleanup.inputs_schema.properties]
delete_branch = { type = "boolean" }
```

Population-driven destruction supplies its declared force policy and no
plugin-owned cleanup input unless the population explicitly supplies that
effect's cleanup-input object. A caller may use the same surface for an
explicit `down` or `destroy`; cleanup intent is not inferred from resource or
workflow creation.

When a workflow declares `workdir`, graph-derived preparation nodes execute
setup, liveness, and cleanup in the session's recorded preparation directory.
A direct invocation records its caller directory; a chain inherits it from its
triggering session; and a population inherits the resident's recorded value.
All other nodes execute setup and liveness in the declared directory. Effects
cannot override either directory per node or action. Cleanup uses the directory
its setup used; a vanished directory is a cleanup failure, not an invitation to
run elsewhere. A probe launch failure caused by that directory invalidates the
node, then cleanup attempts the stored directory and records its failure if it
cannot launch there either. The failed record and allocation remain durable;
an explicit later `up` may reconstruct the invalidated node and its
prerequisites from the latest desired workflow. `--force-recreate` is reserved
for rebuilding a node that has not been invalidated.

## Outputs are production records

An effect's outputs are facts it produced: what setup wrote to stdout, and what a
nesting joint projected outward. They are records of what happened, not a view
of something that keeps changing.

So `[outputs.bind]` reads `inner.outputs.*`, `locals.*`, and this layer's own
`inputs.*` — every one of them fixed when the layer is instantiated. What it
cannot read is a live root: that belongs to a task document's completion
predicate, and re-evaluation semantics exist only there.

<!-- fixture: effects/live-root-output.invalid.toml -->
```toml
[render]
kind  = "effect"
scope = "session"

[render.setup]
type = "exec"
bin  = "github-issue-pr"
args = ["render-instruction"]

[render.health.alive]
type = "noop"

[render.outputs.bind]
checks_status = { from = "resource.state.checks_status" }

[render.outputs_schema]
type = "object"

[render.outputs_schema.properties]
checks_status = { type = "string" }
```

## Health

`[health]` declares this effect's contribution to session health: an `alive`
probe and an `activity` probe, each an action. A setup-bearing effect declares
`[health.alive]`: an executable probe, as below, or, when its produced record
is deliberately never re-observed, `type = "noop"` (see
[`actions.md`](actions.md#noop)). The rule applies independently to every
layer of a nesting chain — a `noop` layer passes the liveness AND without
suppressing an inner layer's own executable probe.

<!-- fixture: effects/health.toml -->
```toml
[pane]
kind = "effect"

[pane.setup]
type   = "shell"
script = 'printf %s "$session_name"'

[pane.setup.bind]
session_name = { from = "session.name" }

[pane.health.alive]
type   = "shell"
script = 'tmux has-session -t "$session_name"'

[pane.health.alive.bind]
session_name = { from = "self.outputs.session_name" }

[pane.health.activity]
type = "shell"
script = '''
PANE=$(tmux capture-pane -p -t "$session_name") || exit 1
FP=$(printf '%s' "$PANE" | cksum | tr -d ' \t')
jq -nc --arg fp "$FP" --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  '{fingerprint: $fp, observed_at: $at}'
'''

[pane.health.activity.bind]
session_name = { from = "self.outputs.session_name" }

[pane.outputs_schema]
type     = "object"
required = ["session_name"]

[pane.outputs_schema.properties]
session_name = { type = "string" }
```

## Terminal

`[terminal]` declares that the effect owns an interactive endpoint, offering
`attach`, `capture`, `send_text`, `send_keys`, and `pid` against it. At most
one effect in a plan declares it. The CLI's attach and capture commands
resolve through it, and the `terminal` capability reaches all five verbs
without the consumer knowing what is behind them.

`send_text` and `send_keys` receive their operand — the literal text to type,
or a key token — as the action's first positional argument. `attach`,
`capture`, and `pid` receive no operand; `pid` returns the endpoint's root
process pid.

<!-- fixture: effects/terminal.toml -->
```toml
[pane]
kind = "effect"

[pane.setup]
type   = "shell"
script = 'printf %s "$session_name"'

[pane.setup.bind]
session_name = { from = "session.name" }

[pane.health.alive]
type   = "shell"
script = 'tmux has-session -t "$session_name"'

[pane.health.alive.bind]
session_name = { from = "self.outputs.session_name" }

[pane.terminal.attach]
type   = "shell"
script = 'tmux attach -t "$session_name"'

[pane.terminal.attach.bind]
session_name = { from = "self.outputs.session_name" }

[pane.terminal.capture]
type   = "shell"
script = 'tmux capture-pane -p -t "$session_name"'

[pane.terminal.capture.bind]
session_name = { from = "self.outputs.session_name" }

[pane.terminal.send_text]
type   = "shell"
script = 'tmux send-keys -t "$session_name" -- "$1"'

[pane.terminal.send_text.bind]
session_name = { from = "self.outputs.session_name" }

[pane.terminal.send_keys]
type   = "shell"
script = 'tmux send-keys -t "$session_name" "$1"'

[pane.terminal.send_keys.bind]
session_name = { from = "self.outputs.session_name" }

[pane.terminal.pid]
type   = "shell"
script = 'tmux display-message -p -t "$session_name" "#{pane_pid}"'

[pane.terminal.pid.bind]
session_name = { from = "self.outputs.session_name" }

[pane.outputs_schema]
type     = "object"
required = ["session_name"]

[pane.outputs_schema.properties]
session_name = { type = "string" }

[pane_session]
kind = "workflow"

[[pane_session.nodes]]
uses = "pane"
```

## Nesting

`inner` makes a definition the outer layer of a nesting chain. Inner and outer
effects are homogeneous, nesting is additive, lifecycle execution is LIFO,
private locals stay layer-local, and inner outputs are never implicitly
promoted into the outer public contract.

The joint reads from the surface being configured: `inner.inputs` is the input
object passed inward, `inner.env` adds environment to the inner effect's
executions, and `outputs.bind` projects or computes the outer effect's public
outputs.

<!-- fixture: nesting/direct-output.toml -->
```toml
[guarded_runtime]
kind  = "effect"
scope = "run"

[guarded_runtime.setup]
type   = "shell"
script = '''
dir=$(mktemp -d)
printf '{"guard_dir":"%s"}\n' "$dir"
'''

[guarded_runtime.health.alive]
type = "noop"

[guarded_runtime.locals_schema]
type     = "object"
required = ["guard_dir"]

[guarded_runtime.locals_schema.properties]
guard_dir = { type = "string" }

[guarded_runtime.inner]
uses = "official.claude.runtime"

[guarded_runtime.inner.inputs]
model        = { from = "inputs.model", default = "fable" }
path_prepend = { from = "locals.guard_dir" }

[guarded_runtime.inner.env]
PLECT_TEAM_CONTEXT = { from = "session.name" }

[guarded_runtime.outputs.bind]
pid         = { from = "inner.outputs.pid" }
socket_path = { from = "inner.outputs.socket_path" }
guard_dir   = { from = "locals.guard_dir" }

[guarded_runtime.outputs_schema]
type     = "object"
required = ["pid", "socket_path"]

[guarded_runtime.outputs_schema.properties]
pid         = { type = "integer", mutable = true }
socket_path = { type = "string", mutable = true }
guard_dir   = { type = "string" }

[guarded_runtime.inputs_schema]
type                 = "object"
additionalProperties = false

[guarded_runtime.inputs_schema.properties]
model        = { type = "string" }
```

Write-through is a property of this joint: a direct projection of a mutable
inner output carries later writes outward, and a computed binding does not.
Nesting is an effect concept only — a task composes in document vocabulary,
never through this joint.

A structured record passes through the joint as data and is serialized by the
inner effect's own fixed logic — it does not become shell source and does not
ride child argv:

<!-- fixture: nesting/structured-record-passthrough.toml -->
```toml
[team_runtime]
kind  = "effect"
scope = "run"

[team_runtime.inner]
uses = "official.claude.runtime"

[team_runtime.inner.inputs]
mcp_servers  = { from = "inputs.mcp_servers", optional = true }

[team_runtime.outputs.bind]
pid         = { from = "inner.outputs.pid" }
socket_path = { from = "inner.outputs.socket_path" }

[team_runtime.outputs_schema]
type = "object"

[team_runtime.outputs_schema.properties]
pid         = { type = "integer", mutable = true }
socket_path = { type = "string", mutable = true }

[team_runtime.inputs_schema]
type                 = "object"
additionalProperties = false

[team_runtime.inputs_schema.properties.mcp_servers]
type = "array"

[team_runtime.inputs_schema.properties.mcp_servers.items]
type                 = "object"
required             = ["name", "command"]
additionalProperties = false

[team_runtime.inputs_schema.properties.mcp_servers.items.properties]
name    = { type = "string" }
command = { type = "string" }
args    = { type = "array", items = { type = "string" } }
env     = { type = "object" }
```

## Validation rules

- The effect grammar is closed: a completion field is not part of it.
- An effect that declares `setup` declares `[health.alive]`.
- `[outputs.bind]` reads `inner.outputs.*`, `locals.*`, or `inputs.*`, and no
  live root.
- A nesting chain that reaches itself is a load error.
- A direct nested projection agrees with the inner output's type and
  mutability.
- A computed nested output is not mutable.
- At most one effect in a plan declares `[terminal]`.
- A dynamic-instantiation target resolving to an effect is a kind mismatch:
  instantiation creates a task instance from a task document.
