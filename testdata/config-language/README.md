# Config-language conformance fixtures

These fixtures are the executable specification of the Plecture configuration
language. Prose in [`../../docs/language/`](../../docs/language/) carries
meaning; this directory carries exact behavior at valid, invalid, and boundary
cases.

They are deliberately implementation-independent, so the same corpus can serve
the compiler, an editor server, migration tooling, schema-generation
verification, and an alternative implementation.

They are not a migration answer sheet. Every shipped plugin configuration was
translated to this shape once, to verify that each dynamic use maps onto a
class the audit defines; those translations were verification evidence rather
than fixtures, and migration tooling regenerates them against then-current
config when migration happens. The exercise's result is recorded on the audit
issue, and the residues it surfaced are owner calls on the specification PR.

## Layout

| Directory | Area |
|---|---|
| `tasks/` | The task document: its contracts, completion predicate, and instruction body |
| `values/` | The five value forms and the tagged-value vocabulary |
| `references/` | Declaration identity, ids, and the dotted reference grammar |
| `actions/` | `exec`, `shell`, and `noop` actions, `bin` versus `command`, bindings |
| `expressions/` | The CEL profile |
| `nesting/` | The nesting joint and its output boundary |
| `effects/` | The effect kind: lifecycle, health, terminal, nesting |
| `workflows/` | The workflow kind, including user-owned populations |
| `channels/` | The channel kind |
| `providers/` | The workspace provider kind |
| `observers/` | The resource observer kind |
| `chains/` | Chains, a task-document construct |
| `plugins/` | Plugin and catalog manifests |
| `config/` | The reserved root files |

## Fixture grammar

Every fixture begins with an expectation header, then a `reason:` line stating
what it pins down, written as TOML comments — task fixtures included, since a
task is an ordinary TOML definition document like every other kind:

```toml
# plect-fixture: result=invalid layer=structural diagnostic=PLECTURE-CFG-VALUE-FROM-AND-EXPR entry=definitions
# reason: a value declares both from and expr, so its form is ambiguous.
```

A task fixture whose `instructions` array has a `file` element has a sidecar
Markdown file sitting beside it, named by that element's own `file` value.
The sidecar carries no header of its own — it is plain prose, exactly what a
real instruction sidecar is — so it is not itself walked as a graded fixture.

| Field | Values |
|---|---|
| `result` | `valid`, `invalid`, `accepted-invalid` |
| `layer` | `structural`, `semantic`, `cel`, `instantiation` — required unless `result=valid` |
| `diagnostic` | The `PLECTURE-CFG-*` code, which must appear in the diagnostics table in [`../../docs/language/README.md`](../../docs/language/README.md) |
| `entry` | Which schema entry validates it: `definitions` (default), `config`, `catalogs`, `lock`, `plugin`, `catalog` |

`result=accepted-invalid` records a rule the language states but the
implementation is permitted not to enforce — the sanctioned static-typing cut
line. The fixture loads today and documents what a complete checker would
reject.

A fixture's filename carries `.invalid` or `.accepted-invalid` before `.toml`
so a reader sees the outcome without opening it.

## Running the harness

```bash
go test ./app/internal/lang/
```

Two harnesses read the corpus, and they ask different questions:

- `TestConformanceFixtures` asks what
  [`../../plecture.schema.json`](../../plecture.schema.json) accepts. Every
  fixture must declare a well-formed expectation and decode as TOML. A `valid`
  fixture, and one whose declared failure belongs to a later layer, must pass
  the schema; an `invalid` fixture with `layer=structural` must be rejected by
  a rule the schema annotates with its declared diagnostic, which is what makes
  "something later rejects this" a claim about a later layer rather than an
  accident of shape.
- `TestNativeConformanceFixtures` asks what the implementation does: it loads
  each definition with this repository's own parsers and validators — resolving
  a task's `instructions` array as part of that load — and the
  diagnostic must be exactly the code and layer the expectation header
  documents. A rule it cannot yet reach is named in its `nativeDeferred` map
  rather than passing silently, and `layer=instantiation` fixtures belong to
  `TestNativeInstantiationFixtures`, which supplies the binding a load has no
  way to make.

Alongside them, `TestCodesMatchDocumentedTable` holds the diagnostic registry
and [`../../docs/language/README.md`](../../docs/language/README.md)'s table to
the same set of codes and layers, and
`TestChapterExamplesQuoteTheirFixtureVerbatim` holds every worked example in
`docs/language/` to the fixture it names.

A fixture that does not decode at all is rejected before any schema rule can be
blamed, so a decode failure satisfies a structural expectation and its
diagnostic is taken on trust, with no schema rule needing to name it.

It is a one-time verification tool for the specification, not a standing check:
nothing wires it into CI, and it has no invariant to guard once the
implementation's own conformance suite reads these fixtures directly.
