# Plecture Configuration Language: Specification and Schema Maintenance

## Purpose

Plecture configuration should be treated as a language of its own even if its concrete representation remains TOML and its expression sublanguage is CEL.

The language specification must be strong enough to support reliable authoring, static analysis, editor tooling, migrations, and LLM-generated configuration. At the same time, Plecture should avoid maintaining a large hand-written specification that duplicates implementation details and gradually diverges from the code.

The documentation and tooling model therefore separates four concerns:

| Layer                    | Purpose                                    | Primary source of truth                                 |
| ------------------------ | ------------------------------------------ | ------------------------------------------------------- |
| Semantic specification   | Stable language semantics and invariants   | Hand-written design documentation                       |
| Structural schema        | The accepted TOML shapes and field types   | Generated from the implementation model where practical |
| Executable specification | Precise valid, invalid, and boundary cases | Conformance fixtures and compiler tests                 |
| Rationale                | Why a language decision was made           | ADRs                                                    |

These layers complement rather than duplicate one another.

## 1. Keep the Hand-Written Language Specification Thin

Hand-written language documentation should describe semantics that authors and independent implementations need to understand and that are expected to remain meaningful even if the implementation changes.

It should not attempt to reproduce every decoder field, optional property, enum value, or validation branch.

For example, the value model may be specified normatively as:

```text
A dynamic Plecture value is either:

- a projection expressed with `from`; or
- a computation expressed with `expr`.

`from` preserves the native type of the source value.
A missing source is an error unless `default` is declared.
`expr` contains a CEL expression.
A pure projection should use `from` rather than `expr`.
```

Similarly, task nesting documentation should specify durable invariants such as:

* inner and outer tasks remain homogeneous;
* nested lifecycle execution is LIFO;
* inner inputs configure the boundary of the inner task;
* inner outputs are never implicitly promoted into the outer public contract;
* outer public outputs are explicitly projected or computed;
* only a direct projection may preserve write-through semantics where such semantics are supported.

The language specification should answer **what a construct means**, not exhaustively mirror **how the current Go structs happen to represent it**.

## 2. Publish a Machine-Readable Structural Schema

Plecture should expose a machine-readable schema for the structural shape of its TOML configuration.

Conceptually:

```text
Plecture config model
        │
        ├── decoder
        ├── compiler
        └── schema generation
                 │
                 ▼
        plecture.schema.json
```

Where practical, this schema should be derived from the same implementation model used by the decoder rather than maintained independently by hand.

The schema can describe rules such as:

* available definition kinds;
* valid fields for a task, workflow, channel, workspace provider, and other definition kinds;
* required and optional fields;
* mutually exclusive variants;
* action variants such as `exec`, `shell`, and `noop`;
* tagged value shapes such as:

```toml
value = { from = "inputs.foo" }
value = { from = "inputs.foo", default = "" }
value = { expr = "inputs.a + inputs.b" }
```

For example, the structural schema can reject a value that declares both `from` and `expr`.

The structural schema is intended for:

* editor validation and completion;
* Taplo or equivalent TOML tooling;
* configuration generators;
* LLM context;
* generated reference documentation;
* early validation before semantic compilation.

The structural schema is **not** the complete specification of the Plecture language. It should not be stretched to encode cross-definition or runtime-dependent semantics that are better handled by the compiler.

## 3. Separate Static Checking into Three Layers

Plecture should explicitly distinguish three classes of static validation.

### Structural validation

Performed against the configuration schema.

Examples:

* an unknown field;
* a missing required field;
* an unknown `type` discriminator;
* `{ from = ..., expr = ... }`;
* an invalid TOML shape for an action or tagged value.

### Plecture semantic validation

Performed by the Plecture compiler after structural parsing.

Examples:

* a referenced definition does not exist;
* a reference resolves to the wrong definition kind;
* a `from` path names an unavailable context or field;
* a workflow node refers to an output not declared by the referenced task;
* task nesting contains a cycle;
* an inner input binding does not satisfy the inner input contract;
* a terminal capability is requested where no terminal is available;
* workflow dependencies derived from data flow form an invalid graph;
* plugin, layer, ownership, or trust rules are violated.

These checks are part of the Plecture language semantics and should remain owned by Plecture rather than being encoded awkwardly in JSON Schema.

### CEL validation

Performed by the CEL parser and checker using the environment declared for a particular expression site.

Examples:

* invalid CEL syntax;
* an unknown visible variable;
* an invalid operation between incompatible types;
* an expression whose result type does not satisfy the expected type of the containing field.

The intended pipeline is therefore:

```text
TOML
  │
  ▼
Structural validation
  │
  ▼
Plecture semantic validation
  │
  ▼
CEL parsing and type checking
  │
  ▼
Compiled Plecture representation
```

The exact ordering may be optimized by the implementation, but the responsibility boundaries should remain clear.

## 4. Maintain Conformance Fixtures as the Detailed Specification

Detailed language behavior should primarily be captured as executable test fixtures rather than prose.

A possible layout is:

```text
testdata/config-language/
  values/
    from.toml
    from-default.toml
    from-and-expr.invalid.toml

  references/
    relative.toml
    qualified.toml
    wrong-kind.invalid.toml

  actions/
    exec.toml
    shell-bind.toml

  nesting/
    direct-output.toml
    computed-output.toml
    cycle.invalid.toml

  expressions/
    valid.toml
    unknown-name.invalid.toml
    type-mismatch.invalid.toml
```

Each fixture should have a stable expected result, such as:

```text
source
→ expected compiled representation
```

or:

```text
source
→ expected diagnostic
```

These fixtures form an executable language specification.

They can eventually be reused by:

* the main Plecture implementation;
* an LSP;
* migration tooling;
* alternative implementations;
* schema-generation verification;
* configuration formatters or analyzers.

Whenever a language behavior is subtle enough to require a long paragraph explaining all of its edge cases, it should usually also have conformance fixtures that make those cases executable.

## 5. Treat Diagnostics as Part of the Language Tooling Interface

Static validation is particularly valuable when configuration is authored or modified by coding agents.

Plecture should therefore prefer structured, stable diagnostics over relying only on human-readable error strings.

For example:

```text
PLECTURE-CFG-UNKNOWN-REF
PLECTURE-CFG-KIND-MISMATCH
PLECTURE-CFG-FROM-MISSING
PLECTURE-CFG-CEL-TYPE
PLECTURE-CFG-NESTING-CYCLE
```

The exact naming scheme can be decided separately, but the principle is that:

* diagnostic codes remain stable when practical;
* human-readable messages may improve over time;
* diagnostics identify the relevant source location;
* tooling can distinguish structural, semantic, and expression errors.

This enables a reliable agent loop:

```text
generate configuration
        ↓
plecture check
        ↓
structured diagnostics
        ↓
repair configuration
```

Diagnostics should not become a frozen ABI prematurely, but language-level error categories are useful enough to design intentionally.

## 6. Specify a Small Plecture CEL Profile

Plecture should reference CEL as an external language rather than reproduce its grammar or semantics.

Plecture only needs to specify the profile in which CEL is embedded.

That profile should define:

* the CEL version or compatibility target;
* enabled standard macros and extensions;
* Plecture-specific custom functions, if any;
* the variables available at each expression site;
* the expected result type at each expression site;
* any Plecture-specific restrictions on otherwise valid CEL expressions.

Custom CEL functions should be kept to a minimum.

Plecture-specific static concepts such as:

* definition references;
* plugin ownership;
* `from` projections;
* task nesting;
* terminal capabilities;
* executable ownership;

should normally remain Plecture TOML structures rather than CEL APIs.

CEL should primarily handle actual computation:

```toml
message = { expr = '''
"[" + event.type + "] " +
(event.body != "" ? event.body : event.summary)
''' }
```

A simple projection should remain:

```toml
owner = { from = "inputs.owner" }
```

rather than:

```toml
owner = { expr = "inputs.owner" }
```

This keeps the CEL profile small and preserves semantic information that Plecture can analyze without interpreting a general expression.

## Source-of-Truth Rules

The four layers should have deliberately different responsibilities.

### Semantic documentation

Authoritative for:

* conceptual meaning;
* invariants;
* composition rules;
* security properties;
* language boundaries.

It should remain concise and stable.

### Generated structural schema

Authoritative for:

* currently accepted structural shapes;
* field types;
* required and optional fields;
* discriminated variants.

It should track the implementation automatically where practical.

### Conformance fixtures and compiler tests

Authoritative for:

* exact behavior at edge cases;
* valid and invalid examples;
* static-analysis outcomes;
* regression protection.

These are the detailed executable specification.

### ADRs

Authoritative for:

* why a major language decision was made;
* alternatives considered;
* compatibility and migration reasoning.

ADRs should not become permanent field-reference manuals.

## Documentation Strategy

Human-facing documentation can then remain small.

A minimal language documentation set may contain:

```text
docs/
  language/
    README.md
    values.md
    expressions.md

  design/
    task-nesting.md
    plugin-packaging.md
    ...

  adr/
    ...
```

The detailed reference can be generated from the structural schema or exposed through tooling rather than reproduced manually.

For example, Plecture may eventually provide commands such as:

```text
plect config schema
plect config check
```

and possibly a generated configuration reference.

This keeps prose focused on understanding the language while machines consume the exact current structural definition.

## Principle

The overall maintenance rule is:

> **Humans maintain meaning.
> Code generates shape.
> Tests preserve behavior.
> ADRs preserve reasoning.**

This division should allow Plecture to develop a well-specified configuration language without creating a second implementation in Markdown that inevitably drifts away from the compiler.

