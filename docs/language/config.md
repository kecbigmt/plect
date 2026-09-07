# Reserved root files

Three files in the user config home are reserved. They carry machine-wide
settings and resolution state rather than definitions, and a definition table
cannot appear in one.

```text
config.toml
catalogs.toml
plect.lock
```

Because they are reserved, discovery skips them: a definition root's recursive
sweep reads every other `.toml` file as a definition document.

## config.toml

```toml
schema_version = 3

max_up_children     = 12
resource_allowlist  = ["^https://github\\.com/kecbigmt/"]
plugin_dirs         = ["~/.config/plect/plugins"]
channels            = ["notify"]
trusted_project_roots = ["/projects/widgets"]

[inputs_schema]
type                 = "object"
additionalProperties = false

[inputs_schema.properties]
task = { type = "string" }
```

| Field | Meaning |
|---|---|
| `schema_version` | The dialect this config tree is written in. |
| `max_up_children` | Optional positive cap for sessions whose logical parent is the virtual root. |
| `resource_allowlist` | Patterns a resource identifier must match to be accepted. |
| `plugin_dirs` | Additional plugin mount directories, after the catalog-resolved ones. |
| `channels` | Channel definitions delivering for every session. |
| `trusted_project_roots` | Canonical project roots permitted to contribute a project layer. |
| `inputs_schema` | Contract for the session inputs this machine accepts. |

`config.toml` has no workspace-directory setting. The invocation's nearest
ancestor containing `.plect/project.toml` is its project root. When its
canonical path is in `trusted_project_roots`, that root's `.plect/` definition
tree composes after plugins and the machine-owned global layer. No other
ancestor is read, and a generated checkout is never a configuration source.

An interactive invocation encountering an unlisted root displays its canonical
path, explains that project definitions can execute commands, and asks whether
to trust it. Yes writes the canonical root to `trusted_project_roots` and
continues that invocation; no stops it. A non-interactive invocation stops
with instructions to add that canonical root to `trusted_project_roots`.
Chains and populations cannot grant trust, and a trust failure never selects a
global workflow. Trust is a root property, not a digest property; removing a
root revokes use of its project definitions for future desired operations.

A session records its selected project root, participating layer revisions,
and effective digest at creation. Later operations use that root rather than
their caller's cwd, but load the latest valid definitions at the recorded root.
They compare and report a changed digest; a digest mismatch neither fails an
operation nor destroys or rebuilds nodes. The current workflow is the desired
state used for reconciliation. Nodes already set up retain cleanup evidence and
setup-time facts, while executable cleanup code always comes from the current
trusted tree. If a desired revision requires a node to be rebuilt,
the diagnostic directs the caller to `--force-recreate`. If the recorded root
cannot be read, desired operations fail with an actionable error and do not
select another root; cleanup is unavailable unless the current trusted tree can
prove the retained cleanup contract. Chains
inherit their triggering session's root. A resident population inherits the
root captured when the resident started, not its process cwd.

`max_up_children` applies one machine-wide capacity key to every session with
no real parent, including sessions placed in an explicit `root:*` sibling
cohort. It applies to ordinary manual `plect up` as well as resident population
admission. A run-up session and an in-flight admission each count; an
idempotent up of an already-up session does not, while `--force-recreate`
holds a new admission. Real children count only against their real parent's
workflow cap. Unset leaves virtual-root admission unlimited.

A manual up rejected at the cap does not authorize the resident evaluator to
bring another session down. Capacity-driven down is available only during a
population admission and only for eligible population-owned members.

There is no field for whether dispatch detaches. Detachment is a property of the
invoking context — the flag given, whether a terminal is attached, whether an
attach target exists — and none of those is durable configuration.

### The dialect declaration

`schema_version` states which dialect of this language the tree is written in.
It is required: a tree that does not say gets a load error rather than an assumed
default.

Versioning is per package, never per file. This one declaration governs the whole
user tree — every definition document and every task document under it inherits
it — and a plugin declares its own in its manifest. A dialect is a property of a
body of configuration that moves together, not of the files it happens to be
split across.

It is a different axis from `plect_min_version`, which is binary compatibility:
one says what language the configuration speaks, the other what program can run
it.

Loading compares the declared dialect against the one the binary knows, three
ways:

| Declared | Behavior |
|---|---|
| The known dialect | Load. |
| Older | Error naming the migration that carries the tree forward. |
| Newer | Error naming the binary as too old. |

No case guesses. A version increment ships together with its migration
procedure in [`../migrations/`](../migrations/), so the older branch always has
something to name.

## catalogs.toml

`catalogs.toml` registers catalog aliases and the plugins enabled under each.
An alias is user-local: it is what makes a catalog-qualified reference
resolvable on this machine, and it is why a plugin author can never write one.

```toml
schema_version = 3

[[catalogs]]
alias   = "official"
source  = "https://github.com/kecbigmt/plecture"
subdir  = "plugins"
plugins = ["tmux", "claude", "github"]
```

`subdir` bounds the fetch and verify trust space to a subtree of the source
rather than the whole repository.

## plect.lock

`plect.lock` records what resolution produced: each catalog's resolved
revision, and each enabled plugin's path, content hash, version, and minimum
`plect` version. Mounting verifies against it and never fetches, so a plugin
problem fails the whole config load rather than silently mounting nothing.

An `editable` plugin entry is exempt from content verification, which is what
makes local plugin development possible without rewriting the lock on every
edit.

## Validation rules

- A definition table in a reserved root file is a load error.
- An unknown field is a load error rather than being ignored.
- `schema_version` is required in `config.toml`, `catalogs.toml`, and
  `plect.lock`.
- A declared dialect that is not the one the binary knows is a load error, in
  the direction the comparison found.
- A catalog alias matches `^[A-Za-z0-9][A-Za-z0-9_-]*$`.
- Every `channels` entry resolves to a definition of kind `channel`.
- Each `trusted_project_roots` entry is an absolute canonical path.
- `max_up_children`, when declared, is at least one.
- A missing `catalogs.toml` means no catalogs are registered, which is not an
  error.
