# The Plecture configuration language

Plecture configuration is a language of its own, built from established
substrates. TOML is its serialization syntax, JSON Schema its contract
language, and CEL its computation sublanguage. Everything else — definitions,
identity, references, values, actions, lifecycle, layers, nesting, plugins,
trust, and composition — is Plecture semantics.

```text
TOML                structural representation
JSON Schema         data contracts
CEL                 computation
Plecture            definitions, references, values, actions, lifecycle,
                    layers, nesting, plugins, trust, validation, composition
```

Plecture introduces no syntax that requires a custom TOML parser, and no
general-purpose expression language of its own.

## The chapters

| Chapter | Subject |
|---|---|
| [`tasks.md`](tasks.md) | The task document: the completion contract and the instruction |
| [`chains.md`](chains.md) | Chains: what a task document spawns, and when |
| [`effects.md`](effects.md) | The effect kind: lifecycle, health, terminal, and nesting |
| [`workflows.md`](workflows.md) | The workflow kind: nodes, event channels, display, and the clocks |
| [`channels.md`](channels.md) | The channel kind and its delivery primitives |
| [`workspace-providers.md`](workspace-providers.md) | The workspace provider kind |
| [`resource-observers.md`](resource-observers.md) | The resource observer kind |
| [`declarations.md`](declarations.md) | Definition blocks, discovery, namespaces, and the reference grammar |
| [`values.md`](values.md) | The five value forms, the tagged-value vocabulary, and the per-surface roots |
| [`expressions.md`](expressions.md) | The Plecture CEL profile |
| [`actions.md`](actions.md) | `exec`, `shell`, and `noop` actions, `bin` versus `command`, and the binding transport |
| [`plugins.md`](plugins.md) | Plugin and catalog manifests |
| [`config.md`](config.md) | The reserved root files |

Tasks come first because work is what the system is for. A task document is a
piece of that work made explicit; effects and workflows are the housing that
gives it somewhere to run. Work is the uncountable phenomenon, divided into
tasks — and a task is what a document declares and an instance carries out.

## Layers of specification

Four layers carry the language, with deliberately different responsibilities.

| Layer | Authoritative for | Source of truth |
|---|---|---|
| Semantic specification | Meaning, invariants, composition rules, security properties | This directory |
| Structural shape | Accepted TOML shapes, field types, required fields, discriminated variants | The implementation's own validation, projected for editors by [`../../plecture.schema.json`](../../plecture.schema.json) |
| Executable specification | Exact behavior at valid, invalid, and boundary cases | [`../../testdata/config-language/`](../../testdata/config-language/) |
| Rationale | Why a language decision was made | [`../adr/`](../adr/) |

The chapters here stay thin. A construct subtle enough to need a long
paragraph also has conformance fixtures that make its cases executable, and
every chapter's worked example is one of those fixtures quoted verbatim.

What a definition may structurally be is answered by the implementation, and
the conformance corpus holds it to naming the exact diagnostic and layer each
case declares. `plecture.schema.json` projects that same shape for editors and
other TOML tooling. It is hand-written, so the corpus also holds the projection
to tracking what it describes: a fixture the language accepts must pass the
schema, and one the language rejects structurally must be rejected by a schema
rule annotated with the same diagnostic. Manifest and config files declare no
definition, so the schema is what judges their shape.

The projection is hand-written until a generator derives it from the same model
the decoder uses, at which point the surface-versus-schema agreement stops
being something a test has to guard.

## Three validation layers

```text
TOML
  ↓
structural validation      the configuration schema
  ↓
Plecture semantic validation   the compiler
  ↓
CEL parsing and type checking   the expression site's environment
  ↓
compiled Plecture representation
```

Structural validation covers known fields, required fields, tagged-value
shapes, action variants, and mutually exclusive combinations. Plecture
semantic validation covers reference resolution, expected kinds, available
`from` roots, graph shape, nesting constraints, capability availability, and
layer and trust rules. CEL validation covers expression syntax and types in
the environment declared for that site.

The implementation may reorder these for efficiency; the responsibility
boundaries do not move.

A reference is always `<subject>.<store>.<key>`. The subject says whose facts
these are — `self`, `inner`, `resource`, `nodes.<id>`, `workflow` — and the
store says which of that subject's contracts they come from: `outputs`, `state`,
`inputs`. So an effect reads its own production records as `self.outputs.<key>`, a
task document its own state as `self.state.<key>`, and the observer it is bound
to as `resource.state.<key>`.

Static resolution — references, kinds, and root existence — is never dropped. Projecting JSON Schema into CEL type information, and checking an
expression's result type against its field, is the first thing dropped if
implementation complexity grows; a dropped check is recorded as an
accepted-invalid conformance fixture rather than forgotten.

## Diagnostics

Diagnostics are part of the tooling interface. A code identifies the language
rule that was broken and the layer that found it; the human-readable message
may improve over time.

Codes carry the `PLECTURE-` prefix because they belong to the language, not to
one program. Any implementation emits them — an editor server, an independent
checker, a generator — so they sit on the Plecture side of the project's naming
split: prose about the language and its specification is Plecture, while
`plect` names the command, its environment, and its self. A code prefixed with
the CLI's name would claim the language's rules for one of its consumers.

| Code | Layer | Rule |
|---|---|---|
| `PLECTURE-CFG-KIND-MISSING` | structural | A definition table declares no `kind`. |
| `PLECTURE-CFG-KIND-UNKNOWN` | structural | `kind` is outside the vocabulary. |
| `PLECTURE-CFG-ID-INVALID` | structural | A definition id does not match `^[A-Za-z_][A-Za-z0-9_]*$`. |
| `PLECTURE-CFG-FIELD-UNKNOWN` | structural | A field is not part of the containing kind's surface. |
| `PLECTURE-CFG-FIELD-REQUIRED` | structural | A required field is absent. |
| `PLECTURE-CFG-FIELD-TYPE` | structural | A field's TOML shape is not the one its surface accepts. |
| `PLECTURE-CFG-SCHEMA-VERSION-OLDER` | semantic | A tree declares a superseded dialect; the error names the migration that carries it forward. |
| `PLECTURE-CFG-SCHEMA-VERSION-NEWER` | semantic | A tree declares a dialect this binary does not know; the error names the binary as too old. |
| `PLECTURE-CFG-VALUE-FROM-AND-EXPR` | structural | One value declares both `from` and `expr`. |
| `PLECTURE-CFG-VALUE-DEFAULT-AND-OPTIONAL` | structural | `default` and `optional` are mutually exclusive. |
| `PLECTURE-CFG-VALUE-TAG-UNKNOWN` | structural | A tagged value uses a key outside the vocabulary. |
| `PLECTURE-CFG-VALUE-TAG-SURFACE` | structural | A capability tag appears on a surface that consumes data only. |
| `PLECTURE-CFG-ACTION-TYPE-UNKNOWN` | structural | An action's `type` is outside the vocabulary its position admits: `exec` or `shell` everywhere, plus `noop` under `[health.alive]` only. |
| `PLECTURE-CFG-ACTION-VARIANT` | structural | An action carries a field belonging to the other variant. |
| `PLECTURE-CFG-ACTION-BIN-AND-COMMAND` | structural | An exec action names its executable through `bin` or `command`, exactly once. |
| `PLECTURE-CFG-SHELL-INTERPOLATION` | structural | Shell source is literal; it carries no Plecture or CEL interpolation. |
| `PLECTURE-CFG-REF-DYNAMIC` | structural | A statically discoverable field carries a computed value. |
| `PLECTURE-CFG-CHANNEL-TIMEOUT-ROOT` | structural | A channel `timeout` reads author-declared parameters only. |
| `PLECTURE-CFG-TASK-INSTRUCTION-ELEMENT` | structural | An `instructions` element declares other than exactly one of `text` and `file`. |
| `PLECTURE-CFG-UNKNOWN-REF` | semantic | A reference resolves to no definition. |
| `PLECTURE-CFG-KIND-MISMATCH` | semantic | A reference site's expected kind differs from the target's declared kind. |
| `PLECTURE-CFG-ID-DUPLICATE` | semantic | One layer declares the same definition id twice. |
| `PLECTURE-CFG-REF-ALIAS-REQUIRED` | semantic | A user-owned reference to catalog content omits its catalog alias. |
| `PLECTURE-CFG-REF-CROSS-PLUGIN` | semantic | A plugin-owned reference names a catalog alias or another plugin's ownership segment. |
| `PLECTURE-CFG-TASK-INSTRUCTION-FILE-MISSING` | semantic | An `instructions` element's `file` names a file that cannot be read. |
| `PLECTURE-CFG-TASK-INSTRUCTION-FILE-CROSS-LAYER` | semantic | An `instructions` element's `file` resolves outside the declaring layer. |
| `PLECTURE-CFG-TASK-INSTRUCTION-CONTROL-FLOW` | semantic | An instruction body carries a `{{ ... }}` construct other than a dotted-path projection. |
| `PLECTURE-CFG-FROM-ROOT` | structural / semantic | A value reads a root the containing surface does not offer. It is structural where a surface's roots are a fixed prefix set, as on an effect's `outputs.bind` and a task document's completion leaves, and semantic otherwise. |
| `PLECTURE-CFG-FROM-PATH` | semantic | A projection names a field the resolved contract does not declare. |
| `PLECTURE-CFG-RESOURCE-OBSERVER-MISMATCH` | instantiation | An instance's resource does not resolve to the observer its task document declares. |
| `PLECTURE-CFG-FIRST-OBSERVE-FAILED` | instantiation | The observation instantiation performs failed, so no instance is created; the observer's own error is reported. |
| `PLECTURE-CFG-BIN-UNKNOWN` | semantic | An executable reference resolves to no declared executable. |
| `PLECTURE-CFG-TERMINAL-UNAVAILABLE` | semantic | A terminal capability is consumed where no effect in the plan declares that verb. |
| `PLECTURE-CFG-HEALTH-ALIVE-REQUIRED` | structural | An effect declares `setup` without declaring `[health.alive]`. |
| `PLECTURE-CFG-NESTING-CYCLE` | semantic | A nesting chain reaches itself. |
| `PLECTURE-CFG-NESTING-OUTPUT-MUTABLE` | semantic | A computed nested output is marked mutable. |
| `PLECTURE-CFG-NESTING-PROJECTION-MISMATCH` | semantic | A direct nested projection disagrees with the inner output's type or mutability. |
| `PLECTURE-CFG-EXTENDS-INHERITED-FIELD` | structural | A task declares `extends` alongside `resource_observer`, which extends inherits rather than composes. |
| `PLECTURE-CFG-EXTENDS-CYCLE` | semantic | An extends chain reaches itself. |
| `PLECTURE-CFG-EXTENDS-JUDGE-ID-DUPLICATE` | semantic | A judge id is declared by more than one declaration in an extends chain. |
| `PLECTURE-CFG-EXTENDS-CHAIN-ID-DUPLICATE` | semantic | A chain id is declared by more than one declaration in an extends chain. |
| `PLECTURE-CFG-EXTENDS-DEFAULT-REDECLARED` | semantic | An extends chain sets a schema key's default more than once. |
| `PLECTURE-CFG-EXTENDS-SCHEMA-TYPE` | semantic | An extends chain redefines a schema key's type or other constraint instead of only adding a default. |
| `PLECTURE-CFG-EXTENDS-SCHEMA-SHAPE` | structural | A document declaring `extends` declares an `inputs_schema`/`state_schema` key other than `type` or `properties`. |
| `PLECTURE-CFG-EXTENDS-SCHEMA-FILE-UNSUPPORTED` | semantic | An extends chain of more than one layer includes a layer using `inputs_schema_file`/`state_schema_file`. |
| `PLECTURE-CFG-WORKFLOW-CYCLE` | semantic | The dependencies derived from node projections form a cycle. |
| `PLECTURE-CFG-POPULATION-CONTRACT` | semantic | A population violates its trusted ownership, query parameter, timing, or shared observer contract. |
| `PLECTURE-CFG-CEL-SYNTAX` | cel | An expression does not parse as CEL. |
| `PLECTURE-CFG-CEL-UNKNOWN-NAME` | cel | An expression names a variable not visible at its site. |
| `PLECTURE-CFG-CEL-TYPE` | cel | An operation, or a result type, does not satisfy the site's expected type. |
| `PLECTURE-CFG-CEL-CUSTOM-FUNCTION` | cel | An expression calls a function the profile does not define. |

## Scope

This language governs configuration definitions in one file form: TOML
documents, whose top-level tables are definition blocks. A task declaration's
instruction is its `instructions` array: each element is either inline `text`
or the content of a sidecar Markdown file its `file` names, joined with a
blank line in declaration order. Either way, it is a Plecture value the
declaration carries, not a second document class.

A Markdown file under a definition root that no instructions element's `file`
names is a template asset rather than a definition: the language does not
read it at all. A referenced sidecar is different — its content is the
declaration's own instruction, projected and validated the same way any
other instruction segment is; see [`tasks.md`](tasks.md).
