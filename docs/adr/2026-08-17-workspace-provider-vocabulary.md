---
superseded_by: 2026-09-07-resource-and-workspace-effects
---

# Workspace provider vocabulary

## Context

The core term `provider` no longer names the responsibility it carries.
The configuration object called a provider answers one narrow question:
given a session resource, what session id does it map to, and how is the
session workspace acquired, subscribed, and released?

The repository already separates that question from resource observation.
`resources/*.toml` defines how a resource is observed and finalized.
`providers/*.toml` defines optional resource-to-session-name resolution,
workspace setup and cleanup, optional subscription, and the setup output
contract. That split is durable, but the bare noun `provider` hides it and
also looks like a daemon or external service role.

The vocabulary appears in these surfaces:

- config paths: global `providers/` and plugin `config/providers/`;
- workflow config: `provider = "<id>"`;
- app Go identifiers and JSON: `ProviderConfig`, `LoadProviders`,
  `WorkflowFile.Provider`, `WorkflowDetail.Provider`, `provider_info`,
  `app/internal/config/provider.go`, and related service/task plumbing;
- contracts Go prose: `contracts/state` uses provider in the workspace-provider
  sense when describing setup outputs, while `contracts/event` and
  `contracts/state` also use provider in the external-integration sense for
  opaque resource, chat, and source values;
- CLI and user-facing text: workflow detail output, root/status/destroy/template
  help, and load or dispatch errors;
- shipped plugin content: `plugins/github/config/providers/github.toml`,
  `plugins/okf/config/providers/local-okf.toml`, plugin README files, plugin
  metadata, `plugins/catalog.toml`, and executable names such as
  `plect-github-provider`;
- boundary checks: `scripts/check-provider-boundary.sh`,
  `scripts/check-provider-boundary_selftest.sh`, and the `provider-boundary`
  CI job;
- design and migration docs: plugin packaging, plugin config layout migration,
  workdir vocabulary migration, and repository-adjacent wiki prose that explains
  this concept through workspace language.

The binary-naming discussion exposed the issue. Core concept names, config
directory names, and shipped plugin executable names live at different layers.
A binary that implements a single plugin capability should not use an `-er`
noun that reads like a resident daemon, but that rule applies to executable
names, not to concept names. The bare noun `provider` was ambiguous; the
compound `workspace provider` is specific to this contract.

The same discussion ratified that plugin executables do not use the `plect-`
prefix. Plugin ids already qualify executable references and service logs, so
the prefix is reserved for host-installed binaries such as `plect` and
`plect-web`.

The term `workspace` has project history. It was used for older runner and OKF
phrasing that did not crisply separate the abstract place where session work
happens from the filesystem directory used by the filesystem implementation.
Reusing the word is acceptable only if this decision pins the new sense and the
migration disambiguates the legacy sense.

## Decision

This decision sets a three-layer naming principle:

- the core concept is named by its responsibility;
- config directories use short plural nouns that name the declaration kind's
  slot in the config tree;
- shipped plugin executables are named for the plugin's concrete realization of
  a core capability, not by mechanically copying the core concept name.

Under that principle, the core concept is named **workspace provider**.

A workspace provider is a trusted configuration declaration that may resolve a
resource id to a session id, acquires the session workspace, releases what it
acquires, may bind an existing session to another resource, and declares the
setup outputs available to workflows.

In this decision, a workspace is the session's acquired work surface. It is not
synonymous with a filesystem directory, a runner process, an executor, or an
environment. The filesystem implementation represents a workspace with a
directory path, and path-bearing names carry the `_dir` or `_dir_path` suffix
to keep that concrete detail out of the concept name.

The code-facing vocabulary for the breaking implementation is:

- config directory: `workspaces/`;
- workflow field: `workspace_provider = "<id>"`;
- Go type and loader: `WorkspaceProviderConfig` and
  `LoadWorkspaceProviders`;
- workflow detail JSON: `workspace_provider` and `workspace_provider_info`;
- CLI noun, when a direct inspection command exists: `workspace-provider`;
- shipped single-purpose executable names: plugin-specific concrete capability
  names such as `github-worktree`;
- setup output path key: `workspace_dir`;
- path-valued state or root fields: `workspace_dir_path`,
  `workspace_dirs_root`, and matching Go/template names.

`resource definition` remains the paired concept. A resource definition
observes a resource's state. A workspace provider turns a session resource into
a workspace lifecycle.

This decision removes bare `provider` as the name of the workspace lifecycle
contract, not the provider noun itself. `Provide` names the responsibility's
center of gravity: the declaration makes a workspace available, and release and
subscription are obligations of the same providing party. It does not ban
`provider` as generic English for an external integration when the prose is not
naming a core config kind, API field, command, or Go identifier. The
provider-boundary checker keeps that term because it guards against core naming
a specific external integration provider.

The concept name does not mechanically become the executable name. The GitHub
workspace provider's concrete workspace realization is a git worktree, so its
single-purpose executable is `github-worktree`, not `github-workspace-provider`
or `github-workspace`. The same layer rule keeps GitHub resource-observation
executables on concrete resource names such as issue and pull request, not the
abstract `resource definition` concept.

## Consequences

This is a breaking vocabulary change. Plecture is pre-1.0, so the
implementation uses a one-time migration rather than compatibility aliases
that accept both vocabularies.

Core becomes easier to review for boundary leaks: the durable core term names
the workspace lifecycle, while plugin-specific integrations remain in plugins.
The existing rule that core must not name any specific integration stays
unchanged.

Implementation touches multiple packages and shipped plugin config. It must
update docs, help text, tests, schema-facing JSON names, and migration
instructions in the same PR that changes code.

The implementation migration shape is:

- rename `providers/` directories to `workspaces/` in global config and plugin
  `config/` trees;
- rename workflow `provider` fields to `workspace_provider`;
- rename setup output `workdir` to `workspace_dir`;
- rename path-bearing state/config/template fields from `workdir` to
  `workspace_dir` or `workspace_dir_path` according to the field's existing
  shape;
- rename Go identifiers, JSON fields, command output labels, help text, and
  error strings that expose the concept;
- update contracts prose that uses `provider` for the workspace provider
  contract;
- audit contracts prose that uses `provider` for external integrations and keep
  only provider-neutral phrasing;
- audit workspace prose that predates this definition and either align it to
  the acquired-work-surface sense or rename it to the narrower concept it means;
- audit `scripts/check-provider-boundary.sh`, its selftest, and the
  `provider-boundary` CI job; under this vocabulary they keep their names
  because they guard against specific external integration leakage;
- rename shipped plugin references and binaries that implement only the
  workspace provider contract to concrete realization names such as
  `github-worktree`;
- update docs and repository-adjacent wiki prose that describes the concept;
- supersede `docs/migrations/workdir-vocabulary-migration.md` for this breaking
  release with a new one-time procedure that accepts both worktree-era and
  workdir-era names and writes only the workspace vocabulary, so operators do
  not chain two state rewrites;
- add a one-time migration procedure under `docs/migrations/` with a backup
  step.

The migration does not add dual-read compatibility. A post-migration plect
binary reads the new vocabulary only.

The implementation PR carries behavior tests for the config loader, workflow
loader, workflow display, lifecycle setup/cleanup, subscription, shipped plugin
config, and legacy migration path.

One-time survey evidence belongs in the PR body. It should include exact
commands used to confirm that no core/user-facing bare `provider` vocabulary
names the workspace provider contract. Historical ADR context, migration
instructions, provider-neutral boundary tooling, and generic
external-integration prose may keep the word when it does not define a config
kind, API field, command, or Go identifier.

The same evidence should confirm that workspace prose in core docs, code,
tests, and shipped plugins uses the acquired-work-surface sense from the design.
Any remaining workspace wording with runner, environment, or domain-bundle
meaning must be renamed to that narrower concept.

Future executor work can add workspace representations that are not only local
directories. Existing path contracts stay explicit because their names carry
`_dir` or `_dir_path`.

The migration cost is real: persisted state fields, a config key, and template
variables move again after the workdir vocabulary migration. Folding that cost
into this breaking change is cheaper than keeping a filesystem-specific concept
name and paying another rename when non-directory workspaces arrive.

Using `workspace provider` keeps an `-er` noun in the concept layer. That is
acceptable because the compound names the full lifecycle responsibility, while
the executable naming layer uses plugin-specific concrete realizations and
keeps shipped binaries out of the resident-daemon shape.

## Alternatives considered

### Keep bare `provider`

Keeping the term avoids a breaking rename, but it leaves the same ambiguity
that caused the issue. It does not say what is provided, and it encourages
phrases such as `resource provider` that blur the boundary between workspace
lifecycle and resource observation.

### Workspace definition

`workspace definition` pairs neatly with `resource definition`, and it can use
the same short config directory form, `workspaces/`. It loses because it implies
a static declaration of a workspace rather than the full active contract. The
concept does not merely define a place; it may match resource ids, derive
session ids, run setup, run cleanup, subscribe sessions, and declare output
schemas.

`workspace definition` also does not remove the binary-naming problem by
itself. `github-workspace-definition` is awkward as an executable name, so it
would still need the same rule that executable names use a short capability
noun instead of mechanically copying the concept name.

### Workspace source

`workspace source` names where a workspace comes from, but it underweights the
parts of the contract that are not sourcing: cleanup, subscription, and the
declared setup output schema. It is clearer than bare `provider`, but it is too
close to `workdir acquisition`, which was rejected for dropping release and
subscription responsibilities.

### Bare source

`source` avoids the overloaded provider word, but it is too generic for core
vocabulary. It can mean an event source, a resource source, a data source, or a
configuration source, and it says nothing about the workspace lifecycle.

### Workdir source

`workdir source` names the concrete directory produced by filesystem setup
hooks, but it makes the concept a filesystem implementation detail while
nearby core concepts are role words: resource, environment, session, workflow,
and task. It is also too narrow for future executors that acquire a container,
VM, or other bounded work surface while still exposing a directory path to
existing tools.

### Workdir provider

`workdir provider` covers more of the lifecycle than `workdir source`, but it
still names the produced filesystem directory rather than the abstract
workspace. It would make the concept too narrow for future non-directory
workspace representations.

### Workspace provider with `workspace_providers/`

The directory name `workspace_providers/` carries the full concept name, but it
breaks the existing config-directory convention of short plural nouns.
`resources/` remains the directory for resource definitions, so the symmetric
directory for workspace provider declarations is `workspaces/`.

### Mechanical concept-to-binary naming

Naming every single-purpose plugin executable after its full concept would
produce `github-workspace-provider`. That keeps the `-er` binary shape that the
binary-naming convention reserves away from non-resident executables. It loses
because executable names need to identify the plugin's concrete realization
rather than repeat the abstract core concept. The shortened abstract form
`github-workspace` has the same problem without the `-er` suffix: GitHub does
not implement workspace in general, it implements the workspace capability with
git worktrees.

### Provider/resource concept-split mains

Naming the workspace executable main after the old provider side of the
provider/resource concept split would make binary names mirror stale
vocabulary. Resource-observation executables should also follow the concrete
realization layer and use the plugin's resource shape, not a generic
`resource-definition` binary name. The rejection applies to carrying bare
`provider` forward for workspace lifecycle executables.

### Retain `plect-` for plugin executables

Keeping names such as `plect-github-worktree` would preserve the old host-binary
prefix inside plugin packages. It loses because plugin executable references
are already qualified by plugin id, and `plect-` is reserved for host-installed
binaries.

### CLI as the executable noun

`cli` is rejected for binaries or packages that implement this contract because
it names an interface style, not a responsibility. It creates no single
responsibility pressure: any command-line surface can be called a CLI, even
when it mixes unrelated contract work.

### Workspace as generic runner language

Reusing workspace without a pinned sense would revive the ambiguity from older
runner and OKF prose. It loses because the concept needs to mean the acquired
session work surface specifically, with directory paths named by `_dir` fields.
