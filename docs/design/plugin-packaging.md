# Plugin packaging, reference resolution, lockfile, and trust model

This document defines the plugin model for distributing reusable Plecture
configuration and executable adapters. The goal is to reduce bootstrap cost
without moving workspace-provider, tool, or domain knowledge into core.
The plugin service lifecycle decision is recorded in
[`../adr/2026-08-17-plugin-boundary-contracts.md`](../adr/2026-08-17-plugin-boundary-contracts.md).

The design keeps two concepts: catalog and plugin. A catalog is a trusted
distribution unit: a subtree of a source repository marked by `catalog.toml`.
A plugin is a distributable package inside a catalog. It may contain executable
adapters, config resources, and metadata. A plugin with only
configuration is still a plugin.

## Goals and constraints

- Core owns generic mechanics: fetch, verify, mount, resolve, and fail-loud
  compatibility checks.
- Plugins own technology or domain commitments: workspace provider adapters,
  resource observation, session runtime surfaces, and workflow packs.
- User-owned config remains the final authority.
- No workspace-provider/resource/task/workflow contract changes are required by this
  design. Any discovered gap must become a later contract issue, not an
  assumption in the package format.
- No auto-update. Every add and update is pinned and explicit.

## Package format

A catalog is a directory with a `catalog.toml` manifest at its root. The
manifest's directory bounds the trust space: Plecture does not fetch, verify,
or mount files outside that subtree for plugins in the catalog.

`catalog.toml` is hand-authored catalog metadata, not a generated index:

```toml
schema_version = 2
description = "Example Plecture plugin catalog."

plugins = [
  "github",
  "session/runtime",
]
```

Catalog fields:

| Field | Required | Meaning |
|---|---:|---|
| `schema_version` | yes | Catalog file-format version. Unknown values fail loud. |
| `plugins` | yes | Explicit catalog-relative paths to directories that directly contain `plugin.toml`. |
| `description` | no | Display-only summary for list/show commands. |

Catalogs have no upstream identity field. A catalog name exists only as the
user's local alias in the catalog registration.

Every path listed in `plugins` must resolve inside the catalog trust space
after symlink resolution. `..` escapes and symlink escapes are load errors. A
listed path without `plugin.toml` is a validation error. A `plugin.toml` under
the catalog subtree that is not listed is also a validation error. This strict
rule is intentional: reviewers can audit the exact published plugin set from
one manifest, and catalogs that need fixture manifests can avoid the reserved
filename.

A plugin is a project root with a small fixed set of roles. Config declarations
live under `config/`; build source lives under `src/`; committed executables
live under `scripts/`; build outputs live under `bin/`.

```text
plugin.toml
README.md
config/
src/
scripts/
bin/
```

With `src/` introduced, the plugin root is not a config-home overlay fragment.
Layers are cut by role. The mirror correspondence with the user config home is
preserved by stripping the `config/` prefix: a plugin file
`config/<relative-path>` shadows or replaces the config-home file
`<relative-path>`. The loader mounts plugin `config/` as the layer root.

`src/` holds source projects that `build` commands compile from. The plugin is
the distribution and enablement unit; a Go module is a build unit. Those units
are explicitly decoupled so distribution cohesion does not dictate source-level
coupling.

A plugin with built Go executables uses either one Go module directly at
`src/` (`go.mod`, `go.sum`, `cmd/`, `internal/`) or multiple independent Go
modules under `src/<project-name>/`, each with its own `go.mod`. The
session runtime plugins can therefore keep channel-server, agent helpers, and
other implementation projects as separate modules inside the plugin that owns
them.
The repository `go.work` lists each module individually, following the existing
multi-module monorepo practice.

The directory name stays `src/`. `modules/` is rejected because it collides with
Nix and Go vocabulary, `internal/` carries Go import-path semantics, `go/`
breaks language neutrality, and `apps/` is misleading for daemon and helper
executables.

`bin/` holds only build output — content a `build` command produces, never
committed to the catalog source itself. `scripts/` holds committed script
executables that need no build step at all; an entry with no `build` points
`path` straight at a file under `scripts/` (or elsewhere in the plugin tree).
A plugin with only build-less scripts needs no `src/`; a plugin with only
shipped config needs neither.

`plugin.toml` is the only required plugin-local file:

```toml
schema_version = 2
version = "0.3.0"
plect_min_version = "0.8.0"
description = "GitHub workspace provider, resource observation, and workflow support."

[[executables]]
name = "github-watcher"
path = "bin/github-watcher"
build = "go -C src build -o ../bin/github-watcher ./cmd/github-watcher"

[[executables]]
name = "github-worktree"
path = "bin/github-worktree"
build = "go -C src build -o ../bin/github-worktree ./cmd/github-worktree"

[[executables]]
name = "github-issue-pr"
path = "bin/github-issue-pr"
build = "go -C src build -o ../bin/github-issue-pr ./cmd/github-issue-pr"
```

A plugin with multiple source modules points each build command at the module
that owns that executable:

```toml
[[executables]]
name = "channel-server"
path = "bin/channel-server"
build = "go -C src/channel-server build -o ../../bin/channel-server ./cmd/channel-server"
```

Plugin metadata fields:

| Field | Required | Meaning |
|---|---:|---|
| `schema_version` | yes | Plugin metadata file-format version. Unknown values fail loud. |
| `version` | no | Informational plugin package version for diagnostics and list/show commands. |
| `plect_min_version` | yes | Minimum plect version required to load the plugin. |
| `description` | no | Human-readable summary for list/show commands. |
| `executables` | no | Relative paths for scripts or binaries shipped by the plugin. |
| `services` | no | Daemon declarations supervised by `plect serve`. |

The catalog-relative path listed in `catalog.toml` is the plugin identity. Full
identity is `<catalog-alias>/<relative-path>`, such as `official/github` or
`official/session/runtime`. Intra-catalog uniqueness comes from the
filesystem. There is no plugin-owned identity field and no metadata identity
check.

Executable entry fields:

| Field | Required | Meaning |
|---|---:|---|
| `name` | yes | Stable executable identity inside the plugin. |
| `path` | yes | Relative executable path. In the primary script case, this points at a file already present in the source tree. |
| `build` | no | Command run at add/update time to produce a compiled executable from plugin source. |

Service entries declare plugin-owned daemons supervised by `plect serve`:

```toml
[[services]]
name = "github-watcher"
executable = "github-watcher"
args = ["serve"]
restart = "on-failure"
health = { type = "process" }
```

A session runtime plugin can declare its daemons the same way:

```toml
[[services]]
name = "channel-server"
executable = "channel-server"
restart = "on-failure"
health = { type = "process" }

[[services]]
name = "slack-adapter"
executable = "slack-adapter"
args = ["serve"]
required_env = ["SLACK_BOT_TOKEN"]
restart = "on-failure"
health = { type = "process" }
```

Service entry fields:

| Field | Required | Meaning |
|---|---:|---|
| `name` | yes | Stable service identity inside the plugin. Full identity is `<catalog-alias>/<plugin-path>/<service-name>`. |
| `executable` | yes | Name of one executable declared by the same plugin. Services cannot name another plugin's executable. |
| `args` | no | Argument vector passed after the executable path. |
| `env` | no | Non-secret environment literals supplied to the child process. |
| `required_env` | no | Environment variable names that must be present in user-owned configuration or the supervisor environment before the service can start. |
| `restart` | no | Restart policy. `on-failure` restarts crashed children with bounded backoff; `never` leaves the service stopped after exit. |
| `health` | no | Health policy. `type = "process"` treats a running child as healthy. Future health kinds must be workspace-provider-agnostic. |

The plugin service supervisor starts services for enabled plugins when
`plect serve` starts, stops them when the resident process stops, and restarts
service processes according to their restart policy. Service status is
resident-global and records service id,
running state, pid, restart count, last exit, last error, last health result,
plugin id, and the lock coordinate or content hash that produced the running
process.

Service logs are attached to the resident process log with the service id.
Service logs do not write into per-session event logs.

Updating a service-owning plugin restarts its services.

Build-less script executables are the primary v1 form. They must carry their
interpreter through a shebang. Built binaries are the additional case for
plugins that need compilation.

The standard `config/` subdirectories with plugin-layer loader behavior are:

| Directory | Mount behavior |
|---|---|
| `config/workspaces/` | Mounted as `workspaces/`. Trusted base layer only. Same-id conflicts between plugin layers are load errors; global user definitions replace plugin definitions. |
| `config/resources/` | Mounted as `resources/`. Trusted base layer only. Same-id conflicts between plugin layers are load errors; global user definitions replace plugin definitions. |
| `config/channels/` | Mounted as `channels/`. Trusted base layer only. Same-id conflicts between plugin layers are load errors; global user definitions replace plugin definitions. |
| `config/tasks/` | Mounted as `tasks/`. Trusted layer plus trusted ancestor overlay. Same-id conflicts between plugin layers are load errors; user-owned layers replace whole definitions. |
| `config/workflows/` | Mounted as `workflows/`. Trusted layer plus ancestor overlay. Same-id plugin-layer conflicts are load errors; a user-owned layer adds nodes, and every other field a shallower layer set is guarded against redeclaration. |

Install-time materialization is rejected because it copies plugin content into
user config, and those copies drift away from the plugin revision recorded in
the lockfile.

Executable adapters are invoked from TOML hooks through normal command lines.
Core does not gain a plugin-specific process protocol in this design. A plugin
may ship a workspace-provider/resource/task config that calls a binary in `bin/`; the
loader resolves those executable paths so a reference becomes a stable
absolute path.

An action naming a command on `PATH` names it directly:

```toml
[agent_runtime]
kind  = "effect"
scope = "run"

[agent_runtime.setup]
type    = "exec"
command = "agent-runtime"
args    = ["launch", "--workspace-dir", { from = "workspace.dir" }]
```

The same action against a plugin-shipped executable names the
catalog-qualified plugin reference instead of a command:

```toml
[agent_runtime.setup]
type = "exec"
bin  = "official/session/runtime/agent-runtime"
args = ["launch", "--workspace-dir", { from = "workspace.dir" }]
```

`bin = "<catalog-alias>/<plugin-path>/<executable>"` is the full form. To
parse it, plect compares the reference against enabled plugin identities in the
same catalog and selects the longest matching plugin path; the remaining
segment is the executable name. `bin = "<catalog-alias>/<plugin-path>"` is
shorthand only when the whole reference exactly matches one enabled plugin and
that plugin declares exactly one executable. A reference is a load error if the
same string can also be read as a shorter plugin plus executable, because no
slash-based syntax can disambiguate that collision under arbitrary-depth plugin
paths. A shorter reference that omits the catalog alias is a load error because
plugin paths are unique only inside a catalog. An executable segment that is
ambiguous inside the selected plugin is also a load error. For locked plugins,
resolution returns an absolute path inside the read-only mounted plugin cache.
For editable path catalogs, it returns an absolute path inside the directly
mounted development tree. It never writes resolved paths back into user
config.

The same reference and read-only-mount rules apply to scripts and built
binaries for locked plugins; editable path catalogs keep the same executable
lookup but use the development-mode mount exception described in the
resolution flow. Scripts are executed through their shebangs; an action still
names them through `bin`.

### Plugin-local `bin` resolution

The fully-qualified form above names a catalog alias — but a plugin's own
shipped config cannot know that alias in advance: `catalogs.toml` assigns it
per-user, at registration time, and nothing constrains it to `official`. A
plugin's workspace-provider/resource/task files that reference their own plugin's
executables by the fully-qualified form are wrong the moment a user
registers the catalog under any other alias.

`bin = "<name>"` with a bare executable name — no alias, no path segment —
resolves differently: against the *containing* plugin's own
`[[executables]]`, found from the file the reference was read from (the
loader already knows which mounted plugin any given config file came from).
No alias comparison happens at all, so the reference is correct under every
alias simultaneously:

```toml
# plugins/session/runtime/config/tasks/agent_runtime.toml, shipped inside
# the session/runtime plugin itself
[agent_runtime.setup]
type = "exec"
bin  = "agent-runtime"
args = ["launch", "--workspace-dir", { from = "workspace.dir" }]

[agent_runtime.cleanup]
type = "exec"
bin  = "agent-runtime"
args = ["stop", "--id", { from = "self.outputs.runtime_id" }]
```

This bare-name reading is available only inside plugin-mounted config: a
reference read from a file that was not mounted from any catalog plugin
(hand-authored global config, an ancestor overlay) has no containing plugin
to resolve against, and fails loud rather than guessing. The containing
plugin's own executable list is also the entire search space — a bare name
never matches another plugin's executable, even one mounted alongside it,
so two plugins may reuse the same executable name with no collision.

The fully-qualified `<catalog-alias>/<plugin-path>[/<executable-name>]` form
remains exactly as specified above, for user-authored config (global
config, ancestor overlays) that names any mounted plugin's executable by the
alias the user chose. A shipped plugin may not use that alias-qualified form:
the referencing plugin has no alias to name the other plugin with, and
cross-catalog references remain unsupported. Shipped plugin config can
reference only its own executables.

Alternatives considered:

- Environment-variable injection would require command strings like
  `$PLECT_PLUGIN_AGENT_RUNTIME_BIN launch ...`, which is longer, less readable,
  and introduces shell-specific quoting and naming rules into every hook.
- A symlink farm would make the TOML look like ordinary commands, but it creates
  another mutable filesystem surface to manage and debug.
- Adding plugin `bin/` directories to `PATH` preserves the old syntax, but
  same-name executables across plugin layers would become declaration-order
  behavior unless another conflict system wrapped `PATH`.

### Plugin Boundary Rule

Plecture has no plugin dependency mechanism. `plugin.toml` has no field for
naming another plugin, declaring capabilities, or requesting workspace-provider
resolution. plect does not acquire artifacts, resolve version ranges,
auto-enable plugins, or build transitive plugin graphs.

Plugin boundaries are governed by
[`plugin-boundary-contracts.md`](plugin-boundary-contracts.md). Shipped plugin
configuration references only executables declared by the same plugin. When a
reusable runtime contract would cross a plugin boundary, the contract either
belongs in core or the participants rendezvous through the event bus.
Task customization by additive nesting is governed by
[`task-nesting.md`](task-nesting.md); that design also specifies the
catalog-qualified task reference form shared by nested tasks and workflow
`uses`.

### Executable Build Model

Executable delivery is staged:

1. Stage 1, v1: build-less scripts are used directly from trusted plugin
   source, and compiled executables are built on the user's machine at
   add/update time. When `build` is absent, `path` must point at a script or
   executable file already present in the source tree. When an executable entry
   declares `build`, plect runs that command with the plugin's own root
   directory as the working directory and places the resulting executable in
   the plugin's cache bin location. A Go plugin source with one module directly
   under `src/` uses `go -C src build -o ../bin/<name> ./cmd/<name>`. A plugin
   with multiple independent modules uses the specific module directory, such
   as `go -C src/channel-server build -o ../../bin/<name> ./cmd/<name>`. `-C`
   changes the working directory before any other path on the command line is
   resolved, so `-o` must climb back to the plugin-root `bin/` that `path`
   names.
2. Stage 2, future direction: plect release artifacts may bundle
   Plecture-maintained plugin binaries as separate plugin packages that ride the
   release. They are not embedded in core, and activation still requires the
   user's declaration. Bundling is only a bootstrap seed: out-of-band
   git-reference updates must remain possible and take precedence when declared,
   because agent CLI churn must not wait for plect releases.
3. Stage 3, future signing story: per-plugin signed prebuilt release assets
   return with the archive source scheme. This removes the local toolchain
   requirement for third-party plugins at scale.

A Go toolchain is an explicit v1 requirement for plugins with Go build commands,
consistent with plect itself being installable through `go install` and with
Go-tooling norms such as user-built language servers and static-analysis tools.
The lock pins the plugin content hash, so a locally built binary is a
derivative of exactly the trusted content; the Git commit SHA is provenance for
where that content was acquired. That is a stronger verification chain than
unsigned prebuilt distribution. Running a build command is within the trust
already granted by confirming the catalog registration, because plugin config
can run shell during normal operation.
Plecture-maintained plugins also remain available through the existing Nix path.

## Reference resolution

Users register catalogs and enable selected plugins in trusted user-owned
config, not inside cloned workspace-dir content. The global declaration file lives at:

```text
~/.config/plect/catalogs.toml
```

`~/.config/plect` here is the default config home. `--config-home` overrides
it for the whole config tree (`config.toml`, `catalogs.toml`, `plect.lock`, and
the global `tasks/`, `workflows/`, `workspaces/`, `resources/`, and
`channels/` overlays); absent that flag, `PLECT_CONFIG_HOME` wins, then
`$XDG_CONFIG_HOME/plect`, then the `~/.config/plect` default. The plugin
cache and runtime state resolve independently of the config home, each
through its own flag/env pair: `--data-home`/`PLECT_DATA_HOME`/
`$XDG_DATA_HOME/plect`/`~/.local/share/plect` for runtime state, and
`--cache-home`/`PLECT_CACHE_HOME`/`$XDG_CACHE_HOME/plect/catalogs`/
`~/.cache/plect/catalogs` for the resolved catalog snapshots this section
describes.

Example:

```toml
schema_version = 2

[[catalogs]]
alias = "official"
source = "git+https://github.com/example/plect-plugins"
plugins = ["github", "session/runtime"]

[[catalogs]]
alias = "team"
source = "git+ssh://git@example.com/team/plect-catalog"
plugins = ["session/runtime"]

[[catalogs]]
alias = "local"
source = "path:///home/user/src/plect-catalog"
plugins = ["okf"]

[[catalogs]]
alias = "mono"
source = "git+https://github.com/example/monorepo"
subdir = "plugins"
plugins = ["github"]
```

Declaration file fields:

| Field | Required | Meaning |
|---|---:|---|
| `schema_version` | yes | Declaration file-format version. Unknown values fail loud. |

Catalog entry fields:

| Field | Required | Meaning |
|---|---:|---|
| `alias` | yes | User-chosen local catalog alias. It is the trust name and the first segment of plugin identities. |
| `source` | yes | Exact catalog source, not a prefix. Supported v1 schemes are `git+https`, `git+ssh`, `path`, and `path+editable`. The source value never carries a revision. |
| `subdir` | no | Catalog-relative subdirectory of the fetched source that becomes the catalog root; `catalog.toml` is read from there. Empty means the source root itself. Written by `plect catalog add --subdir <path>`, never inferred. |
| `plugins` | yes | Catalog-relative plugin paths enabled from this catalog. Each path forms a catalog-qualified plugin identity with the alias, `<catalog-alias>/<relative-path>`. |

`subdir` exists as its own field, not a suffix folded into `source` (no
Terraform-style `//subdir`, no nix-style `?dir=` query), for the same reason
`source` never carries a revision: the locator stays a plain, tool-agnostic
URL or path, and every plect-specific modifier is its own explicit,
independently-settable field.

A `catalogs` entry is the trust act. Each registration binds a user-chosen alias
to one exact source and the plugin paths enabled from that source. It does not
pin selected plugin content; per-plugin lock entries carry the resolved
acquisition coordinates and content hashes. Default alias suggestions belong in
provider README install snippets, not in any manifest. Core contains no official
host, owner, registry, provider list, or prefix policy.

Ownership stays split by file. `config.toml` is human-only. `catalogs.toml` is
human-owned, but plect appends or edits it only in direct response to explicit
human commands: `plect catalog add` writes a new `[[catalogs]]` entry after
interactive trust confirmation, `plect plugin add` appends a path to that
entry's `plugins` list, and `plect plugin remove` removes that path. `plect.lock`
is tool-only; resolved revisions and content hashes live there, never in
`catalogs.toml`.

Git is the default transport for reference distribution, not a requirement of
the resolution model. The model needs only fetch, revision or hash
verification, and read-only mount; the `path` scheme is the git-free floor. Core
invokes git as an external command and does not take a git library dependency.
Environments without git lose only the git schemes, with a clear error.

Resolution flow:

1. Read catalog registrations and nested plugin selections from global user
   config.
   The first implementation does not read them from ancestor overlays.
2. For each referenced catalog, resolve the registration to concrete content:
   - Git catalog: fetch the source and resolve the requested revision supplied by
     the add or update command to a commit SHA. `--revision` accepts tags and
     branches as input, but the lockfile records only the resolved commit SHA,
     never the symbolic ref. The source in `catalogs.toml` remains revision-free.
     The lockfile's catalog record is the durable home for the last explicitly
     trusted catalog snapshot; plugin lock entries remain the usage pins.
   - Locked path catalog: resolve symlinks and verify selected plugin
     directories against their per-plugin tree SHA-256 hashes. Path sources have
     no catalog revision.
   - Editable path catalog: resolve symlinks and mount the path directly. It
     records neither a catalog revision nor a content hash and is therefore
     non-reproducible.
3. Join the registration's `subdir`, if any, onto the fetched source root —
   the result is the catalog root — with the same realpath containment check
   used for plugin paths in step 4: a `subdir` that escapes the fetched
   source via `..` or a symlink is a load error, not a silent clamp. Find
   `catalog.toml` at that catalog root and fail if `schema_version` is
   unknown.
4. Validate every listed plugin path with realpath containment inside the
   catalog root, and fail on unlisted `plugin.toml` files inside the catalog.
5. For each enabled plugin id, split the catalog alias from the relative path
   and fail if the alias is not registered or the path is not listed by that
   catalog.
6. Verify the plugin's lock entry against the catalog acquisition coordinate
   and plugin content hash, except editable path catalogs. For Git catalogs, the
   acquisition coordinate is the resolved commit SHA and the content hash is
   computed over the catalog trust-space subtree actually mounted for the
   plugin. The SHA identifies the repository snapshot; the content hash verifies
   the trusted bytes. Recording both also avoids relying on Git SHA-1 collision
   resistance alone. Path catalogs hash each selected plugin directory, not the
   whole catalog subtree, so changing one local plugin does not force unrelated
   sibling plugins to move.
7. Read the plugin's `plugin.toml` and fail if `schema_version` is unknown.
8. Check `plect_min_version` against the running plect version.
9. Mount the plugin directory as a read-only base config layer, except editable
   path catalogs, which are an explicit local development escape hatch.

Core should materialize resolved catalog snapshots under a cache owned by the
user, for example:

```text
~/.cache/plect/catalogs/<source-digest>/<lock-coordinate>/
```

The source digest is derived from the exact registered source, not from the
user-local alias. Reusing an alias for a different catalog therefore cannot
reuse the old catalog's cache namespace. The lock coordinate is the resolved
commit SHA for Git catalogs and the plugin content hash for locked path
catalogs. Editable path catalogs are mounted directly and are not cached.

`plugin_dirs` is the base-layer mechanism itself: catalog resolution's step 9
above mounts each resolved plugin directory into that same list, alongside any
hand-authored entries. The two kinds are not equivalent — only a
catalog-resolved entry carries lock verification and the `plect_min_version`
check; a hand-authored entry carries no catalog identity and skips both.

Reference declarations are global-only in the first implementation. Ancestor
overlays can still customize tasks and workflows under the existing loader
rules, but they cannot register catalogs or select new plugins. The workspace
directory's own `.plect/` directory is also excluded because cloned content must not fetch
or run code.

Alternatives considered: encoding `subdir` into the source locator itself
(Terraform's `//subdir` suffix, nix's `?dir=` query parameter) is rejected
for the same reason a revision is never embedded in `source` — see
`subdir`'s field entry above.

Alternatives considered: `archive+https` is rejected from v1. Current adopters
are a single user plus a planned team, and both source plugins from Git. The
`path` scheme remains the git-free floor for local development. Archive
distribution returns with the signed prebuilt-asset story, not as an unsigned v1
transport.

## Lockfile

The lockfile records exactly what was mounted. The default path is:

```text
~/.config/plect/plect.lock
```

Because catalog registrations and nested plugin selections are global-only in the
first implementation, this lockfile is global-only too. A workspace-dir-owned
lockfile is ignored for plugin resolution.

Example:

```toml
schema_version = 2

[[catalogs]]
alias = "official"
catalog_source = "git+https://github.com/example/plect-plugins"
catalog_resolved_revision = "4f2db5e4c2b4b4a8c6c0f6c0d4d2d2ecf0c1a0b3"

[[catalogs]]
alias = "mono"
catalog_source = "git+https://github.com/example/monorepo"
subdir = "plugins"
catalog_resolved_revision = "9c1e2a3b4d5e6f708192a3b4c5d6e7f809182a3b"

[[plugins]]
id = "official/github"
catalog_alias = "official"
catalog_source = "git+https://github.com/example/plect-plugins"
catalog_resolved_revision = "4f2db5e4c2b4b4a8c6c0f6c0d4d2d2ecf0c1a0b3"
path = "github"
content_hash = "sha256:..."
version = "0.3.0"
plect_min_version = "0.8.0"
editable = false

[[plugins]]
id = "official/session/runtime"
catalog_alias = "official"
catalog_source = "git+https://github.com/example/plect-plugins"
catalog_resolved_revision = "4f2db5e4c2b4b4a8c6c0f6c0d4d2d2ecf0c1a0b3"
path = "session/runtime"
content_hash = "sha256:..."
plect_min_version = "0.8.0"
editable = false

[[plugins]]
id = "local/okf"
catalog_alias = "local"
catalog_source = "path:///home/user/src/plect-catalog"
path = "okf"
content_hash = "sha256:..."
plect_min_version = "0.8.0"
editable = false
```

Catalog record fields:

| Field | Meaning |
|---|---|
| `alias` | User-chosen catalog alias from `catalogs.toml`. |
| `catalog_source` | Catalog source from `catalogs.toml`. It never includes a symbolic Git revision. |
| `subdir` | Copy of `catalogs.toml`'s `subdir` for this alias, empty when unset. Checked for drift the same way `catalog_source` is: if a hand-edited `catalogs.toml` no longer matches, resolution fails loud rather than silently trusting a different subtree than the one last confirmed. The fix is `plect catalog remove <alias>` then `plect catalog add` again — not `plect catalog update`, which performs the identical drift check itself and refuses for the same reason (see below). |
| `catalog_resolved_revision` | Git-only immutable catalog revision. It is always the resolved commit SHA from the last explicit catalog add or catalog update. |

Plugin record fields:

| Field | Meaning |
|---|---|
| `id` | Catalog-qualified plugin identity, `<catalog-alias>/<relative-path>`. |
| `catalog_alias` | User-chosen catalog alias from the registration. |
| `catalog_source` | Catalog source from `catalogs.toml`. It never includes a symbolic Git revision. |
| `catalog_resolved_revision` | Git-only immutable catalog revision. It is always the resolved commit SHA. |
| `path` | Plugin path relative to `catalog.toml`. |
| `content_hash` | Digest of the mounted plugin directory after checkout or path resolution. For Git catalogs, this verifies the trust-space subtree content independently of the commit SHA. For locked path catalogs, this tree SHA-256 is the only pin. Editable path catalogs omit it. |
| `version` | Optional plugin package version read from metadata when present. |
| `plect_min_version` | Compatibility floor read from metadata. |
| `editable` | Whether the catalog was mounted in explicit local development mode. |

Add and update commands:

- `plect catalog add <alias> <source> [--subdir <path>] [--revision <rev>]`
  resolves the catalog, normalizes the source into an exact registered
  source, joins `--subdir` (if given) onto the fetched root, reads
  `catalog.toml` from the result, shows the exact source, subdir, resolved
  lock coordinate, manifest description, and plugin paths, and asks for
  interactive confirmation. On consent, it writes a new `[[catalogs]]` entry
  with `alias`, revision-free `source`, `subdir`, and an empty `plugins`
  list. For Git sources, it also writes a catalog lock record with the
  resolved commit SHA and the same `subdir`. That record is the durable home
  for a `--revision` input until plugin entries are added; `catalogs.toml`
  still records no revision, and plugin lock entries record only resolved
  commit SHAs.
- `plect catalog update <alias> [--revision <rev>]` fetches the newest matching
  catalog snapshot and repoints every enabled plugin from that catalog to the
  new snapshot with fresh per-plugin lock entries. When `--revision` is
  supplied for a Git catalog, it is a catalog-level intent change: after
  confirmation, plect resolves the requested revision to a commit SHA and
  records that SHA in the catalog lock record and each updated plugin lock
  entry. It does not write the requested revision to `catalogs.toml`.
  `subdir` has no update-time flag: it is a registration-time decision read
  back from `catalogs.toml`, re-applied to the new snapshot unchanged;
  changing which subtree is trusted is a new `catalog add`, not an update.
  Before fetching anything, `catalog update` (and `plugin add`/`plugin
  update` below) compare `catalogs.toml`'s current `source`/`subdir` for the
  alias against the last catalog lock record and refuse if they disagree —
  a hand-edited registration is exactly the drift
  `catalog_source`/`subdir`'s field entries above describe, and none of
  these commands is the confirmation step that is allowed to accept it
  silently. Only `plect catalog remove` + `plect catalog add` can.
- `plect catalog remove <alias>` shows the enabled plugins that would be
  disabled, requires confirmation in interactive contexts, then removes the
  catalog registration and those plugin selections and lock entries.
- `plect catalog list` shows registered aliases, sources, catalog validation
  state, and a summary of enabled plugin lock coordinates grouped by catalog. It
  does not imply that one resolved Git commit applies to every enabled plugin in
  the catalog.
- `plect plugin add <alias>/<path>` enables one plugin path from a registered
  catalog. For Git catalogs, it uses the catalog lock record when one exists;
  otherwise it resolves the registered source and writes the resulting commit SHA
  to both the catalog lock record and the plugin lock entry. It also adds the
  path to that catalog entry's `plugins` list.
- `plect plugin update <alias>/<path> [--revision <rev>]` fetches the newest
  matching catalog snapshot, validates the catalog, and repoints only that
  plugin's lock entry. When `--revision` is supplied, it affects only that
  plugin's lock entry and does not rewrite the catalog registration. Tags and
  branches are accepted as input for Git catalogs, but the lock records the
  resolved commit SHA. Other plugins from the same catalog keep their previous
  locked coordinates; cache snapshots coexist by source digest and lock
  coordinate.
- `plect plugin remove <alias>/<path>` disables that plugin and removes its lock
  entry, and removes the path from that catalog entry's `plugins` list.
- `plect plugin verify` re-hashes cached plugin directories and fails if any
  differ from the lockfile.
- `plect plugin list` shows selected, resolved, locked, and compatibility state.
- `plect plugin show <alias>/<path>` shows metadata, executables, services,
  compatibility, and lock coordinates.

Non-interactive contexts fail instead of prompting for first-seen catalog
registrations. Any override flag must be explicit and visible in command
history.

No command silently advances a plugin. A missing or mismatched lock entry is a
load error unless the user is running an explicit add or update command.
Editable path catalogs are the only exception: they are marked non-reproducible
in `plect plugin list`, excluded from `plect plugin verify --locked`, and meant
only for local plugin development.

`plect.lock` carries mechanical pinning only: catalog source, Git commit SHA
when applicable, plugin path, plugin content hash, metadata, and editable state.
Trust policy stays in catalog registrations; the lockfile never records trust
semantics or symbolic Git refs.

Alternatives considered:

- Metadata-supplied plugin identities were rejected because collisions move up
  to a flat namespace and local renaming becomes a special case. Filesystem
  paths already provide intra-catalog uniqueness.
- A central registry or marketplace layer was rejected for v1 because it needs
  central infrastructure and a signing story. It can be revisited with signed
  catalogs or packages.
- Catalog-level usage pinning was rejected because it forces a single revision
  across all enabled plugins from one catalog. That reproduces known friction
  from nix-flake-style updates when the user wants to update one package while
  leaving siblings untouched.
- The accepted UX follows Helm and Homebrew: users choose local aliases and
  update individual packages. The trust boundary follows Go modules: the
  manifest position defines the subtree. Reproducibility comes from lock-based
  pinning, which Helm-style indexes do not provide by themselves.
- The user-side registration file is named `catalogs.toml` because entity-plural
  registration files match Helm `repositories.yaml` and apt `sources.list`.
  Installed-state names were rejected because this file declares desired state,
  not installed state. `trusted_`-prefixed names were rejected because trust is a
  property of the add-time confirmation procedure, not of the file. The
  one-word overlap with the catalog's `catalog.toml` is handled in prose by
  possessive phrasing: your `catalogs.toml` versus the catalog's `catalog.toml`.

## Trust boundary

Plecture config can run shell on the user's machine. Plugin config has the same
power because workspace provider setup, resource observe/finalize, task
setup/cleanup, and exec channels all execute commands.

Threat model:

- The config directory, including catalog registrations, is the root of trust.
  It is in the same trust boundary as the `plect` binary because Plecture config
  is effectively executable shell already.
- Catalog registrations are policy and `plect.lock` is a mechanical record.
  Keeping them separate preserves their ownership split: humans edit policy;
  tooling writes the lock.
- A catalog registration edit alone changes nothing mounted. Content
  immutability still comes from `plect.lock`: Git catalogs record the resolved
  commit SHA plus the trust-space content hash, locked path catalogs record the
  tree SHA-256, and editable path catalogs are explicitly non-reproducible.
- Plugin selections are global-only. A dispatched session's workspace
  directory cannot inject plugins or trust new catalogs through cloned content.
- Catalog registration and lock changes are human-governed actions. The `plect`
  command may edit `catalogs.toml` and `plect.lock` only as the direct effect of
  explicit human commands; agent-driven workflows must not edit them
  automatically and should fail or escalate instead.
- First-seen catalogs require interactive confirmation through
  `plect catalog add`. Non-interactive contexts fail unless the user supplied an
  explicit override that remains visible in command history.
- Remote attacks are constrained by transport authentication plus lock
  verification: trust is established at catalog registration time, then the
  resolved Git commit SHA and trust-space content hash are verified on every
  load.
- Users can point at any catalog they choose to trust. Third-party installation
  is possible by design, and responsibility for trusting a catalog lies with the
  human who confirmed it. Deferred signing is what makes third-party consumption
  safe at scale, not what makes it possible.
- No auto-update exists. Plugin directories mounted by core are read-only during
  normal command execution.
- Workspace-dir-owned cloned content cannot declare plugin references,
  workspace providers, resources, channels, or task definitions.

Loader trust restrictions follow this shape:

- Workspace providers, resources, and channels load only from plugin dirs and
  global config.
- Tasks reject definitions inside the workspace-dir layer.
- Workflows allow workspace-dir files only to add nodes.

## Shadowing and precedence

For loaders that receive plugin dirs, resolution order is:

1. Plugin layers, in declaration order.
2. Global user config.
3. Trusted ancestor overlays, outermost to innermost.
4. Workspace-dir `.plect/` overlay, with the same restrictions.

Each mounted plugin is its own namespace, addressed by its catalog alias and
plugin path, and the user-owned layers are one namespace between them. Two
plugins may therefore declare one id, and a user-owned declaration sharing an id
with a plugin's does not replace it: the two hold different addresses, and which
one a reference selects follows from how the reference is written. Two plugin
layers with no catalog identity are the exception, because no address can name
them apart, so declaration order would be choosing: that stays a load error the
user resolves by enabling them through a catalog or replacing one definition in
global config.

Same-id behavior by kind, within one namespace:

| Kind | Same-id rule |
|---|---|
| Workspace providers, resources, channels | A deeper user-owned layer replaces the whole definition. No partial override. |
| Tasks | A deeper user-owned layer replaces the whole definition. No partial override. |
| Workflows | User-owned layers merge by adding nodes. Every other field a shallower layer set cannot be redeclared, except runtime tuning tables where deeper trusted layers replace the whole table. |
| Workflow input schemas | A workflow's input contract is stated by one layer. A deeper layer redeclaring it is a load error, since a contract no single layer states is one no author can read. |

Partial override model:

- To partially customize a workflow you own, add a same-id workflow declaration
  in a trusted overlay that adds new `[[nodes]]` entries. A plugin's workflow is
  a separate address and is not extended this way; a workflow that starts from
  one is a copy.
- To stand a task, workspace provider, resource, or channel in for a plugin's,
  declare your own and reference yours. Yours answers to its bare id and the
  plugin's to its address, so the reference is what chooses.
- A plugin-provided task cannot be edited in place through a patch mechanism.
  Whole-definition replacement keeps arbitrary shell behavior auditable.

This model keeps the existing loader semantics and makes plugin distribution a
new source of base layers rather than a new merge language.

Alternatives considered: letting declaration order decide same-id plugin
conflicts would make behavior depend on list order in `catalogs.toml`. That is
too easy to miss during review, especially for executable shell hooks, so plugin
layer conflicts fail loud.

## Compatibility

A plugin with an unsatisfied `plect_min_version` must fail loud before any of
its config is mounted. The error should name the plugin id, required version,
and running version.

Compatibility checks happen in this order:

1. Catalog resolution and catalog manifest validation.
2. Plugin path validation and lockfile verification.
3. Plugin metadata parse.
4. `plect_min_version` check.
5. Config load and existing schema/contract validation.

There is no silent degradation. A plugin cannot say "try this weaker behavior
on old core."

Compatibility metadata decisions:

- `plect_min_version` is the only generic compatibility field before 1.0. A
  maximum tested version is rejected because an advisory field with no current
  consumer is speculative. Revisit only if real incompatibility reports
  demonstrate the need.
- Executable adapter protocol versions remain adapter-specific documentation
  unless core later defines a generic adapter protocol.

## Walkthrough examples

### Runtime and delivery plugins

The runtime surface is split into plugins for the terminal
multiplexer, each agent runtime, and chat delivery. The split uses the
core-owned contracts described in
[`plugin-boundary-contracts.md`](plugin-boundary-contracts.md): an interactive
endpoint for text-terminal runtime access and conversation events for chat and
agent message exchange.

Catalog registration and plugin enablement:

```bash
plect catalog add official git+https://github.com/example/plect-plugins --revision v0.3.0
plect plugin add official/tmux
plect plugin add official/claude
plect plugin add official/slack
plect plugin update official/claude
```

`plect plugin update official/claude` fetches the newest catalog
snapshot and repoints only `official/claude`; `official/tmux`,
`official/slack`, and `official/github` stay at their locked
coordinates until explicitly updated.

Catalog-owned files:

```text
catalog.toml
tmux/plugin.toml
tmux/config/tasks/pane.toml
claude/plugin.toml
claude/config/tasks/runtime.toml
claude/config/tasks/initial_prompt.toml
claude/config/channels/delivery.toml
claude/scripts/claude-agent-activity
claude/src/channel-server/go.mod
claude/src/channel-server/cmd/channel-server/main.go
codex/plugin.toml
codex/config/tasks/codex.toml
codex/config/tasks/initial_prompt.toml
codex/config/channels/terminal_submit.toml
codex/config/tasks/exec_runtime.toml
codex/config/channels/exec_delivery.toml
codex/scripts/codex-agent-activity
codex/src/codex-helpers/go.mod
codex/src/codex-helpers/cmd/codex-exec-worker/main.go
codex/src/codex-helpers/cmd/codex-exec-enqueue/main.go
slack/plugin.toml
slack/config/channels/slack.toml
slack/src/slack-adapter/go.mod
slack/src/slack-adapter/cmd/slack-adapter/main.go
```

`claude/bin/channel-server`, `codex/bin/codex-exec-worker`,
`codex/bin/codex-exec-enqueue`, and
`slack/bin/slack-adapter` are what `plect plugin add`/`update`
produce from source modules at add/update time — build output, not catalog
content, so they are never committed (see the Package format section's
`src`/`bin`/`scripts` split).

Residual user config:

- Which multiplexer and agent runtime are selected for a workflow.
- Local command path or model defaults if they differ from plugin defaults.
- Event channel bindings for the user's runtime session and team conversation.
- Team-specific workflow overlays that add local notification or review nodes.
- Slack credentials or environment files. Without credentials, the Slack
  service remains inert.

No workspace-provider, task, workflow, or channel contract change is required. The
workflow still references tasks and channels by their existing ids.
The tmux plumbing is genuinely shared runtime logic with no agent-specific
branch. Agent activity remains de-generalized: Claude and Codex each carry a
small branch-free activity script instead of sharing one helper that switches on
agent kind.

### GitHub plugin

The GitHub plugin remains a plugin because URL parsing, branch lookup, and
worktree acquisition are GitHub-specific.

Catalog registration and plugin enablement:

```bash
plect catalog add official git+https://github.com/example/plect-plugins --revision v0.3.0
plect plugin add official/github
plect plugin update official/github
```

`plect plugin update official/github` fetches the newest catalog snapshot and
repoints only `official/github`; `official/session/runtime` stays at its
locked coordinate.

Catalog-owned files:

```text
catalog.toml
github/plugin.toml
github/config/workspaces/worktree.toml
github/config/resources/github.toml
github/config/tasks/github_work.toml
github/config/tasks/github_review.toml
github/config/workflows/github_coding.toml
github/src/go.mod
github/src/cmd/github-worktree/main.go
github/src/cmd/github-issue-pr/main.go
github/src/cmd/github-watcher/main.go
```

`github/bin/github-worktree`, `github/bin/github-issue-pr`, and
`github/bin/github-watcher` are what `plect plugin add`/`update` produce
from `github/src` at add/update time — build output, not catalog content,
so they are never committed (see the Package format section's
`src`/`bin`/`scripts` split).

Residual user config:

- Resource allowlist entries for allowed owners or repositories.
- Authentication outside Plecture, such as the GitHub CLI token.
- Workspace dirs root choice.
- Which session-runtime task surface the user composes the GitHub workflow with.
- Project-board or watcher subscriptions that are local operating policy.

Core still sees only workspace provider, resource, task, workflow, and
channel contracts. It does not parse GitHub URLs or know GitHub exists.

### okf plugin

The okf plugin is scoped by the OKF specification, not by one use case. Its
first version owns the goal resource mechanics that have machine semantics
(observation, finalization, and workspace dispatch) and the `goal_bootstrap`
effect that re-creates a dropped instance of a host-owned goal task. Bundle
records that do not have machine semantics, such as retrospectives, stay
plain files outside the plugin.

Catalog registration and plugin enablement:

```bash
plect catalog add local path:///home/user/src/plect-catalog
plect plugin add local/okf
plect plugin update local/okf
```

`plect plugin update local/okf` refreshes the catalog snapshot used by
`local/okf` only. Other enabled plugins from `local` remain pinned to their
current lock entries.

Catalog-owned files:

```text
catalog.toml
okf/plugin.toml
okf/config/workspaces/local_okf.toml
okf/config/resources/okf_goal.toml
okf/config/tasks/goal_bootstrap.toml
okf/src/go.mod
okf/src/cmd/okf-goal/main.go
okf/src/cmd/okf-bundle/main.go
```

`okf/bin/okf-goal` and `okf/bin/okf-bundle` are what `plect plugin
add`/`update` produce from `okf/src` at add/update time — build output, not
catalog content, so they are never committed (see the Package format
section's `src`/`bin`/`scripts` split).

Plugin-owned behavior:

- Goal resource id syntax.
- Workspace dispatch for a `local-okf://` resource: a read-context workspace
  directory symlinked into the owner's bundle.
- Goal observation and finalization entrypoints.
- Revision and checklist status reporting for goal resources.
- Idempotent completion logging for goal resources.
- The `goal_bootstrap` effect, which re-creates a dropped `pursue_goal`
  instance for every open goal a host's own `pursue_goal` task document
  admits.

Internally separable plugin behavior:

- Owner alias resolution.
- Bundle root discovery.
- Bundle containment checks.
- Frontmatter parsing shared by future OKF concepts.

Not plugin-owned:

- Retrospectives or other bundle records without machine semantics.
- The `pursue_goal` and `goal_review` task documents themselves: which agent
  reviews a goal, on what instruction, and under what completion gates is a
  choice the OKF specification does not make, so the host declares both.
  `goal_bootstrap` has nothing to instantiate until the host's own
  `pursue_goal` document exists.
- The `goal_review` workflow itself: not runnable config this plugin ships.
  The host must define its own `goal_review` workflow before a host-defined
  `pursue_goal` chain can spawn one.

Residual user config:

- Which goal roots or owners are allowed.
- Which orchestrator workflow is used.
- The `pursue_goal` and `goal_review` task documents, and the `goal_review`
  workflow and session runtime that handles the work — an operator defines
  these against their own agent-runtime and channel plugins, or against a
  team-owned overlay (see `plugins/okf/README.md` and
  `docs/language/tasks.md`).
- Any local overlay that maps goal review into the team's workflow shape.

No workspace-provider/resource/task/workflow contract changes are needed. If the plugin
discovers that goal state needs data the existing resource observation contract
cannot express, that belongs in a later contract issue.
