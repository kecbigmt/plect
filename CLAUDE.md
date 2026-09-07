# Standing rules for coding agents

Plecture lets individuals and teams compose their own systems for autonomous
work without committing to a particular agent, VCS, execution environment,
workspace technology, or communication tool. Plecture owns the durable
structure of work: identity, lifecycle, relationships, observation,
verification, and handoff.

## Placement: core vs. plugin

Durable structure of work (identity, lifecycle, relationships, observation,
verification, handoff) belongs in `app/` and `contracts/` (core).

A plugin is a distributable package: executable adapters (0+) + config
resources (0+ — workspace providers, resources, tasks, workflows, templates,
channels) + metadata. A plugin with only configuration and no executables
is still a plugin. Commitments to a particular technology (a VCS, an agent
CLI, a terminal multiplexer, a chat service) belong in `plugins/`, whether
expressed as code or as shipped config. (Packaging mechanics — the package
format, lockfile, and reference-resolution model — are covered by a
separate design in progress, not by this section.)

Core must not know any specific provider exists. No provider names in core
identifiers, imports, help text, or error strings. This is a blocking review
dimension.

## Comments

Priority when writing a comment: **why / why-not** first, far ahead of
**what**, further ahead of **how**. The code and the diff already show what
changed and how; a comment earns its place only by explaining why a choice
was made, or why an alternative was not.

- **Default to no comment.** Write one only when the why is genuinely
  non-obvious from the code itself.
- **Keep it to the minimum that states the why-not.** The bar is not a line
  count — a genuinely non-obvious why-not can run several lines — but never
  pad it, and never let it restate what the code already shows.
- Never put issue, PR, or ADR numbers in code comments. A number tied to an
  external tracker rots the moment the comment is relocated or the tracker
  is renumbered. Put a long-form rationale in a `docs/adr/` decision record
  instead, and keep the comment itself a terse, self-contained why-not.
- Every comment must be a self-contained, complete sentence understandable
  without external context.
- Keep explanations in their own layer, and don't duplicate one layer's
  explanation in another: a code comment carries only why-not; `--help`
  text and CLI usage strings speak to the user; a test is the executable
  specification; `docs/adr/` carries the long-form rationale.
- A test function's name should usually make a docstring unnecessary. A
  one-sentence statement of intent is acceptable when the name alone
  doesn't carry it; don't restate the test's steps.
- The same rationale is never written in more than one comment within a
  change; it lives in the single outermost place (the function's doc
  comment or `docs/adr/`), and other sites do not restate or reference it.

Bad — restates what the code does and leans on an external decision
record instead of standing on its own:

```go
// FetchOutput resolves one dynamic output by running its script against
// the source of truth (see the design doc). It returns the value a later
// check compares, and the value a status command displays as current.
func FetchOutput(...) (string, error) { ... }
```

Good — states only the non-obvious why-not, self-contained:

```go
// A non-zero exit is a fetch failure, not an evaluation failure: a check
// built on it reads as pending, not as failed.
func FetchOutput(...) (string, error) { ... }
```

## Language

All prose — comments, docs, identifiers, messages — is English only. The
sole exception is non-English literals used as test data that specifically
exercises multibyte/unicode behavior, labeled as such by an adjacent English
comment.

For which project name to use where (Plecture vs. `plect`), see
[`docs/naming.md`](docs/naming.md); it is the decision record for all
prose naming.

## Repository map and dependencies

| Directory     | Responsibility                                                |
|---------------|----------------------------------------------------------------|
| `app/`        | CLI + MCP server: session lifecycle, task DAG, state, dispatch |
| `contracts/`  | Shared data contracts between the CLI and plugins               |
| `plugins/`    | Distributable packages: executable adapters and config resources for a particular technology (channel relay, GitHub workspace provider and watcher, Slack adapter) |
| `deploy/`     | Deployment packaging: container images and their runtime contract (build, process supervision, persistence, first boot) — not a catalog plugin, not core |

Core (`app/`, `contracts/*`) never imports `plugins/*`.

`app`, each package under `contracts/`, and each package under `plugins/`
are independent Go modules, wired together by the repository's `go.work`
for editor tooling. A bare `go build ./...` from the repository root does
not resolve, because the root itself is not a module. Build and test per
module, as documented in the README:

```bash
go build ./app/...
go test ./app/...
```

and likewise for each module under `contracts/` and `plugins/`.

## Agent configuration

Claude and Codex sessions operate under the same rules and skills, so the
configuration has one source of truth per surface:

- `AGENTS.md` is a symlink to `CLAUDE.md`. Edit `CLAUDE.md`; never edit
  `AGENTS.md` directly or let the two diverge into separate files.
- Skills live in `.claude/skills/<name>/SKILL.md`, with `name:` and
  `description:` frontmatter fields.
- `.agents/skills/<name>` is a relative symlink to
  `../../.claude/skills/<name>`, one per skill, so a Codex session sees the
  same skill set as a Claude session.

`scripts/check-agent-config.sh` (wired into CI) validates these invariants:
the `AGENTS.md` symlink target, and, for every directory under
`.claude/skills/`, the presence of `SKILL.md` with both frontmatter fields
and a matching `.agents/skills/` symlink.

## Development workflow

This section is the source of truth for how changes get made. If the
repository later adopts agent skills or checklists, they must derive from
these rules, not replace them.

### TDD as the default flow

For any behavior change or bug fix, write the failing test first (red),
make it pass with the minimal implementation (green), then refactor. A bug
fix PR must contain a regression test that fails against the pre-fix code
and passes after the fix.

### Refactoring discipline

Refactoring commits are behavior-neutral and must not be mixed with
behavior changes in the same commit. Before refactoring code that lacks
test coverage, add characterization tests first so the existing behavior
is protected before it is moved or restructured. The required order is:
characterization tests, then refactor, then behavior changes as separate,
later commits.

Coverage means error branches and failure paths are tested, not just the
happy path. A change that only exercises success cases is incomplete.

### Audit discipline

- **Two-authorities audit lens.** Every defect in the config-language
  implementation arc reduced to the same shape: two authorities answering
  one question and disagreeing (a stored id vs. the workflow, a plan node
  vs. a state entry, a schema vs. a validator). A pre-PR-ready audit runs
  this lens, and its scope includes code authored during the PR under
  review, not only code that predates it. (Origin: recurring shape across
  the config-language implementation arc's defects.)
- **Wholesale revert warning.** `git checkout <base> -- <file>` restores the
  base branch's version of a file wholesale, including references to things
  the current branch has since deleted or renamed. After such a revert, diff
  the restored file against the branch's own removals before committing it.
  (Origin: a revert during the config-language arc resurrected references to
  identifiers the branch had already deleted.)
- **Locate the absence, don't assert it.** A claim that something "was
  previously unverified" about a mature area of the codebase requires citing
  where the coverage would live (which test, which file) and showing it
  missing there. An assertion of missing coverage without that citation is
  not evidence. (Origin: an unverified-coverage claim during the
  config-language arc's follow-ups turned out to be wrong.)

### Design principles: YAGNI and SOLID

Stated operationally — testable rules an agent can apply and a reviewer
can block on, not name-drops.

- **YAGNI.** Build only what a present, concrete consumer needs. No
  speculative flags, config keys, or extension points. An abstraction
  needs at least two real consumers — in this change, or already in the
  tree (rule of three preferred) — otherwise write the concrete code
  twice first.
- **SOLID, Go-idiomatically.** Single responsibility per package/type,
  statable in one sentence. Small consumer-side interfaces: accept
  interfaces, return structs; define an interface where it is used, not
  where it is implemented. No interface with exactly one implementation
  and one caller.
- Speculative complexity that ships anyway must carry an explicit removal
  condition (a deadline or a metric) — the same discipline this repo's PR
  process already requires when complexity is introduced ahead of proven
  benefit.

### Standing checks vs. one-time verification

Adding a CI check or test is a decision to pay its maintenance cost
forever, not a one-time cost.

- A requirement to verify that something is absent, swept, or consistent
  is satisfied **once**, by evidence in the PR body (the exact command and
  its output) — not by adding a permanent check.
- Before writing a new corpus assertion, search for an existing harness
  first: a measurement showing the implementation already agrees with the
  spec is often evidence a check already exists. In `app/internal/lang`,
  the three corpus harnesses are `TestNativeConformanceFixtures` (what the
  package's own validators do), `TestConformanceFixtures` (what the
  published schema accepts), and `TestCodesMatchDocumentedTable` (the
  diagnostic registry against its documented table) — check these before
  writing a new one. (Origin: a duplicate corpus assertion written during
  the config-language arc.)
- A rename is complete when a computed reference inventory — across code,
  comments, error strings, and docs — returns empty, not when spot checks
  pass. (Origin: a rename during the config-language arc left references
  that spot checks missed.)
- A standing check must guard an invariant that future changes can
  silently break, and its maintenance cost must not scale with legitimate
  edits. Never pin prose literals; never assert exact counts of document
  structure.
- **Behavior tests are the spec, and committing them is the default, not
  the exception.** BDD-style tests that express required behavior (the
  Given/When/Then of the acceptance criteria, including failure paths) are
  an expected deliverable of every behavior change, and their maintenance
  cost is the cost of having a specification — always justified. This rule
  must never be cited to skip or trim them.
- What requires justification is the other category: standing meta-checks
  (lint jobs, structure validators, doc checkers) and over-fitted
  assertions whose maintenance scales with legitimate edits rather than
  with behavior.
- When proposing a new standing check, state in the PR what invariant it
  guards and what would silently break without it. No answer, no check.

This follows Meszaros, *xUnit Test Patterns*, "Goals of Test Automation"
(http://xunitpatterns.com/Goals%20of%20Test%20Automation.html): the
maintenance-commitment framing is the Economics of Test Automation, the
BDD-positive bullet is Tests as Specification, and the
never-scale-with-legitimate-edits prohibition generalizes the Fragile
Test smell.

### Test placement and speed

Prefer table-driven tests where idiomatic to the case being tested.
Per-module tests must stay fast enough to run on every CI matrix job;
push slow or environment-dependent cases behind an explicit opt-in rather
than letting them slow down the default run. Integration-style tests live
next to the module they exercise, not in a separate top-level tree.

### Installability invariant

`go install github.com/kecbigmt/plecture/app/cmd/plect@latest` must keep
working without local replace directives. A change that touches an
inter-module version pin (a `require` line in one module's `go.mod`
pointing at another module in this repository, e.g. `app`'s pin on
`contracts/*`) must verify that the commit embedded in the new pseudo-version
is reachable from `main` before merging — an unreachable commit breaks
resolution for every downstream `go install`.

### Before every push

Run `scripts/check-all.sh` and fix anything it reports before pushing. It
runs `gofmt`, `go vet`, `go test`, and every `scripts/check-*.sh` CI runs,
in the same order and with the same arguments CI uses. A push that fails a
check CI would fail costs a full review round.

### Definition of done

A PR is done when:

- CI is green, and CI is the mechanism that enforces this, not manual
  confirmation.
- New or changed behavior is covered by tests that were written before
  the implementation that satisfies them.
- There is no formatting or vet debt (`gofmt`, `go vet` clean) in any
  module touched by the change.
- The PR body contains `Closes #<issue>` for the issue it resolves. A PR
  that omits this publishes no `pr_url`, and any review chain that depends
  on that value silently never fires. (Origin: observed twice in one day
  during the config-language arc.)

## Compatibility policy

Plecture is pre-1.0. Do not add backward-compatibility code paths. Breaking
changes ship with a one-time migration (script or documented procedure,
including a backup step), not a compatibility shim. The migration
procedure lives in `docs/migrations/`, added or updated in the same PR as
the breaking change — not as follow-up work.

## Design and ADR documentation

- **Decision records** live in `docs/adr/YYYY-MM-DD-<slug>.md` — the date of
  the decision and a kebab-case slug (e.g.
  `docs/adr/2026-08-16-plugin-service-lifecycle.md`). Filename sort is
  chronological, matching the knowledge-bundle retrospective naming already
  in use. Each ADR has the sections Context, Decision, Consequences, and
  Alternatives considered.
- ADRs carry no Proposed or Accepted status. An unmerged PR is the proposal,
  merging is acceptance, and git history records date and author.
- Supersession is the only lifecycle fact recorded in ADR files. A
  superseding ADR declares YAML frontmatter
  `supersedes: <YYYY-MM-DD-slug>`, and the old ADR gets YAML frontmatter
  `superseded_by: <YYYY-MM-DD-slug>`. That frontmatter edit is the only edit
  permitted to an accepted ADR.
- ADR frontmatter is present only when metadata exists, matching the
  knowledge-bundle documents' existing practice. Do not add empty frontmatter
  blocks or status keys. The ADR title remains the Markdown H1.
- Implementation progress is never recorded in an ADR; it is derivable from
  code and design documents and otherwise rots.
- **Evolving design documents** (proposals still being worked out, broader
  than a single decision) live in `docs/design/<slug>.md`. Genre is conveyed
  by directory placement, not by a status line. Design documents link the ADRs
  they implement or that were produced from them.
- A design document describes how the system is specified to behave:
  present-tense, normative prose, with no change narrative. What changed,
  from what, and why belongs in an ADR's Context, Decision, and Alternatives.
  Phrasing like "currently", "instead of", "previously", or before/after
  framing inside a design document is a smell that the content is ADR
  material.
- Read `docs/design/README.md` before authoring or revising a design document.
- Follow `docs/design/README.md` for configuration-language boundaries:
  configuration is declarative wiring, not computation.
- **The configuration-language reference** lives in `docs/language/`. It is
  the semantic specification: thin, present-tense prose per construct and per
  definition kind. Its companions are `plecture.schema.json` (structural
  shape) and `testdata/config-language/` (the conformance fixtures that are
  the detailed executable specification). A chapter's worked example is a
  fixture quoted verbatim, so prose cannot drift from behavior.
- **Migration procedures** stay in `docs/migrations/` (see Compatibility
  policy above) — they are not ADRs or design documents.
- All of the above is English-only prose; use `docs/naming.md` for project
  naming.
- Design and ADR documents are owner-gated: a design or ADR PR is never
  merged by automation, regardless of CI status.

## Self-containedness

Never introduce references to any specific person's environment, machines,
or other repositories, including in test fixtures and sample data.
