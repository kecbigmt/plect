---
supersedes:
  - 2026-09-07-resource-and-workspace-effects
  - 2026-09-07-invocation-project-context
---

# Lifecycle-configuration change notification and cleanup contract

## Context

Workflows are editable trusted configuration. A changed lifecycle definition can
intentionally repair cleanup for an allocation created by an earlier revision,
so refusing current cleanup merely because configuration changed prevents a
legitimate release. At the same time, an operator needs notice that the
configuration about to execute differs from the one used previously.

Execution records answer a different question: what allocation exists, what
facts are needed to clean it up, and which allocations must be released first.
They must not become a source of executable cleanup code. The previous
per-layer cleanup-digest and plugin-content-pin proposal conflated notification
with execution authorization and added retention solely to refuse an edited
cleanup.

## Decision

### Lifecycle-configuration change notification

Before `plect up`, `plect down`, or `plect destroy` begins execution, plect
loads the current trusted configuration and computes one session-wide lifecycle
configuration digest. If it differs from the session's prior baseline, plect
warns before execution and then executes the current trusted configuration. A
change notification neither adds a confirmation prompt nor requires
`--force-recreate`; a digest mismatch never makes cleanup unavailable.

The digest is SHA-256 over canonical UTF-8 JSON for an explicit projection of
the parsed lifecycle configuration. Object keys are sorted recursively, arrays
retain declared order, and the encoded object has no insignificant whitespace.
For every configured node, the projection includes its composition, effect
reference, scope, dependency structure, cwd selection, setup/cleanup/liveness
action declarations, and input/environment binding expressions. It includes
the referenced effect declarations needed to form that lifecycle configuration,
including nested effects. Action declarations include their declared type,
literal shell source or executable reference, and declared argument vector.

The projection excludes display and description prose, instruction bodies as
such, external-file contents, plugin content hashes, executable binaries,
`PATH` resolution, and transitive executable dependencies. A changed reference
is still a configuration change. If ordinary compared input data happens to
contain embedded instruction text, the resulting extra warning is acceptable;
the projection does not inspect values semantically to suppress it. Editable
and locked plugins use the same declaration comparison. This is lifecycle-
configuration change notification, not tamper detection, provenance
verification, or a security boundary against an actor able to modify both
configuration and the stored digest.

The session retains one nullable baseline shared by `up`, `down`, and
`destroy`. After all execution preconditions pass, plect warns when appropriate
and records the current digest when the lifecycle executor starts. An execution
failure does not erase that fact. A first execution, including the first
execution of a migrated session with no baseline, records the baseline without
a warning. Read-only operations and configuration inspection do not change it.
The digest and configuration loaded for comparison are the same configuration
used by that operation.

### Cleanup and execution records

Cleanup executable code is always resolved from the current trusted
configuration tree, never replayed from `storage.db`. An execution record
retains operational facts: allocation and execution identity, retained
declaration identity, setup inputs and outputs, nested-layer facts and
environment, setup cwd, dependency edges, release state, and failure
information. Updating the notification baseline never rewrites an older
allocation's identity, facts, or release obligation.

Missing required definitions, valid project trust, recorded cwd, or cleanup
inputs are execution-precondition failures. They leave the obligation
outstanding and do not cause cwd fallback, implicit release, or reconstruction.
They are distinct from a lifecycle-configuration change notification. When
cleanup starts and actually fails, its obligation also remains outstanding; its
unresolved dependents prevent release and reconstruction of prerequisites that
they need, while independent release branches continue. The configuration
author is responsible for whether current cleanup suits an existing allocation.

`plect up --force-recreate` retains its release-then-new-generation behavior:
it cannot reconstruct an allocation whose required release failed. `plect
destroy --force` remains the separate, explicit record-discard operation. It
records that release was not verified before removing remaining execution
records; it is neither successful cleanup nor an external-release
acknowledgement.

### External-release acknowledgement

The proposed command, still subject to owner approval before implementation, is
`plect execution acknowledge-release --session <name> --execution <ulid>
--reason <text>`. Its MCP operation carries the same fields. The same local
CLI or MCP authority that issues teardown authorizes the assertion; an actor
label is audit attribution, not a separate human authorization boundary.

The command atomically verifies that the exact execution in that session has an
outstanding obligation, records the external-release outcome, and appends
`plect.execution.external_release_acknowledged` with the session id, execution
id, reason, authority label when available, and timestamp. A retry against an
execution that still exists and is already externally acknowledged returns that
recorded outcome without another transition or audit event. A removed execution
returns not found; audit history is not a command-result authority. Successful
cleanup, discard, another execution, and any other release outcome are never
relabeled as an external acknowledgement. Cleanup and acknowledgement serialize
on the execution's release transition.

### Migration

The migration stops resident processes and backs up every configuration tree
and the durable data directory. It initializes the notification baseline when a
legacy session next begins lifecycle execution; an absent historical digest or
plugin hash alone does not make the execution uncleanable and produces no first-
execution warning.

Migration preserves concrete execution facts already recorded. It does not
manufacture a missing declaration identity, cwd, input, output, nested-layer
fact, trust decision, or allocation identity from today's workflow. A legacy
retained declaration using a retired spelling, such as the spellings addressed
by #541, has the named outcome `legacy declaration spelling is unresolved` when
it no longer resolves from current trusted configuration. It remains an
outstanding precondition failure until the allocation is released externally
and acknowledged or the operator uses `plect destroy --force`; migration does
not infer a replacement spelling from current workflow declarations.

## Consequences

Lifecycle configuration changes are visible before they execute without
blocking a configuration repair. The design stores one compact notification
baseline per session, not declaration snapshots, retained comparison graphs,
per-field digests, detailed differences, or configuration history.

Execution records retain the concrete facts needed for cleanup and generation
correctness but not executable cleanup code, per-layer refusal digests, plugin
content pins, artifact retention, or a new secret-management system. Existing
runtime values may be sensitive; warnings and inspection surfaces do not dump
them merely because they are operational facts.

The design adds no automatic restoration, artifact store or garbage collector,
dependency-closure retention, generic replay, signing, or isolation between
actors sharing operating-system authority.

## Alternatives considered

### Refuse cleanup when lifecycle configuration changes

Rejected. It prevents a trusted configuration edit from repairing cleanup for
an existing allocation and mistakes a notification for an authorization gate.

### Compare plugin content hashes and executable files

Rejected. The required consumer is a lifecycle-configuration change warning,
not comprehensive tamper detection. Declaration references provide the useful
change signal without expanding into external-file or binary hashing.

### Preserve separate baselines by lifecycle operation

Rejected. One session-wide baseline tells the operator whether the next
lifecycle execution uses different configuration; separate operation histories
would warn repeatedly without a consumer.
