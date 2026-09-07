---
supersedes:
  - 2026-09-07-resource-and-workspace-effects
  - 2026-09-07-invocation-project-context
---

# Minimum cleanup contract for execution records

## Context

Execution records distinguish an allocation from the editable workflow that
would create one today. The prior resource-effect and project-context decisions
correctly require retained release obligations, recorded dependency order, and
configuration selected from a trusted project root. Their cleanup wording,
however, describes retaining cleanup declarations, plugin references, and
executables. That would make `storage.db` a source of executable code and
would permit an older allocation to run code that is no longer current trusted
configuration.

The SQLite execution-identity work intentionally removed retained cleanup
code. It leaves the minimum evidence needed to decide whether current trusted
configuration is the same cleanup contract as the one accepted during setup.
An operator also needs distinct outcomes for a successful cleanup, an external
release assertion, and deliberate loss of a record.

## Decision

The resource-entry and ordinary-effect decisions remain in force, with this
contract defining their cleanup and reconstruction semantics.

### Trust and retained evidence

Cleanup executable code is resolved only from the session's current trusted
configuration tree. `storage.db` retains no shell body, command, executable
artifact, or replayable cleanup declaration. It is not a security boundary and
does not establish provenance for code.

At setup, each execution records the acquired resource identity, its execution
generation, its release obligation, the setup directory, and the setup-time
facts that its cleanup bindings need. It also records one cleanup-contract
digest and one plugin content revision for every nested cleanup layer. These
are comparison evidence, not executable artifacts. The layer position and
retained execution declaration identify the corresponding current layer; a
missing, reordered, or different layer makes cleanup unavailable.

The digest is SHA-256 over a canonical UTF-8 JSON object. Object keys are
sorted recursively, arrays retain their declared order, strings retain their
exact values, and the encoded object has no insignificant whitespace. The
object contains the layer position, action type, and exactly one action body:

- a shell action's literal script body;
- an exec action's resolved command path or plugin executable path and its
  declared argument vector;
- the complete cleanup binding table, including the canonical value expression
  for every key; and
- the containing plugin's `plect.lock` content hash when the layer or its
  executable comes from a locked plugin, or `null` for user-owned content.

The setup record stores the same plugin content hash separately so a diagnostic
can identify whether an otherwise matching definition is unavailable because
the lock resolution changed. An editable plugin has no lock content hash and
therefore cannot satisfy this cleanup contract for an acquired allocation.

Before cleanup, plect resolves every required layer from the current trusted
tree, computes the same digest, and compares both the digest and recorded
plugin content hash. A missing definition, unreadable tree, changed digest,
changed or absent locked plugin content, missing setup directory, missing
required setup fact, unavailable credential or environment value, or missing
reliable target identity makes cleanup unavailable. It leaves the obligation
outstanding and records a non-secret reason. It never substitutes a newer
definition, a caller directory, or a different layer.

The cleanup invocation combines current trusted executable code with retained
setup-time facts. Current invocation `force` and validated plugin-owned cleanup
inputs are supplied only at teardown; they are neither setup facts nor digest
inputs. A credential, environment value, or target identity that is necessary
to run the current cleanup is required at teardown, but it is retained only
when a concrete cleanup binding needs a setup-time value. Inspection, Web,
MCP, diagnostics, audit events, and backups do not expose retained sensitive
values merely because they are cleanup evidence.

This comparison covers the declared action, bindings, resolved command path
and arguments, and locked plugin content. It does not promise integrity or
availability of commands found through `PATH`, a script's transitive executable
dependencies, or arbitrary external state. Effects and adapters remain
responsible for the resource-specific ownership evidence required before a
destructive action; a directory that happens to exist never proves ownership of
what is in it.

### Release, failure, and reconstruction

An execution's release obligation has one of these outcomes: outstanding,
released by successful cleanup, released by an external-release acknowledgement,
or discarded. Unavailable cleanup and a failed cleanup leave the outcome
outstanding; the former records why cleanup cannot safely start, while the
latter records a completed failed attempt and may be retried.

Teardown uses the execution-owned dependency edges, with dependents before
their prerequisites. It attempts every released-order item whose unreleased
dependents are already resolved. An unavailable or failed dependent prevents
release of its prerequisites, because those prerequisites may still be needed
to release it. It does not prevent teardown of independent branches. The
result reports every attempted, unavailable, and dependency-blocked execution
without treating any of them as released.

Ordinary `up` may retry cleanup for an existing allocation. It cannot recreate
that execution or a prerequisite needed by its retained release plan until all
of the allocation's applicable obligations are resolved. Once they are,
reconstruction uses the latest desired workflow and creates a new execution
generation. `plect up --force-recreate` requests this normal release-then-new-
generation path when a desired change requires reconstruction; it still refuses
to reconstruct while cleanup is failed or unavailable. It neither acknowledges
external release nor discards records.

`plect destroy --force` is deliberately different. It records a tombstone and
audit event, warns that release was not verified, then discards the session and
every remaining execution record. It is an operator-directed record discard,
not successful cleanup or an acknowledgement of external release. A later
resource with the same apparent identity is a new allocation and is never
treated as released by that discard.

The proposed acknowledgement surface, for owner approval before implementation,
is `plect execution acknowledge-release --session <name> --execution <ulid>
--reason <text>`. The corresponding MCP operation takes the same session,
execution, and reason fields. It is authorized by the same local CLI or MCP
authority that can issue teardown; an actor label is audit attribution, not a
separate human authorization boundary. The command atomically verifies that
the exact execution in that session still has an outstanding obligation,
records its externally-released outcome, and appends
`plect.execution.external_release_acknowledged` with the session id, execution
id, reason, authority label when available, and timestamp. Retrying after that
same acknowledgement is idempotent and returns the recorded outcome. A retry
that finds successful cleanup, a discarded record, another execution, or no
outstanding obligation fails rather than clearing anything else. Concurrent
cleanup and acknowledgement serialize on that execution's release transition.

### Migration

The migration stops resident processes, then backs up every configuration tree
and the durable data directory. An unreleased legacy allocation should be
released with the pre-upgrade binary before migration whenever possible.

No migration backfills today's configuration, cleanup digest, plugin content
revision, setup directory, or setup-time binding fact as though it had existed
at setup. An unreleased legacy execution lacking any required evidence migrates
as an outstanding obligation with cleanup unavailable and the reason
`historical cleanup evidence is unavailable`. It remains inspectable but cannot
be cleaned up or reconstructed by the new release. The operator either releases
it before upgrade, waits for the approved acknowledgement operation after an
external release, or explicitly discards it with `plect destroy --force`.

## Consequences

The contract refuses more cleanup attempts than a retained-code replay design:
a configuration change, missing lock content, or unavailable setup fact can
leave an obligation unresolved. That is intentional. Running newer code against
an older allocation is less safe than preserving an inspectable failure.

Execution records gain concrete per-layer evidence and release outcomes, while
the current trusted tree remains the only executable authority. The next
implementation work must add validation and user-visible unavailable reasons,
then implement the approved acknowledgement surface. It does not add an
artifact store, garbage collector, dependency-closure retention, generic replay,
signing, secret-management system, or isolation between actors sharing the same
operating-system authority.

## Alternatives considered

### Retain cleanup code in the database

Rejected. It makes mutable local state executable authority and turns a digest
into an insufficient substitute for configuration trust.

### Treat a changed digest as permission to run current cleanup

Rejected. A digest mismatch detects that the setup-time contract cannot be
shown to match; it does not authorize a newer cleanup definition.

### Let force-recreate discard unreleased allocations

Rejected. Reconstruction needs a confirmed release boundary. `destroy --force`
is the explicit, auditable record-discard operation for the exceptional case.
