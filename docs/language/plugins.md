# Plugin and catalog manifests

A plugin is a distributable package: executable adapters, config resources, and
metadata. Its manifest and its catalog's manifest are TOML, but they are not
definition documents — they sit outside a definition root and carry no `kind`.

## plugin.toml

`plugin.toml` lives at a plugin's root and declares the package's identity, the
executables it owns, and the services the resident process supervises.

```toml
schema_version    = 3
version           = "0.1.0"
plect_min_version = "0.0.0"
description       = "GitHub resource, workspace effect, and watcher subscription."

[[executables]]
name  = "github-worktree"
path  = "bin/github-worktree"
build = "go -C src build -o ../bin/github-worktree ./cmd/github-worktree"

[[executables]]
name = "gh-guard"
path = "scripts/gh-guard"

[[executables]]
name = "github-watcher"
path = "bin/github-watcher"

[[services]]
name       = "github-watcher"
executable = "github-watcher"
args       = ["serve"]
restart    = "on-failure"
health     = { type = "process" }
```

| Field | Meaning |
|---|---|
| `schema_version` | Manifest format, so a manifest is identifiable before anything else is read. |
| `version` | The package's own version. |
| `plect_min_version` | The lowest `plect` this plugin loads against. |
| `description` | What the package is for. |
| `[[executables]]` | `name`, `path`, and an optional `build` command run at install or update time. |
| `[[services]]` | A long-running process the resident supervisor owns. |

An executable's `name` is what a `bin` reference resolves; `path` is relative to
the plugin root. `build` is literal shell, run at install or update time rather
than at session time.

A service declares the `executable` it runs, its `args` and `env`, the
`required_env` that must be non-empty for it to start, a `restart` policy, and a
`health` type. `required_env` is what keeps an unconfigured service inert
instead of crash-looping: a service whose credentials or target are unset
should stay stopped, not restart forever.

A service's `executable` names one of this manifest's own executables.

## catalog.toml

`catalog.toml` lives at a catalog root and declares which plugins the catalog
publishes.

```toml
schema_version = 3
description    = "Plecture's official plugin catalog: reusable resource, effect, task, and channel packs."

plugins = [
  "tmux",
  "claude",
  "codex",
  "slack",
  "okf",
  "github",
]
```

Explicit enumeration, not directory presence, is what publishes a plugin. A
directory inside the catalog's trust space that the manifest does not list is
not catalog-addressable — nor is a listed plugin's own build source.

## Config resources

A plugin's `config/` directory is its definition root. Its layout is author
organization only: one definition per file, or kind-named subdirectories, mean
the same thing. See [`declarations.md`](declarations.md).

A plugin's definitions form one id namespace across all kinds, and every
reference written inside a plugin is relative.

## Validation rules

- `schema_version`, `version`, `plect_min_version`, and `description` are
  required in a plugin manifest.
- `schema_version` and `plugins` are required in a catalog manifest.
- An executable `name` is unique within a manifest.
- A service's `executable` names a declared executable.
- A plugin's `plect_min_version` must not exceed the running `plect`.
