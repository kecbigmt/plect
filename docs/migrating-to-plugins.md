# Migrating a hand-authored config to catalog plugins

This guide is for an operator whose global plect config tree predates
catalog plugins: workspace providers, resources, tasks, workflows, and
channels that were hand-authored or copied into `~/.config/plect` (or
whatever directory `PLECT_CONFIG_HOME`/`--config-home` points at) directly,
possibly wired up through hand-authored `plugin_dirs` entries in
`config.toml`. It walks that config tree to a plugin-consuming one: a
`catalogs.toml` registration, enabled plugins recorded in `plect.lock`, and
only genuinely local choices left as residual global config. See
[`docs/design/plugin-packaging.md`](design/plugin-packaging.md) for the
full model this guide cuts over to — catalogs, plugins, reference
resolution, the lockfile, and precedence — and each plugin's own `README.md`
for what it ships.

Nothing here is specific to this repository's own catalog. The steps apply
to any catalog source (`git+https`, `git+ssh`, `path`, or `path+editable`)
and any plugin set; examples below register this repository's own catalog
because it is the one guaranteed to be reachable while following along.

## 0. Prerequisites

- A `plect` binary recent enough to have `plect catalog` and `plect plugin`
  (`plect catalog add --help` succeeds).
- Git on `PATH` if any candidate catalog uses a `git+https`/`git+ssh`
  source. `path`/`path+editable` sources need no git.
- A Go toolchain if any plugin you plan to enable declares an `[[executables]]`
  entry with a `build` command in its `plugin.toml` — `plect plugin add`/
  `update` runs that build locally.

## 1. Inventory: what becomes redundant, what stays

For each plugin you intend to enable, diff your hand-authored config
against that plugin's shipped content, by **definition id, not filename** —
a workspace provider, resource, task, workflow, or channel keeps its id
across the move, but the file that carries it may not keep the same name or
path.

For every standard config declaration a candidate plugin ships under
`config/`, compare the plugin path with the `config/` prefix stripped against
the matching path under your config home. For example, plugin file
`config/tasks/pane.toml` maps to config-home file `tasks/pane.toml`.

| Comparison result | Disposition |
|---|---|
| Your file defines the same id with identical (or now-superseded, less-capable) content | Redundant. Delete it in the cutover step. |
| Your file defines the same id with content that differs on purpose (a local patch, a house style, a different flag) | A divergence — do not delete it yet. Handle it in [Divergence handling](#4-divergence-handling) below. |
| Your file defines an id the plugin does not ship at all | User-owned residue. It stays, unchanged, as global config. |

Typical residue, independent of which plugins you enable: resource
allowlist entries (`resource_allowlist` in `config.toml`), `workspace_dirs_root`,
authentication configured outside plect, and team-specific workflow overlays
that only add nodes or channels to a plugin workflow.

Also note every `config.toml` `plugin_dirs` entry that points at a
directory whose content a candidate plugin now supersedes. These are the
entries [step 3](#3-cutover) removes — see the warning there for why leaving
them in place is not just redundant but breaks loading.

## 2. Verify in isolation

Do this before touching your real config home. Point `PLECT_CONFIG_HOME` at
an empty scratch directory so a mistake here cannot affect production
sessions; the plugin cache and runtime state still resolve from their own
real defaults (they are keyed by source and lock coordinate, not by config
home, so cache reuse across environments is safe), only `config.toml`,
`catalogs.toml`, `plect.lock`, and the global overlays move. To isolate the
plugin cache too — for a rehearsal that must not touch the host's cache at
all — also point `PLECT_CACHE_HOME` (or `--cache-home`) at a scratch
directory; it is never passed on to a child a declaration starts.

```bash
export PLECT_CONFIG_HOME="$(mktemp -d)"

plect catalog add official git+https://github.com/kecbigmt/plecture --subdir plugins --revision main
plect plugin add official/github
plect plugin verify
plect plugin list
```

`plect catalog add` shows the resolved source, lock coordinate, and
published plugin paths, and asks for interactive confirmation before
writing anything — confirm only after checking those against what you
expect. Repeat `plect plugin add <alias>/<path>` for every plugin from your
inventory. `plect plugin verify` re-hashes the mounted content against
`plect.lock` and should report every plugin `ok`.

Check that plugin-shipped workflows resolve:

```bash
plect workflow list
```

If a workflow can be exercised safely against a disposable resource
(harmless issue/branch, throwaway repo, or similar), run a dispatch smoke
test end to end:

```bash
plect up <resource-id> --workflow <workflow-id> --detach
plect status <resource-id>
plect destroy <resource-id>
```

A clean `plect status` with the tasks you expect and a clean `plect destroy`
confirm the plugin's setup/cleanup hooks actually run, not just that its
TOML parses. Once satisfied, discard the scratch directory:

```bash
rm -rf "$PLECT_CONFIG_HOME"
unset PLECT_CONFIG_HOME
```

## 3. Cutover

Back up first. The whole config home is the unit to copy, not individual
files — `catalogs.toml`, `config.toml`, and the global overlay directories
all change together in this procedure:

```bash
cp -r ~/.config/plect ~/.config/plect.pre-plugins-backup
```

(Substitute your actual config home if `PLECT_CONFIG_HOME`/`--config-home`
overrides the default.)

Then, against your real config home:

1. Register each catalog identified in the inventory step, the same way as
   in verification:

   ```bash
   plect catalog add <alias> <source> [--revision <rev>]
   ```

   Non-interactive contexts (scripts, CI) must pass `--yes` explicitly —
   there is no silent default confirmation.

2. Enable every plugin from the inventory:

   ```bash
   plect plugin add <alias>/<path>
   ```

3. Delete the config files identified as redundant in step 1: hand-authored
   files whose ids now come from enabled plugin files under `config/` after
   stripping that prefix.

4. Remove the `plugin_dirs` entries noted in step 1 whose content the newly
   enabled plugins now supersede.

   **This step is not optional.** Same-id conflicts between plugin layers
   are load errors, not a declaration-order pick — a hand-authored
   `plugin_dirs` entry that still defines the same workspace provider, resource, task,
   workflow, or channel id as a newly enabled catalog plugin makes every
   later `plect` invocation fail to load, with an error naming the
   conflicting id and both plugin layers. `config.toml`'s `plugin_dirs`
   entries not superseded by any enabled plugin are residue and stay.

5. Leave every other file identified as residue untouched.

Run `plect plugin verify` once more against the real config home to confirm
the mounted content still matches `plect.lock`, then exercise your normal
workflows for a session or two before relying on the cutover for
unattended use.

## 4. Divergence handling

A divergence from step 1 — your existing definition and the plugin's
shipped definition share an id but differ in content on purpose — has two
resolutions, and picking between them is a per-divergence judgment call,
not a default:

- **Fold it into a user-owned override.** Per the shadowing and precedence
  rules in the design, the user layer (global config or a trusted ancestor
  overlay) always wins over plugin layers. For workspace providers, resources,
  tasks, and channels this means placing a full same-id definition
  in global config or an overlay — there is no partial-field patch for these
  kinds. For workflows, a same-named
  workflow file in an overlay can add new `[[nodes]]` or
  `[[event.channel]]` entries without redeclaring the plugin's singleton
  fields. This is the right choice when the divergence is genuinely local:
  a team convention, a credential path, an internal service the plugin has
  no way to know about.
- **Upstream it.** If the divergence is a fix or an improvement that other
  consumers of the same catalog would also want, contribute it back to the
  catalog as a change to the plugin's shipped definition, then delete your
  local override once the catalog is updated (`plect catalog update`/
  `plect plugin update`) and re-verify. This is the right choice when the
  divergence isn't actually local — it just hadn't been shipped yet.

When in doubt, start with the override (it is reversible and takes effect
immediately); an override that turns out to match what everyone wants is a
prompt to upstream it later, not a decision to make once and never revisit.

## 5. Rollback

Restoring the pre-cutover config home from the backup restores pre-cutover
behavior exactly — plugin directories are mounted read-only and never write
back into user config, so nothing outside the config home needs to be
undone:

```bash
rm -rf ~/.config/plect
cp -r ~/.config/plect.pre-plugins-backup ~/.config/plect
```

No plect process should be running against the config home while the
rollback is applied, since a concurrent write to `catalogs.toml` or
`plect.lock` could otherwise interleave with the copy. There is nothing
else to reverse: catalog additions only ever mount read-only content
alongside the pre-existing config, they never rewrite or delete anything
outside the files this guide already touches in the cutover step.
