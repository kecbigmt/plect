# Use SQLite with a declarative schema and versioned migrations

## Context

Plecture persists runtime state in JSON and event history in JSONL, with
associated lock, cursor, generation, and tombstone files. CLI commands, the
daemon, and the Web backend access local persistence directly.

There is no measured performance problem or observed consistency failure
motivating this decision. The primary concern is maintaining safe upgrades
as Plecture is distributed to more users and maintained by more contributors.
Extending file-based persistence with coordinated multi-file migrations,
interruption recovery, and version tracking would increase the storage
infrastructure Plecture must maintain itself.

SQLite provides a transactional boundary for schema changes, data changes,
and migration records within one database. It reduces custom persistence
machinery without requiring users to operate a database server. It does not
determine semantic data conversions or compatibility with running binaries.

The desired database structure needs a declarative single source of truth.
That source does not need to be a Go model. Core may use cgo and ship compiled
binaries, while plugin executables remain independently buildable by users.

Relevant precedents illustrate separate aspects of this choice:

- [Nix local storage](https://github.com/NixOS/nix/blob/master/src/libstore/local-store.cc)
  combines SQLite with schema checks and process-level migration exclusion.
- [Dagster storage](https://docs.dagster.io/deployment/oss/oss-instance-configuration)
  and [Prefect server](https://docs.prefect.io/v3/concepts/server) use databases
  for execution state and history; their migration entry points differ.
- [GitHub Desktop](https://github.com/desktop/desktop/blob/development/app/src/lib/databases/repositories-database.ts)
  combines versioned IndexedDB declarations with explicit data conversions.

These are architectural references, not evidence that one tool combination
is universal or that their recovery policies suit Plecture's durable state.

## Decision

### Storage boundary

Use SQLite for core-owned durable runtime state, event records, and related
bookkeeping. Include session relationships, task execution state, population
state, reservations, and event consumption metadata in the migration inventory.
Keep related records in the same local database where they require atomic
updates. Exact tables and transaction boundaries belong in the implementation
design, derived from existing semantics.

Keep user-authored configuration, workspace files, and external resources in
their existing locations. This decision does not change the Plecture language,
introduce event sourcing, or require complete relational normalization.
Use columns and constraints for identities, relationships, and actual query
requirements; retain arbitrary inputs, outputs, and state as JSON where needed.
JSON payload evolution still requires explicit semantic conversion when its
contract changes.

Preserve the local multi-process access model initially. Do not combine this
migration with mandatory daemon ownership of all storage. Store the database
on local disk; remote clients access Plecture through its service interfaces,
not a shared database file over a network filesystem.

### Driver and query access

Use `database/sql` with `github.com/mattn/go-sqlite3`. Accept cgo in core
builds and provide compiled core binaries for supported targets. Source
installation remains supported with the documented C toolchain requirements.

Keep the driver and database implementation inside core persistence packages.
Shared contracts must not acquire this dependency merely to expose state
types. Core's cgo requirement must not propagate into independently built
plugins through shared contracts.

Use sqlc to generate Go query functions and database types from SQL. Keep
generated database types at the persistence boundary and translate to existing
domain types. Do not introduce an ORM or a generic repository framework.
Keep transactions explicit and do not hold write transactions across agent,
network, terminal, or other long-running external operations.

### Declarative authority and migration tooling

Use `schema.sql` as the hand-edited authority for the desired database
structure. Use Apache-2.0 Atlas Community Edition to generate versioned SQL
migrations in goose format. Review generated SQL and add explicit data
conversions where structural differences do not capture intent.

Use pressly/goose as a Go library to execute migration SQL embedded in the
core binary. It is the runtime authority for which migrations have been
applied. Do not also use Atlas to apply migrations to user databases or
maintain a competing applied-version ledger.

```text
schema.sql + existing migration history
    -> Atlas Community Edition -> reviewed SQL migrations -> goose -> SQLite

schema.sql + query SQL -> sqlc -> generated Go -> database/sql -> SQLite driver
```

Commit migration SQL and generated Go code. Applied migrations are immutable;
subsequent changes receive new migrations. Desired structure and historical
transition instructions have distinct roles: a migration can contain a data
conversion that cannot be inferred from the final schema.

Pin tool versions and the Community Edition distribution explicitly. Normal
builds consume committed generated code; users do not need Atlas or sqlc.
The workflow must run without Atlas login, cloud services, or proprietary
features. Do not depend on CE-excluded objects such as views or triggers
without revisiting how their declarations remain under the same authority.

CI verifies reproducible generation and that applying the migration history
produces the declared application schema, excluding tool-owned bookkeeping.
These checks prevent independently edited representations from drifting.
Database structure remains separate from the
[Web API contract](2026-09-06-web-api-schema-contract.md) and configuration
language schemas.

### Upgrade and recovery responsibilities

Coordinate normal database use and migration across processes. A migration
runner must exclude incompatible active readers and writers, recheck the
schema after obtaining exclusion, and prevent new accesses until migration
finishes. A process-local mutex or SQLite's single-writer behavior alone does
not establish application-version compatibility.

Check the supported persistence version before normal access. Reject unknown
newer or unsupported versions with an actionable error. Do not silently
initialize an empty database when existing durable state is unreadable or a
migration fails. Preserve evidence needed for diagnosis and recovery.

Apply each migration and its applied-version record atomically where supported.
Test any operation requiring a different transaction boundary explicitly.
Structural constraints and atomic commits do not validate the business meaning
of a conversion; migration tests must establish that meaning.

The JSON/JSONL-to-SQLite transition is a one-time import with stopped writers,
a coherent backup, validation, and an explicit cutover. Preserve identity,
event ordering, consumer progress, generation/tombstone semantics, and dynamic
values. Translate file-offset cursors deliberately rather than treating them
as database row identifiers. Do not retain permanent dual writes or silently
fall back to the old files after cutover. Account for old binaries that still
understand only the legacy layout.

Follow the repository's pre-1.0 policy: document the supported upgrade path,
backup, and recovery procedure in `docs/migrations/` in the same change that
introduces a breaking transition. Do not add runtime compatibility shims.
Do not promise lossless downgrades; restoring a pre-migration backup discards
subsequent updates unless separately preserved.

Before rollout, validate the pinned toolchain with schema generation, goose
application, and declaration equivalence. Exercise old-data upgrades,
interrupted migrations and restart, concurrent startup/migration exclusion,
unsupported-version rejection, and the initial import. Exact lock mechanics,
the migration command/startup interaction, and table layout require a focused
implementation design; they are not implied by selecting SQLite.

## Consequences

- Plecture gains a standard transactional persistence boundary and reviewed,
  versioned upgrades. The expected benefit is lower maintenance risk, not a
  promised performance improvement.
- Runtime dependencies include the driver and goose. Atlas CE and sqlc add
  development-tool maintenance but are not user installation requirements.
- Core release builds require cgo toolchains. Plugin build independence remains
  an architectural constraint, not an assumption about every plugin's own
  dependencies.
- The initial import has real cost and risk. Preserving current behavior and
  validating old data take precedence over redesigning the domain model.
- SQLite does not remove the need for backup policy, application-level
  compatibility checks, process coordination, or JSON payload migrations.
- Plain SQL declarations, migration files, and committed generated code limit
  dependence on a particular schema planner or query generator.

## Alternatives considered

### Retain JSON/JSONL and build a migration framework

This avoids an initial storage conversion and remains viable for small data.
However, safely coordinating transformations across state, logs, and cursor
files requires machinery that SQLite and migration tooling already provide
within a database. There is no specific file-format requirement that justifies
owning that machinery for core runtime state.

### SQLite with handwritten SQL migrations only

This is a valid smaller toolchain. It does not provide the chosen declarative
view of the desired schema without maintaining and checking another artifact.
Atlas CE supplies that planning step; goose remains the runtime executor.

### GORM or another model-driven ORM

Go models do not need to be the schema authority. SQL keeps constraints,
queries, and transaction behavior explicit. sqlc reduces query boilerplate
without adding ORM behavior. Automatic model synchronization also does not
replace reviewed semantic migrations.

### A cgo-free SQLite driver

This simplifies some build environments. cgo-free core builds are not a
requirement, so choose mattn/go-sqlite3 and handle toolchains in release
packaging rather than selecting primarily on cross-compilation convenience.

### bbolt or a server database

bbolt provides transactions but leaves key layouts, query indexes, and their
evolution largely application-defined. PostgreSQL adds server operation that
the local, single-host scope does not require. Neither is a better fit for
the combination of declarative SQL structure and local deployment.

### Require daemon-only persistence in the same change

A single owner can simplify coordination, but it also changes standalone CLI
availability and invocation semantics. Evaluate that boundary independently
of the storage migration.
