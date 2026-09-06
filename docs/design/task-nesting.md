# Task nesting

This design is governed by
[`../adr/2026-08-17-task-nesting.md`](../adr/2026-08-17-task-nesting.md), with
its gate-binding rules narrowed by
[`../adr/2026-08-22-retire-nested-gate-binding-rules.md`](../adr/2026-08-22-retire-nested-gate-binding-rules.md).

## Design Core

Inner and outer are homogeneous, so composition is closed. From the outside a
nested effect is exactly an effect: it presents the surfaces a plain effect
presents — an inputs schema, public outputs, `[health]`, and `[terminal]` —
and nothing that consumes effects needs to know whether nesting is inside it.
Workflows, downstream nodes, status, and the orchestrator address a nested
effect the way they address any other.

The joint is the only nesting-specific vocabulary: `[inner]` names the next
effect inward and carries its inputs and environment, `locals` holds the
joint's private intermediates, and `[outputs.bind]` wires the boundary
outward. No other nesting-only concept exists.

Nesting therefore lets an outer effect add a layer of its own to an inner
effect without copying the inner effect's file. Any layer that owns effect
definitions writes outer effects: user-owned configuration, a
configuration-only plugin, and a plugin factoring its own effects, such as
runtime variants that share a common inner layer. The rules in this design do
not vary with the layer that owns the outer effect.

Closure decides the field rules, so they are corollaries rather than a list of
exceptions. Whatever a plain effect declares, an outer effect declares for its
own layer, and layers compose additively: `[health]` by AND on `alive` and OR
on `activity`. Each layer declares only its own additions and answers only for
them, which is what makes the no-override rule hold by construction.

No effect field is unconditionally inner-owned, so closure is realized in full.
What remains are conflict rules rather than ownership rules: `[terminal]`
admits at most one declaration per nesting chain, judge ids and `[inner.env]`
keys are unique across the chain, and a public output name has one definition source
within its layer.

A nested effect has a chain of effect definitions. The outermost is the id
named by a workflow node. Each outer effect names its next inner one through
`[inner].uses`, and that inner effect may itself be nested. The innermost
remains unaware that it is nested.

The lifecycle is an N-layer LIFO stack:

```text
outermost setup -> ... -> innermost setup -> innermost cleanup -> ... -> outermost cleanup
```

The nested effect declares only the public outputs the outer one explicitly
binds or produces. Inner public outputs are not passed through automatically.
The outer effect may re-export inner outputs, rename them, or bind values
computed by its own setup, but downstream consumers read only the outer
effect's declared public contract. Outer setup emits locals: always-private intermediate
values available to outer cleanup, to the inner input and environment values,
and to public output binding. Public output binding is a live projection, not a
setup-time copy: it resolves against the current inner output and local values.
A direct projection of an inner output routes mutable writes to that inner
output; a computed binding produces a value of its own and is read-only.

## Configuration Shape

An outer effect is a definition under a `tasks/` root declaring `inner`, plus
the joint tables that wire it:

```toml
[team_claude]
kind = "effect"

[team_claude.inner]
uses = "official.claude.runtime"

[team_claude.setup]
type   = "shell"
script = "jq -nc --arg guard_dir \"$guard_dir\" '{guard_dir:$guard_dir}'"

[team_claude.setup.bind]
guard_dir = { from = "nodes.gh_guard.outputs.dir" }

[team_claude.cleanup]
type   = "shell"
script = "true"

[team_claude.outputs.bind]
pid           = { from = "inner.outputs.pid" }
socket_path   = { from = "inner.outputs.socket_path" }
mcp_config    = { from = "inner.outputs.mcp_config" }
agent_session = { from = "inner.outputs.session_id" }
guard_dir     = { from = "locals.guard_dir" }

[team_claude.inner.inputs]
tmux_session = { from = "inputs.tmux_session" }
model        = { from = "inputs.model", optional = true }
effort       = { from = "inputs.effort", optional = true }
path_prepend = { from = "locals.guard_dir" }

[team_claude.inner.env]
PLECT_TEAM_CONTEXT = { from = "session.name" }

[team_claude.inputs_schema]
type = "object"
required = ["tmux_session"]
additionalProperties = false

[team_claude.inputs_schema.properties]
tmux_session = { type = "string" }
model        = { type = "string", pattern = "^[A-Za-z0-9_.:-]*$" }
effort       = { type = "string", pattern = "^[A-Za-z0-9_.:-]*$" }

[team_claude.locals_schema]
type = "object"
required = ["guard_dir"]
additionalProperties = false

[team_claude.locals_schema.properties]
guard_dir = { type = "string" }

[team_claude.outputs_schema]
type = "object"
required = ["pid", "socket_path", "mcp_config", "agent_session", "guard_dir"]

[team_claude.outputs_schema.properties]
pid = { type = "integer", mutable = true }
socket_path = { type = "string", mutable = true }
mcp_config = { type = "string" }
agent_session = { type = "string", mutable = true }
guard_dir = { type = "string" }
```

The outer effect's `inputs_schema` is its own schema. It is not an edit to the
inner effect's schema. Workflow node inputs validate against the outer schema,
then `[inner.inputs]` resolves the inner input object and validates it against
the inner effect's schema.

The `[inner.env]` table injects environment variables into process executions
owned by the inner effect: setup, cleanup, `[health]` probes, and terminal
operation commands. It does not affect the outer effect's hooks, any sibling
effect, or commands the inner effect sends into an interactive endpoint. Agent runtimes that launch by typing into a terminal
therefore need an author-declared input for launch-line environment exports.

Among the terminal operations, the injection reaches the ones plect runs
itself: `capture` and `pid`. `send_text` and `send_keys` resolve into
another execution through a `{ terminal = "..." }` binding and carry
whatever environment that execution runs with. `attach` resolves to a
command string the caller's own shell executes, and carries the caller's
environment: placing bound values there would mean synthesizing quoted
assignments into that command string, and this design specifies no quoting
for it. A runtime needing bound environment on the attach line declares an
author-declared launch input for it, the same rung-1 answer
terminal-launched agent runtimes already take.

Nesting fields:

| Field | Required | Type | Meaning |
|---|---:|---|---|
| `[inner]` | yes | table | `uses` is the reference to the nested inner effect. |
| `[setup]` | no | action | Outer setup hook. Absent means `{}` locals. |
| `[cleanup]` | no | action | Outer cleanup hook. Absent marks the outer layer cleaned. |
| `[inner.inputs]` | no | value table | The values that produce the inner effect's inputs. |
| `[inner.env]` | no | value table | The values that produce the inner effect's process environment additions. |
| `[outputs.bind]` | no | value table | The values that bind the nested effect's public outputs from inner outputs or locals. Re-exports and renames are explicit here. |
| `[inputs_schema]` / `inputs_schema_file` | no | JSON Schema | The outer effect's workflow-facing inputs contract. |
| `[locals_schema]` / `locals_schema_file` | no | JSON Schema | The private locals contract for outer setup emissions. |
| `[outputs_schema]` / `outputs_schema_file` | no | JSON Schema | The nested effect's explicit public output contract. Every public output is declared here. |
| `[terminal]` | no | terminal table | An interactive endpoint for a nesting chain whose other layers declare none. |
| `[health]` | no | health table | Liveness and activity probes for the resources this layer brings up. |

A layer declaring `[setup]` declares `[health].alive`, as an executable probe
or as `type = "noop"`; the rule applies independently to every layer, and a
`noop` layer does not suppress an inner layer's own executable probe. See
[`health-declaration.md`](health-declaration.md).

The nested effect's effective scope is the innermost one's scope. If an outer
effect declares `scope`, it must match the scope of its next inner effect, and
the rule repeats down the nesting chain.
The inner effect's `self.outputs` sees only its own outputs. `[inner.inputs]`,
`[inner.env]` and `[outputs.bind]` read outer setup values from `locals`;
`[outputs.bind]` also reads inner setup values from `inner.outputs`.
An `[outputs.bind]` entry is a direct inner-output binding only when it is a
projection — `{ from = "inner.outputs.<key>" }` — and nothing else. An
expression, or a projection of anything but an inner output, makes the binding
computed.
A direct projection carries the inner output's native value, so integer,
boolean, object, and array outputs keep their schema type. A computed binding
produces a value of its own, so its public schema type is whatever the
expression yields.
Every public key named by `[outputs.bind]` must appear in the effective
`outputs_schema`, and every public field is declared explicitly by the outer
schema. A same-name re-export such as `pid = { from = "inner.outputs.pid" }`
still needs a local schema property.

Inspection output such as `plect task show` prints the nesting chain from the
outermost effect to the innermost plugin effect.

An effect owns no completion predicate and no chain: what a session owes and
what follows from it are a task document's, and a document has no nesting
joint. Composition is therefore about lifecycle alone — what a layer brings up
and takes down, the endpoint it offers, and the health it reports.

## Interactive Endpoint

Any one layer of a nesting chain may declare `[terminal]`. An outer effect
declaring it over an inner chain that declares none composes an interactive
surface around a headless inner effect, which is addition rather than
modification. Two layers of one chain declaring `[terminal]` is a load error
naming both layers.

The per-chain rule is one this design introduces, and it is what keeps plan
assembly's existing count sound. Plan assembly rejects a plan whose nodes
declare more than one `[terminal]`, counting nodes; a nesting chain sits inside
one node, so without a per-chain rule two layers could place two declarations
behind a single node and pass that count. With it, a nested effect contributes one
terminal declaration or none, and terminal-layer resolution for attach, capture,
and a `{ terminal = "..." }` binding needs no nesting-aware tie-break.

Whichever layer declares `[terminal]`, the composed public contract carries
`interactive_endpoint` bound from that layer. When the inner chain declares it,
`[outputs.bind]` binds `interactive_endpoint` from
`{ from = "inner.outputs.interactive_endpoint" }`. When the outer effect
declares it, the
outer binds `interactive_endpoint` from its own setup-emitted local. A chain
with a `[terminal]` declaration whose composed public contract does not bind
`interactive_endpoint` is a load error either way.

Terminal operation commands belong to the declaring layer's `[terminal]` table.
Other layers do not inject, wrap, or replace them, and consumers reach the
declaring layer only through the composed public contract.

## Layer Health

`setup`, `cleanup`, and `[health]` are one lifecycle trio, and each layer of a
nesting chain owns its own. An outer effect declares `[health]` for the resources
that layer brings up.

Composition across the layers of a chain has the same dual shape as composition
across instances. `alive` is the AND: any failing layer makes the composed effect
unhealthy, and the report names the failing layer. `activity` is the OR:
evidence from any layer counts. A layer declaring no `[health]` contributes
nothing to either composition — vacuous in the AND, no vote in the OR — and
neither does one whose activity probe emits no envelope. Each layer declares
only its own probes and never replaces another layer's, so composition needs no
conflict rule.

A layer earns an activity probe only when its activity indicates session
progress. A chatty sidecar layer with an activity probe would mask a stalled
agent.

Probe semantics and the activity envelope are specified by
[`health-declaration.md`](health-declaration.md).

## Reference Resolution

`[inner].uses` accepts a relative effect id or a catalog-qualified reference:

```toml
[team_claude.inner]
uses = "runtime"
```

```toml
[team_claude.inner]
uses = "official.claude.runtime"
```

The relative form resolves in the namespace it is written in: a plugin's own
effect when a plugin wrote it, and the user-owned layer stack otherwise. The
catalog-qualified form selects an effect inside an enabled plugin: its final
segment is the definition id and the segments before it are the alias and the
plugin path, which must name an enabled plugin.

Workflow node `uses` accepts the same two forms:

```toml
[[my_review.nodes]]
id   = "runtime"
uses = "official.claude.runtime"
```

A user-owned workflow names a plugin's effect by that address; naming the id
alone selects a user-owned effect of that id, if one exists, and otherwise
resolves to nothing. Shipped plugin workflows use relative ids because catalog
aliases are user-local.

A plugin's own outer effect names its inner effect with the relative form,
which resolves in that plugin's own namespace. Cross-plugin nesting is
excluded by the reference grammar rather than by a validation rule: naming
another plugin's effect requires the catalog-qualified form, and catalog
aliases are user-local, so plugin-authored config has no vocabulary for
writing one.
Catalog-qualified plugin identity, and the user-chosen local alias it starts
with, are specified by [`plugin-packaging.md`](plugin-packaging.md).

## Validation Rules

Loading nested effect definitions fails when:

- `[inner].uses` names an unknown effect, an unknown catalog alias, a disabled
  plugin, or an effect missing from the selected plugin.
- `[inner].uses` forms a nesting cycle, including self-reference.
- the outer effect declares a `scope` that differs from the inner effect's.
- two layers of the nesting chain declare `[terminal]`.
- `[outputs.bind]` reads a root other than `inner.outputs`, `locals`, or
  `inputs`.
- `[outputs.bind]` declares a public key missing from `outputs_schema`.
- `[outputs.bind]` declares a computed output mutable in `outputs_schema`.
- `[outputs.bind]` declares a direct inner-output binding whose
  `outputs_schema` type differs from the bound inner output's type.
- `[outputs.bind]` declares a direct inner-output binding mutable in
  `outputs_schema` when the bound inner output is not mutable.
- `[inner.inputs]` omits an inner required input or binds a key rejected by a
  closed inner schema.
- an `[inner.env]` key is not a valid process environment name.
- an `[inner.env]` key repeats a key from any other layer in the nesting chain.
- a layer of the nesting chain declares `[terminal]` and the outer public
  contract does not bind `interactive_endpoint` from that layer.

Running a nested effect fails when:

- the outer setup exits non-zero or emits locals rejected by the locals schema;
- a joint value does not resolve, declaring neither a default nor `optional`;
- the resolved inner inputs fail the inner effect's schema value validation;
- a public output binding does not resolve, or produces a value rejected by the
  public outputs schema;
- any inner effect process fails under the normal rules for one.

Cleanup skips layers that never reached setup. If an inner setup fails after an
outer setup succeeds, the next cleanup unwinds each produced outer layer.

## Customization Ladder

Task nesting is the middle rung:

1. Parameterization: a plugin effect author declares a supported input.
2. Effect nesting: an outer effect adds lifecycle, locals, env, an interactive
   endpoint, input binding, and explicit output binding. Completion conditions
   and chains are a task document's, which a layer replaces by id rather than
   nests.
3. Fork: a user copies the whole effect because the required behavior is not an
   author-declared variation point and cannot be added outside it.

Workflow node inputs remain the home for user default values. Script-internal
variation remains parameterization work for the plugin effect author.
Author-declared parameters are data-shaped: values, paths, naming templates,
booleans, environment maps, and structured records the effect serializes into
data for the program it launches. They are never behavior-shaped script hooks,
executable substitution, or output-shape hooks; a behavior-shaped parameter
hides a shadow behind a keyhole and breaks final-by-default ownership.

What separates the two is where the value lands, not what it names. A
parameter must never reach the effect's own lifecycle commands or shell source.
A structured record that the effect serializes, through an author-fixed
mechanism, into data consumed by the inner program — an agent's config file,
for instance — is data even when the record's fields include a command for
that program to manage: the effect's own execution is identical either way, so
there is no shadow behind the keyhole. `path_prepend` sits on the same side of
the line while being the more behavioral of the two, which is what makes the
criterion consistent with it: a prepended path changes which binary the effect's
own commands resolve to, where a serialized registration record changes only a
file the launched program reads.

## Plugin-Counterpart Shadow Mapping

The evaluation set contains six production shadows with plugin counterparts.
Runtime-argument wiring for a retiring third-party service is outside this
mapping. Explicit outer-output binding applies to effect definitions; workspace,
resource, and channel shadows resolve through plugin parameterization or
adoption in their owning config kinds.

| Shadow | Durable intent | Rung | Zero-copy path |
|---|---|---:|---|
| `tasks/runtime.toml` | Different runtime inputs, extra agent-process env, path injection. | 1 + 2 | Rung 2 binds model/effort/path inputs. Rung 1 in the `claude` plugin provides an author-declared `launch_env` input whose exports are included in the terminal launch line. Workflow-owned defaults stay in the workflow node. |
| `tasks/codex.toml` | Different runtime inputs, extra agent-process env, path injection. | 1 + 2 | Rung 2 binds the plugin effect's inputs. Rung 1 in the `codex` plugin provides an author-declared `launch_env` input whose exports are included in the Codex TUI launch line. |
| `tasks/exec_runtime.toml` | Different runtime inputs, extra worker-process env, path injection, worker state location. | 1 + 2 | Rung 2 binds runtime/path inputs. Rung 1 in the `codex` plugin provides author-declared `launch_env` and `state_root` inputs for the worker's exported environment and state directory. |
| `channels/exec_delivery.toml` | Queue timeout and message formatting around the shipped enqueue executable. | 1 | The channel adopts the plugin's shipped enqueue executable. Rung 1 in the `codex` plugin declares `enqueue_timeout` and `message_envelope`; `message_envelope` is a formatting template over the existing event fields, not executable substitution. |
| `workspaces/worktree.toml` | Workspace layout root, branch naming, cleanup policy, and local-only status outputs. | 1 | Rung 1 in the `github` plugin declares `workspace_layout_root`, `issue_branch_template`, `tagged_branch_suffix`, and `delete_branch_default`. The plugin already owns `title`; the local-only `checks_status` output is dropped from workspace setup because resource observation owns CI status. |
| `resources/github.toml` | Script-internal observation drift with the same public state keys, plus review-state observation needed outside workspace setup. | As-is + 1 | Adopt the plugin resource implementation as-is for the existing state keys. Rung 1 in the `github` plugin adds the concrete `review_decision` observed state key; no open-ended resource output hook is needed. |

The mapping leaves no listed plugin-counterpart shadow on rung 3. Configs with
no plugin counterpart, such as local envfile, chat thread binding, initial task,
agent docs, orchestrator workspace, and the `github` plugin's
task instructions (`work`, `review`, `respond`, `investigate` — the plugin
ships none of them), are user-owned config and are outside the shadow count.

## Worked Example

A team-local Claude runtime customization can stay small:

```toml
[team_claude]
kind = "effect"

[team_claude.inner]
uses = "official.claude.runtime"

[team_claude.setup]
type   = "shell"
script = "jq -nc --arg guard_dir \"$guard_dir\" '{guard_dir:$guard_dir}'"

[team_claude.setup.bind]
guard_dir = { from = "nodes.gh_guard.outputs.dir" }

[team_claude.cleanup]
type   = "shell"
script = "true"

[team_claude.outputs.bind]
pid           = { from = "inner.outputs.pid" }
socket_path   = { from = "inner.outputs.socket_path" }
mcp_config    = { from = "inner.outputs.mcp_config" }
agent_session = { from = "inner.outputs.session_id" }
guard_dir     = { from = "locals.guard_dir" }

[team_claude.inner.inputs]
tmux_session = { from = "inputs.tmux_session" }
model        = { from = "inputs.model", optional = true }
effort       = { from = "inputs.effort", optional = true }
path_prepend = { from = "locals.guard_dir" }

[team_claude.inner.env]
PLECT_TEAM_CONTEXT = { from = "session.name" }

[team_claude.inputs_schema]
type = "object"
required = ["tmux_session"]
additionalProperties = false

[team_claude.inputs_schema.properties]
tmux_session = { type = "string" }
model = { type = "string", pattern = "^[A-Za-z0-9_.:-]*$" }
effort = { type = "string", pattern = "^[A-Za-z0-9_.:-]*$" }

[team_claude.locals_schema]
type = "object"
required = ["guard_dir"]
additionalProperties = false

[team_claude.locals_schema.properties]
guard_dir = { type = "string" }

[team_claude.outputs_schema]
type = "object"
required = ["pid", "socket_path", "mcp_config", "agent_session", "guard_dir"]

[team_claude.outputs_schema.properties]
pid = { type = "integer", mutable = true }
socket_path = { type = "string", mutable = true }
mcp_config = { type = "string" }
agent_session = { type = "string", mutable = true }
guard_dir = { type = "string" }
```

The same pattern covers effect-shaped shadows whose differences are input values,
chain declarations, added completion conditions, explicit output binding, or a
single command path expressed as an environment/path input. Agent-process
environment for terminal-launched runtimes belongs to author-declared launch
inputs on the runtime plugin. Workspace layout belongs to the workspace
provider or workflow node inputs. Script-internal command changes belong to
parameterization; if no author-declared variation point exists, a fork remains
the explicit last resort.
