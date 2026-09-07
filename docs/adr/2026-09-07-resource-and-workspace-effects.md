---
supersedes: 2026-08-17-workspace-provider-vocabulary
---

# Resource entry points and ordinary environment effects

## Context

`workspace_provider` makes one declaration answer resource identity, delivery,
directory acquisition, and exceptional pre-plan lifecycle questions. Its
reserved `workspace_dir` output is persisted through an `@workflow` pseudo-node.
`resource_observer` separately recognizes and observes the same external
things. The split gives two authorities for an identifier and makes a
filesystem directory look like the definition of a session environment.

The GitHub issue and pull-request workflows, a Slack conversation workflow,
and the local goal workflow show the required shape. They need resource
identity and delivery, ordinary setup/cleanup effects, an agent's work
directory when applicable, public outputs consumed by display and delivery,
and tasks which may concern a resource other than the session's entry resource.

## Decision

`workspace_provider` and `resource_observer` are replaced by `kind =
"resource"`. A resource recognizes an identifier (`match`), derives its
resource-instance name (`name`), observes it, queries it, binds delivery, and
finalizes it. Resource action inputs are supplied at each concrete operation:
an entry invocation, an additional task-resource binding, and a standalone
resource operation each provide an object satisfying that resource's declared
`inputs_schema` (or the empty object when none is declared). Query inputs are
a separate object satisfying the query contract. They do not inherit from one
another.

A workflow declares exactly one entry resource type. `plect up` receives a
concrete entry-resource identifier and an optional workflow. With no workflow,
resolution succeeds only when exactly one workflow accepts the identifier's
resolved resource type. A workflow constrains session creation, not the types
of tasks the resulting session may carry.

The resource instance name is not a session name. Core constructs the session
name from that name plus an optional caller-supplied tag. No tag uses the
resource instance name unchanged; a tag is non-empty, validated as one session
name segment, and the resulting name must be unused unless it resolves to the
same entry resource and tag. The recorded entry resource, tag, and selected
project root are the dispatch and storage authority; adding a task does not
rename or retarget the session.

Effects assemble an environment under their ordinary dependency graph. An
environment can include a conversation, a terminal, credentials, and a
directory; it need not include a directory. A session-scoped effect therefore
acquires a worktree or Slack-thread directory, using ordinary setup, liveness,
invalidation, and cleanup rules. There is no reserved output, session column,
or `@workflow` node.

A workflow may declare one `workdir` projection from a node output. It must
resolve to an absolute local filesystem directory path; an environment without
such a path omits `workdir`. Its
producer and that producer's transitive prerequisites are preparation nodes;
the set is derived before default-workdir edges are added. Every other node
executes its setup and liveness actions in the declared directory and depends
on its producer. Preparation setup, liveness, and cleanup actions use a
recorded preparation directory: a direct invocation records its caller
directory, a chain inherits its triggering session's value, and a population
inherits its resident's value. This gives setup no unstated dependency on a
directory it is creating or a daemon's cwd. A workflow without
`workdir` has no default directory. Cleanup uses the directory selected for
that node's setup; if it has disappeared, cleanup fails and records that fact.
A probe launch failure there invalidates the node, attempts cleanup in the
stored directory, and retains the failed record, allocation, and cleanup
obligation if cleanup also cannot launch. A later explicit `up` may retry that
stored cleanup, but may not reconstruct the node or its prerequisites until
release of the old allocation is explicitly confirmed. Successful recorded
cleanup is the normal confirmation.
If cleanup cannot run, automatic reconstruction stops and reports operator
recovery required while the record, cleanup information, and failure reason
remain inspectable. After external release, an explicit operator-confirmation
operation records the operator's assertion rather than a successful cleanup,
with the execution identity, who, what, when, and an audit event. It resolves
only that allocation's obligation. Old allocations are released in the retained
plan's release order. After all applicable obligations are explicitly resolved,
ordinary `up` reconstructs in the latest desired workflow's setup dependency
order. `--force-recreate` does not acknowledge an obligation implicitly.
A missing directory does not prove that a process or
external allocation is gone. Recovery never falls back to the invocation
directory or another node's directory. There are no per-node or per-action cwd
overrides.

`[workflow.outputs]` and `outputs_schema` are the public projection record.
They bind declared values when their source node outputs exist and are persisted
with the session. A consumer of a public output depends on its underlying
producer nodes; those expanded dependencies join cycle detection and
preparation-graph derivation before default-workdir edges. `workflow.outputs.*`
remains the only workflow-output root for display, task instructions, downstream
node bindings, and channel delivery; it is not an effect lifecycle and does not
create a pseudo-node.

The initiating caller selects the optional initial task. `plect up`, a chain,
and a population each select at most one compatible task; omission creates an
environment for a human. The task binds one concrete resource and its declared
resource type must match that resource. It may be a different type from the
entry resource. Completion reads its resource observation, task state, and
judge evidence; it does not require mutation of the resource. `observe`
refreshes Plecture's knowledge and does not advance the external thing.

Chains may start another session on their triggering condition, addressing the
same resource or a newly observed one such as a pull request. Population
queries derive their resource type from their containing workflow and manage
the desired sessions under declared retention and removal policy. A Slack
conversation session can consequently retain its directory and conversation
while it gains an issue-investigation task.

## Database impact map

This map states the persistence invariants the implementation must satisfy. It
does not prescribe tables, columns, or migration SQL.

### Execution identity and ownership

A logical workflow node and each of its setup attempts have distinct identity.
A new attempt never overwrites an unreleased attempt's release recipe. The
execution record owns its nested layers, cleanup contract, inputs, outputs,
directory, and resolved plugin references, so a changed nesting declaration
cannot replace an old allocation's cleanup information.

The retained execution plan also owns the dependencies and lifetime information
for allocations, including default-workdir edges. Release follows those
recorded dependencies, not the latest desired workflow: an old agent cleans up
before the old checkout on which it depended is released. The current operation
supplies `force` and cleanup inputs, validated against the retained cleanup
contract. Crash recovery preserves partial attempts and treats an execution
identity as a record identity, not an exactly-once guarantee for external
effects. Retention is bounded to records needed for unreleased allocations. A
failed cleanup retains its obligation; reconstruction waits for successful
recorded cleanup or the explicit, audited operator assertion of external
release, as specified above. Execution identity and ownership are implemented
by [#496](https://github.com/kecbigmt/plecture/issues/496).

### Session entry identity

A session records its entry resource type, concrete identifier, derived
instance name, and optional tag. `sessions.id` remains incarnation identity,
not the entry identity. Live session names are unique, but finding a live name
also requires compatibility with its recorded entry resource and tag; name
equality alone is insufficient.

### Configuration selection

The selected project root and preparation directory are durable session facts.
The digest is a comparison baseline for change detection and reporting, kept
separate from execution records. It is neither a foreign key nor an operation
blocker when desired configuration changes.

### Workflow public outputs

The declaration-owned public projection is stored as session-owned JSON. It is
refreshed or invalidated when a source execution changes. An execution's
recorded directory is a historical fact, not a current output or workspace
authority.

### Resource bindings

Entry-binding inputs and additional task-resource binding inputs are stored
separately from task and workflow inputs. A global resources table is not
introduced by default: sharing one would require explicit ownership rules for
context, credentials, and observations.

### Population identity across project contexts

The existing workflow-address-plus-name identity can collide when two selected
project roots each define a local workflow and population with the same names.
Before implementation, either population identity and session provenance must
include a stable configuration-selection context, or one runtime data store
must deliberately reject simultaneous colliding contexts. A configuration
digest is not suitable for this identity because an editable policy must retain
ownership through a digest change.

## Consequences

The new dialect specializes workflows by entry resource type. This duplicates
otherwise similar issue, pull-request, and conversation wiring, and moving a
workflow to another entry type requires a new workflow plus migration of its
population and callers. That explicit cost is preferable to pretending every
task in one environment has one resource type.

The migration is one breaking change after the SQLite cutover: back up each
configuration tree and durable state; rewrite `workspace_provider` and
`resource_observer` declarations as resources and effects; replace provider
nodes and `workspace.*` bindings; migrate stored pseudo-node/output records to
ordinary nodes and public outputs; and remove obsolete workspace fields. It
has no compatibility interval. The implementation must preserve invocation
cleanup inputs, including `force` and plugin-owned options; creation inputs do
not authorize destructive cleanup.

The implementation also has to define validators, storage migration, resource
operation surfaces, session-name collision diagnostics, public-output
persistence, graph construction, desired-workflow reconciliation, execution
records, vanished-directory cleanup behavior, and task/chain/population
compatibility. Existing runtime tests validate the old dialect, not this
decision. The operational procedure is
[`resource-and-workspace-effects-migration.md`](../migrations/resource-and-workspace-effects-migration.md).

## Alternatives considered

### Retain a workspace provider

Rejected. It keeps a provider-only lifecycle and makes an optional directory a
core identity concern.

### Give every node its own cwd control

Rejected. The reference configurations need only one workflow work directory;
additional overrides add competing order and cleanup semantics without a
consumer.
