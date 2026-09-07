# Tasks

Plecture gives autonomous work a place to go. A task document is the work — one
piece of it, made explicit enough to hand over.

A task declaration is an ordinary `[<id>]` table carrying `kind = "task"`, in
any `tasks/*.toml` file. `done_when` says when it is done, `resource`
what it is about, `[[chains]]` what follows from it — and `instructions` says
what is to be done. One declaration carries all of it, because an instruction
and the conditions for calling it finished are one statement about one task.

The place that work goes is a session — assembled from effects by a workflow,
and described in the chapters after this one. Work is divided into tasks; a
task document declares one, and an instance carries it out.

```toml
[work]
kind              = "task"
description       = "Implement a fix or feature for an issue and create a PR"
resource          = "issue_pr"
instructions      = [{ file = "document.md" }]

[work.inputs_schema]
type = "object"

[work.inputs_schema.properties]
instruction = { type = "string" }

[work.done_when]
all = [
  { check = "resource.state.resource_kind", in = ["pull", "issue"] },
  { check = "resource.state.checks_status", in = ["SUCCESS", "NULL"] },
  { judge = "acceptance criteria are satisfied, with a concrete reason", id = "ac-met" },
  { judge = "the resource change actually resolves the requested work without unaddressed risks", id = "solves" },
]

[work.budget]
heartbeat_budget = 3
on_exhaust       = "escalate"
```

<!-- fixture: tasks/document.md -->
```markdown
Resolve the issue at {{ resource.id }}.

Steps:

1. Understand the issue
2. Investigate the relevant code
3. Implement the changes
4. Write and run tests
5. Commit and push, then open a pull request
```

## Serialization

Every kind is declared the same way: a `[<id>]` table in a TOML definition
document, `task` included. One serialization means one grammar, one
structural schema, one validator, and one fixture set. Discovery does not
special-case task by file class — see [`declarations.md`](declarations.md).

The instruction is a value the declaration carries, not a TOML string field,
for four reasons. Triple-quote escaping breaks structurally on an instruction
that quotes TOML examples containing multi-line strings, which shipped
instructions do — a `file` segment sidesteps that entirely, and a `text`
segment remains available for a body short enough to embed. Authoring,
reviewing, and exchanging a long instruction reads and diffs better as its own
Markdown file than as a quoted string, and a Markdown sidecar renders on
GitHub the way TOML never will. And the goal file a task converges with is
already a document.

## The instruction

`instructions` is an ordered array. Each element is one segment of the
instruction, carrying exactly one of `text` (inline) or `file` (a sidecar
Markdown file, resolved relative to the declaring TOML file); declaring both
or neither on one element is a load error
(`PLECTURE-CFG-TASK-INSTRUCTION-ELEMENT`). The array's elements render as
plain prose, joined with a blank line in declaration order — one segment is
the common case, and the array shape is what lets a later document extend a
base instruction by appending a segment rather than concatenating strings.

A `file` segment's sidecar carries no frontmatter of its own, so it renders
correctly wherever Markdown does. It must resolve within the same trusted
layer: a path escaping it is a load error
(`PLECTURE-CFG-TASK-INSTRUCTION-FILE-CROSS-LAYER`), and a path naming no file
is too (`PLECTURE-CFG-TASK-INSTRUCTION-FILE-MISSING`). Declaring no
`instructions` at all leaves the instruction empty.

The Markdown sidecar carries no declaration of its own — it is a template
asset in the sense [`declarations.md`](declarations.md) describes, except that
this one is named by an element's `file` rather than sitting unreferenced.

Its interpolation is part of the language, not asset templating. `{{ <path> }}`
in prose is exactly the `from` projection in prose position: the same root
vocabulary, resolved and validated at load against the environment this surface
declares. The `{{ }}` delimiters are inherited from the Go templates the
instruction assets used, and are the mustache-family spelling a reader already
knows.

The roots are the ones the instruction is *about* — `inputs.<key>`,
`resource.id`, `resource.state.<key>`, `self.state.<key>`, `session.*` — listed
in [`values.md`](values.md) alongside the roots `done_when` reads. A projection
preserves its native type everywhere else in the language; in prose position it
is stringified, because prose has nowhere to put a list.

There is no control flow in the instruction. CEL is expression-only, and no
conditional or defaulting construct is introduced for prose position: a
`{{ ... }}` that is not a bare `dotted.path` is
`PLECTURE-CFG-TASK-INSTRUCTION-CONTROL-FLOW`, not a form the instruction
carries through unexecuted. An author who needs a value to render even when
the input behind it is absent gives that input's schema property a `default`,
so the projection always resolves; an author who needs different prose for
different cases writes separate instruction segments (or separate task
documents) and lets `extends`/chain wiring choose between them, rather than
branching inside one body.

## Declaration

A task declaration is a `[<id>]` table carrying `kind = "task"`, with every
field under that table — exactly how every other kind is declared. Task gets
no identity spelling of its own.

| Field | Meaning |
|---|---|
| `kind` | `task`. |
| `description` | What this task is for. |
| `resource` | The resource this task is written for. |
| `[[<id>.instructions]]` | The instruction, as an ordered array of `text` / `file` segments. |
| `[<id>.inputs_schema]` | The instruction's author-declared parameters. |
| `[<id>.state_schema]` | This task's own state: the keys something else writes. |
| `[<id>.done_when]` | The completion predicate. |
| `[<id>.budget]` | The convergence bound, if any. |
| `[[<id>.chains]]` | What this task spawns, and when. See [`chains.md`](chains.md). |

Identity follows [`declarations.md`](declarations.md) unchanged: the id is the
table name, filename and directory stay non-semantic, task ids share the one
per-layer namespace every kind shares, and references use the same dotted forms
validated against `kind = "task"`.

Instance identity is separate and orthogonal: the id names the declaration,
while an instance is identified by its concrete resource and instance name. A
task can be added to a session whose entry resource has another type; this does
not change the session's identity or environment.

## State

`state_schema` declares this task's own state: the keys a reviewer or another
session writes into an instance. It is plain JSON Schema, and it carries no
mutability annotation — state is mutable by definition.

One rule covers the whole language: any definition that holds state declares it
with `state_schema`. A resource declares the state it publishes about a
resource; a task document declares the state it holds about itself.

Those two schemas are the two roots a completion predicate reads:
`resource.state.*` for what the resource publishes, and `self.state.*` for what this
task holds. Both are live — every read is current as of that evaluation.

There is no intermediate declaration between a schema and the predicate that
reads it. A key does not have to be re-listed to be readable: the resource's
`state_schema` already says what exists, and this document's `state_schema` says
what it keeps.

`verdict_revision` in the examples here is a convention, not a reserved key.
Core special-cases nothing about it: it is an ordinary declared state key whose
meaning lives entirely in the configuration that reads it.

```toml
[review]
kind        = "task"
description = "Review a pull request and record a verdict"
resource    = "issue_pr"
instructions      = [{ text = "Review the pull request at {{ resource.id }} and record your verdict." }]

[review.inputs_schema]
type = "object"

[review.inputs_schema.properties]
instruction = { type = "string" }

# verdict_revision is this task's own state: written into the instance by the
# reviewer rather than published by the resource. It carries no mutability
# annotation, because state is mutable by definition.
[review.state_schema]
type = "object"

[review.state_schema.properties]
verdict_revision = { type = "string" }

[review.done_when]
all = [
  { check = "resource.state.resource_kind", in = ["pull", "issue"] },
  { expr = "self.state.verdict_revision == resource.state.revision" },
]

[review.budget]
heartbeat_budget = 3
on_exhaust       = "escalate"
```

## Completion

`done_when` is a conjunction of leaves. A check leaf compares one key, named by
its root path. An expression leaf states a predicate computed over those roots —
which is how a recorded verdict is compared against a live revision, with no key
of its own to hang on. A judge leaf waits for independent reviewer input
recorded against the instance, optionally restricted to reviewers in a declared
relation.

A leaf reads `resource.state.*` or `self.state.*`, and nothing else. Both are
resolved at load against the schemas that declare them, so a misspelled key
fails before anything runs.

`budget` bounds convergence. Omitting it leaves completion unbounded, which is
what a standing goal needs: continuing to exist is not exhaustion.

## What a task document is not

A task document owns no lifecycle. It has no `setup`, no `cleanup`, no health
probe, no interactive endpoint, and no nesting joint. It brings nothing up and
takes nothing down — those are an effect's concerns, and a task document is
dispatched into a session a workflow has already built.

```toml
[broken_task]
kind        = "task"
description = "A task document that tries to own a lifecycle"
resource    = "issue_pr"
instructions      = [{ text = "Resolve the issue at {{ resource.id }}." }]

[broken_task.setup]
type = "exec"
bin  = "github-issue-pr"
args = ["render-instruction"]

[broken_task.done_when]
all = [{ check = "resource.state.checks_status", in = ["SUCCESS"] }]
```

## Documents and instances

A task document is authored. Its identity is the id its declaration carries,
in the one per-layer namespace every kind shares, and references resolve to it
by that id.

A task instance is created from a document, by dynamic instantiation or by
another document's chain spawn. Its identity is its resource plus its instance
name. The two identities are orthogonal: one document backs many instances, and
an instance's resource says nothing about which id declared it.

The distinction runs through the rest of this specification. A document is
authored, declared, and referenced; an instance is created, evaluated, and
finalized. `done_when` is declared once on the document and
evaluated per instance.

## Resource binding

A task document declares the resource type it is written for:

```toml
resource = "issue_pr"
```

The field is named after the kind it references and is an ordinary dotted
reference whose target must declare `kind = "resource"`.

An instance is still a document paired with a resource. The document is
*type-declared and instance-late-bound*: which resource, it learns at
instantiation; what kind of resource, it states up front.

Declaring it closes the chain at load time:

```text
resource       state_schema   the keys it publishes
    ↓
task document  done_when      reads them as resource.state.*
```

Because the resource is known from the declaration, a key it does not publish is
a load error rather than a surprise at run time. And because the declaration
states a type, instantiation checks compatibility up front: binding an instance
to a resource that does not resolve to the declared resource fails immediately,
rather than producing an instance that can never satisfy.

The initiating caller selects an optional initial task: `plect up`, a chain,
or a population. A workflow does not select it. Each surface selects at most
one task and supplies one compatible concrete resource binding.

Every shipped task document is written for exactly one resource type, so this
dependency already existed — it was hiding in a runtime convention. Declaring it
is the move this language makes everywhere else.

One resource that reports more than one kind of thing is a sign it wants
splitting rather than the declaration wanting widening. Where a document's
completion depends on which kind it got — a pull request's checks against an
issue's timestamp, say — two resources state that better than one resource plus a
kind check.

A `done_when` check on the resource's own kind key remains useful for narrowing
*within* one resource, where it publishes more than one subtype:
`resource.state.resource_kind in ["pull", "issue"]` distinguishes two shapes
the same resource reports.

A workflow's `[[nodes]]` never reference a task document. A node names an effect,
because a node is a position in a lifecycle graph; task arrives afterward,
against the session that graph produced.

## Extension

`extends` specializes a task document without copying it: `[<id>] kind = "task"`
plus `extends = "<task reference>"`, the ordinary dotted reference grammar,
naming the base. An extension is a real declaration with its own id —
referencing sites (workflows, dispatch, another chain) name the extension, and
the base stays untouched and independently referable.

An extension does not declare `resource`; it inherits the base's, so
the resource a completion leaf resolves against has one source, never two
declarations that could disagree.
`description` is the one exception to composition: it is per-declaration
display metadata, entirely outside it. An extension states its own
(omitted means empty), and the base's is untouchable by construction.

Composition is a closed, entirely additive whitelist:

- `[[instructions]]` — the extension's elements append after the base's
  (concatenated array, blank-line join at render — the array shape
  [`instructions`](#the-instruction) already carries).
- `[[chains]]` — the extension's own chains add to the base's. A chain id is
  unique across the whole extends chain, the same rule a judge id follows; a
  collision is a load error (`PLECTURE-CFG-EXTENDS-CHAIN-ID-DUPLICATE`).
- `done_when.all` — leaf append only, monotone strengthening. Judge ids are
  unique across the whole extends chain; a collision is a load error
  (`PLECTURE-CFG-EXTENDS-JUDGE-ID-DUPLICATE`).
- `inputs_schema` / `state_schema` — new keys are freely addable. An existing
  key may only gain a default where none is set anywhere in the inner chain;
  redeclaring a default the inner chain already fixed is a load error
  (`PLECTURE-CFG-EXTENDS-DEFAULT-REDECLARED`). Redefining a key's type or any
  other constraint is always a load error (`PLECTURE-CFG-EXTENDS-SCHEMA-TYPE`).
  From outside, an extended task is simply a task with those defaults — its
  interior is hidden, the same inner-first encapsulation
  [`effects.md`](effects.md) states for nesting. An extension's own
  `inputs_schema` / `state_schema` table declares only `type` and `properties`
  — `required`, `additionalProperties`, and every other schema-object-level
  keyword answer for the composed contract's overall shape, which only the
  root gets to set; declaring one on an extension is a load error
  (`PLECTURE-CFG-EXTENDS-SCHEMA-SHAPE`), not something composed as a union or
  otherwise, because there is no additive reading of "this layer may also
  narrow what counts as valid." A restated `type` must agree with whichever
  earlier layer already fixed it, the same redefinition error as a property's
  (`PLECTURE-CFG-EXTENDS-SCHEMA-TYPE`); composition keeps only the first
  value, so a disagreeing later layer's declaration would otherwise be dead
  weight rather than the error it should be.
  `inputs_schema_file` / `state_schema_file` are
  not supported inside a real extends chain (more than one layer): the path
  resolves relative to its declaring layer's own directory, which a single
  composed document has no per-field way to remember, so a layer using either
  form anywhere in the chain is a load error
  (`PLECTURE-CFG-EXTENDS-SCHEMA-FILE-UNSUPPORTED`) rather than a contract
  silently dropped. Both forms stay fully supported outside an extends chain.

There is no way to remove, replace, or weaken a base leaf, element, or key. A
task essentially different from its base is a full declaration of its own — a
fork — deliberately: weakening a gate has to be visible as a whole
declaration, never a small diff against something it silently no longer
agrees with.

Depth is unbounded, aligned with effect nesting: an extends chain that reaches
itself is a load error (`PLECTURE-CFG-EXTENDS-CYCLE`). A plugin's base
`instructions` are written append-aware — neutral prose a variant can extend
with "when dispatched as X, additionally …" — rather than the if/else
interleaving a Go template made possible.

`plect task show` on an extension prints the composed extends chain, outermost
(the extension) first, naming for every layer each instruction element,
chain, `done_when` leaf (of every kind — check, expr, and judge — not only
judges), and schema key that layer itself contributes to the composed
declaration, including which layer's own declaration set a key's default.

Two extensions of one base each choosing a different reviewer with a static
chain — the shape that dissolves a templated `{{if eq .Work.workflow
"claude"}}codex{{else}}claude{{end}}` conditional into two plain declarations:

```toml
[work]
kind        = "task"
description = "Implement a fix and hand it to review"
resource    = "issue_pr"
instructions      = [{ text = "Resolve the issue at {{ resource.id }}." }]

[work.done_when]
all = [
  { check = "resource.state.checks_status", in = ["SUCCESS"] },
  { judge = "acceptance criteria are satisfied", id = "ac-met" },
]

[work_claude]
kind    = "task"
extends = "work"

[[work_claude.chains]]
id        = "review"
workflow  = "claude_reviewer"
placement = "sibling"

[work_claude.chains.when]
all = [{ judge_pending = "ac-met" }]

[work_codex]
kind    = "task"
extends = "work"

[[work_codex.chains]]
id        = "review"
workflow  = "codex_reviewer"
placement = "sibling"

[work_codex.chains.when]
all = [{ judge_pending = "ac-met" }]

[claude_reviewer]
kind = "workflow"

[codex_reviewer]
kind = "workflow"
```

A gate variant appends one instruction segment and the judge that records it —
additive on both surfaces the same declaration touches:

```toml
[review]
kind        = "task"
description = "Review a pull request and record a verdict"
resource    = "issue_pr"
instructions      = [{ text = "Review the pull request at {{ resource.id }}." }]

[review.done_when]
all = [
  { check = "resource.state.checks_status", in = ["SUCCESS"] },
  { judge = "the change is correct", id = "correct" },
]

[review_security]
kind         = "task"
extends      = "review"
instructions = [{ text = "Additionally record whether the change introduces a security risk." }]

[review_security.done_when]
all = [{ judge = "the change introduces no security risk", id = "no-security-risk" }]
```

Three layers deep — an official base, a team extension, and a member's
personal extension of the team's — is the team-adoption shape unbounded depth
and the inner-first default rule exist for:

```toml
[official_review]
kind        = "task"
description = "Review a change against the official contract"
resource    = "issue_pr"
instructions      = [{ text = "Review the change at {{ resource.id }}." }]

[official_review.done_when]
all = [{ judge = "the change is correct", id = "correct" }]

[team_review]
kind         = "task"
extends      = "official_review"
instructions = [{ text = "Additionally check the team's own style checklist." }]

[team_review.done_when]
all = [{ judge = "the team style checklist is satisfied", id = "team-style" }]

[my_review]
kind         = "task"
extends      = "team_review"
instructions = [{ text = "Additionally leave inline comments for anything worth changing." }]
```

## Validation rules

- A task declaration carries a `resource`.
- `resource` resolves to a definition of that kind.
- Each `instructions` element carries exactly one of `text` and `file`.
- An element's `file` resolves, relative to the declaring file, to a readable
  file within the declaring layer.
- A completion key reads `resource.state.*` or `self.state.*`.
- A `resource.state.*` key names a property the declared resource's
  `state_schema` declares, and a `self.state.*` key one this document's declares —
  both checked at load.
- An instance's resource resolves to the declared resource.
- Instantiation observes the resource once; a failed first observation rejects
  instantiation, and no instance is created.
- A lifecycle field is not part of the task grammar.
- A workflow node referencing a task document is a kind mismatch.
- `extends` resolves to a definition of kind `task`.
- A document declaring `extends` does not also declare `resource`.
- An extends chain that reaches itself is a load error.
- A judge id is declared by at most one definition in an extends chain.
- A chain id is declared by at most one definition in an extends chain.
- An existing `inputs_schema` / `state_schema` key may gain a default only
  where the inner chain sets none; redeclaring one already set, or redefining
  the key's type or any other constraint, is a load error.
- A document declaring `extends` declares only `type` and `properties` on its
  own `inputs_schema` / `state_schema`.
- An extends chain of more than one layer includes no layer declaring
  `inputs_schema_file` / `state_schema_file`.
