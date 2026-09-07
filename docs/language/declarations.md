# Declarations, identity, and references

The language's unit of meaning is the definition block. Files are packaging.

Splitting definition blocks across any number of `.toml` files, and
concatenating those files into one TOML document in path-sorted traversal
order, are semantically equivalent — except for file-relative path fields,
which stay relative to the file containing the definition table.

## Definition blocks

A definition is a top-level table whose name is the definition id, carrying a
required `kind`:

```text
[<id>]
kind = "<kind>"
```

| Field | Meaning |
|---|---|
| `<id>` | The responsibility name references use. |
| `kind` | The declaration contract this block implements. |

The kind vocabulary is `effect`, `task`, `channel`, `workflow`, and
`resource`.

Every kind is declared the same way, `task` included: a `[<id>]` table
carrying `kind`, in a TOML definition document. A task's instruction — the one
value with a natural home outside TOML — is its `instructions` array: each
element is either inline `text` or the content of a sidecar Markdown file
named by `file`, resolved relative to the declaring file. Nothing else
changes for it: the id is the table name, the id grammar holds, the namespace
is shared, and references resolve the same way. See [`tasks.md`](tasks.md).

A resource is the external thing a session and task address. Effects, channels,
and workflows have runtime instances; a task is the statement of work about a
resource. Every block in this language is a definition.

An id is a TOML bare-key segment matching `^[A-Za-z_][A-Za-z0-9_]*$`. Quoted
keys are not ids. Dots are excluded because dots separate address segments.
Hyphens are excluded because an effect id is also a workflow node id when a node
omits `id`, and a node id must be a safe dotted path segment.

An id names a responsibility, not a product name repeated for qualification: a
Claude Code plugin's launch effect is `[runtime]`, so its catalog-qualified
address reads `official.claude.runtime`.

Nested and array tables stay under the definition table:

```toml
[github_issue]
kind  = "resource"
match = '^https://github\.com/(?P<id>.+)$'
name  = { from = "match.id" }

[github_issue.observe]
type = "exec"
bin  = "github-issue-pr"
args = ["observe", "--resource", { from = "resource.id" }]

[runtime]
kind  = "effect"
scope = "run"

[runtime.setup]
type = "exec"
bin  = "agent-runtime"
args = ["launch"]

[runtime.health.alive]
type = "noop"

[review_session]
kind     = "workflow"
resource = "github_issue"

[[review_session.nodes]]
uses = "runtime"
```

Everything nested below a definition table belongs to that definition. A
definition does not exist without a top-level `[<id>]` table and its `kind`.

## Discovery

Each trusted config layer has a definition root: a plugin's `config/`
directory, the user config home excluding reserved root files, or a trusted
project's `.plect/` directory excluding its project marker.

Within a definition root, every `.toml` file that is not a reserved root file
is read recursively in lexicographic order by slash-separated relative path. A
`.md` file is never itself a definition: one an instructions element's `file`
names is that declaration's instruction sidecar, and any other is a template
asset the language does not read.
Subdirectories are author organization only: one definition per file and
kind-named directories such as `config/effects/` are equally valid and mean the
same thing.

The user config home's reserved root files are `config.toml`, `catalogs.toml`,
and `plect.lock`. They are not definition files. A project definition root
reserves `.plect/project.toml`; it contains exactly the required marker:

```toml
schema_version = 3
```

No fields other than `schema_version` are valid there, and it never contributes
a definition. Its version declares the dialect of that selected project
definition layer only. The global configuration dialect remains the
`schema_version` in `config.toml`; neither declaration governs the other.
A missing marker version or a version the binary does not know is a load error
under the same dialect comparison rules as `config.toml`.

Cross-file array-of-table entries append in traversal order while preserving
in-file order. Cross-file duplicate definition ids in one layer are load
errors, as are table collisions below a definition table that TOML would
reject in one concatenated document.

Invocation selects an optional project definition layer before workflow setup:
the nearest ancestor with `.plect/project.toml` is its project root. Only that
root's `.plect/` tree participates; no higher ancestor and no generated
checkout participates. The project root must be listed as trusted in the
machine configuration, or be explicitly trusted by an interactive invocation;
chains and populations cannot grant that trust. See [`config.md`](config.md).

## Namespaces

Each plugin has one definition id namespace across all kinds. Each user-owned
layer likewise has one. Two definitions with the same id in one layer are a
load error even when their kinds differ.

Different plugins may reuse an id; their full addresses differ by catalog
alias and plugin path.

| Deeper definition | Rule |
|---|---|
| Same id, same kind | A deeper user declaration replaces the shallower declaration as a whole. |
| Same id, different kind | Coexist. Each layer has its own namespace, so the deeper declaration shadows nothing, and a reference resolves by the kind its site expects. |

The whole-definition kinds are resource, channel, effect, task, and workflow.

Catalog-qualified references select catalog plugin definitions and are not
shadowed by a same-id relative definition in a user-owned layer. A same-id
user-owned definition does not extend a plugin-owned workflow either: a
user-owned workflow or effect selects a user-owned replacement by referencing
its relative address.

## References

References are dotted addresses:

```text
<id>
<catalog-alias>.<plugin-path>.<id>
```

Catalog aliases and plugin path segments match
`^[A-Za-z0-9][A-Za-z0-9_-]*$`. The first character is alphanumeric so a
segment cannot read as a CLI flag; hyphens are valid after it because aliases
and plugin paths never become node ids.

The relative form refers to a definition in the same plugin, or in the
user-owned layer stack. In user-owned config, a relative reference that would
select catalog content is a load error: the reference must carry its alias, so
that its validity depends on the reference and the alias it names rather than
on which other catalogs happen to be enabled. Stored config therefore has no
alias-optional middle form.

The catalog-qualified form's final segment is the definition id; the segments
between the alias and that id are the plugin path, which must name an enabled
plugin under that alias.

A reference written inside a plugin uses only the relative form: catalog
aliases are user-local and unknowable to a plugin author, and cross-plugin
references from shipped plugin config remain banned by the plugin boundary
rule.

```toml
[my_review]
kind     = "workflow"
resource = "official.github.issue"

[[my_review.nodes]]
uses = "official.tmux.pane"

[[my_review.nodes]]
id   = "agent"
uses = "official.claude.runtime"

[[my_review.event.channel]]
name    = "runtime"
uses    = "official.claude.delivery"
include = ["plect.instruction", "user.emit"]

[my_review.event.channel.inputs]
path = { from = "nodes.agent.outputs.socket_path" }
```

Exemplar workflow designs may use the scaffold-only form `<plugin>.<id>` for a
single-segment plugin path. Scaffolding rewrites it to the user's alias during
copy-time verification, before the workflow is stored as config. An exemplar
for a multi-segment plugin path identifies its source plugin through scaffold
metadata instead of adding dotted segments.

### Expected kinds

A reference carries no kind segment. After resolution, the target's declared
`kind` must match the site's expected kind.

| Reference site | Expected kind |
|---|---|
| Workflow `resource` | `resource` |
| Workflow population `resource` | `resource` |
| Workflow population `session.task` | `task` |
| Workflow node `uses` | `effect` |
| Workflow event channel `uses` | `channel` |
| Effect `inner.uses` | `effect` |
| Task field `resource` | `resource` |
| Task field `extends` | `task` |
| Task chain `workflow` | `workflow` |
| Dynamic-instantiation target | `task` |
| Chain spawn target | `task` |
| `config.toml` `channels` | `channel` |

A mismatch names the site, the reference, the expected kind, and the target's
declared kind.

When a workflow node omits `id`, the node id defaults to the referenced
definition's id — not to the full dotted reference. For
`uses = "official.claude.runtime"` the default node id is `runtime`. Two nodes
that would default to the same id need an explicit `id`.

Nesting, extends, and chain-spawn references use this same grammar. They get
no second reference language.

Executable lookup is a separate namespace, not a definition reference: it
keeps the slash grammar described in
[`../design/plugin-packaging.md`](../design/plugin-packaging.md), because
selecting an executable must split an arbitrary-depth plugin path from an
executable name. See [`actions.md`](actions.md).

## Validation rules

- A discovered TOML file in a trusted definition root with no top-level
  definition table is a load error unless it is a reserved root file.
- A definition table missing `kind`, or carrying an unknown `kind`, is a load
  error.
- A definition id must match `^[A-Za-z_][A-Za-z0-9_]*$`.
- Two definitions with the same id in one layer are a load error, whatever
  their kinds.
- A stored reference uses either the relative or the catalog-qualified form.
- A user-owned reference to catalog content includes the catalog alias.
- A plugin-owned reference includes no catalog alias and no other plugin's
  ownership segment.
- A reference site's expected kind matches the resolved target's `kind`.
- A `.plect/` directory is never read as configuration.
