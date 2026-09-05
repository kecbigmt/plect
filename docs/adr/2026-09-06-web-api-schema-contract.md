# Define Web API contracts in TypeSpec and generate through OpenAPI

## Context

The [Web UI client/server decision](2026-09-05-web-ui-client-server-boundary.md)
places HTTP between the shared browser/desktop UI and the existing Go service
layer. The API needs an independently reviewable contract shared by Go and
TypeScript without duplicating business semantics.

Reusable response structures, model composition, and alternative response
shapes motivate templates, intersections, and unions. Handwritten OpenAPI
can express the resulting wire schemas, but its verbosity makes those
relationships harder to author and review.

Plecture also carries user-defined workflow inputs, Task state, and Effect
outputs. Their schemas are not all known when the Web UI is compiled.
A richer API definition language does not turn those values into statically
known client types.

## Decision

### Contract authority and generation

Use TypeSpec as the hand-edited source for Web HTTP contracts. Emit OpenAPI
as the intermediate contract consumed by Go and TypeScript generators:

```text
TypeSpec -> OpenAPI -> Go HTTP types/interfaces
                   -> TypeScript API types/client integration
```

Commit the emitted OpenAPI and generated Go/TypeScript source. Do not edit
generated files. Pin compiler, emitter, and generator versions; use pnpm
for JavaScript tooling. Generate deterministically and check for differences
in CI. Normal Go builds consume committed generated source without requiring
the TypeSpec toolchain.

The API schema governs wire shape, required fields, response codes, and
transport constraints. Existing language specifications and services govern
Plecture semantics. Keep the configuration-language schema separate from
the Web API contract.

### Thin Go boundary

Use net/http-compatible generated code. Handwritten handlers translate
API inputs to service arguments, call existing services/read projections,
and translate results and errors back to API responses.

Keep generated API types within the Web boundary. The service layer and
durable state contracts do not import them. Do not add generic repository,
use-case, or transport layers without concrete consumers.

Preserve authentication and CSRF protection at the HTTP boundary. Generated
types/interfaces do not replace runtime request validation, authentication,
or response-contract tests.

### Type composition and dynamic values

Use templates to reuse contract definitions, exposing concrete instantiated
wire models. Use intersections only where the resulting fields and constraints
are unambiguous. Prefer tagged, mutually distinguishable variants for unions.
Verify emitted anyOf/oneOf/allOf semantics rather than assuming a source-language
operator has an identical representation in generated Go.

Explicitly specify missing, null, empty, and unavailable values. Preserve
user-defined JSON in inputs/state/outputs; validate workflow-specific input
content using the existing selected workflow schema. Do not generate a new
static SDK for every user configuration or introduce an API-specific business
state to simplify a type.

Check numeric round trips, including integers outside JavaScript's safe range.
Choose representations based on actual fields and existing contracts; do not
globally reinterpret arbitrary user JSON or prescribe a custom decorator
without a demonstrated requirement.

### SSE and contract verification

Share event payload types between history responses and JSON SSE where their
wire shapes agree. Keep stream connection and recovery in a small dedicated
client. Document and test initial history/stream coordination, event IDs versus
resume cursors, duplicates, cancellation, disconnects, and replay failure.
Do not change the durable event contract to suit generated HTTP types.

CI checks schema compilation, reproducible generation, generated-code
compilation, and behavioral contract tests. Request/response tests cover
failure cases and conversions to existing service semantics. Detect breaking
wire changes for explicit review; pre-1.0 policy permits intentional breaks
with the required migration procedure, not speculative compatibility shims.

### Tool selection boundary

Use oapi-codegen as the baseline Go generator candidate and compare ogen
against the same small contract. Select on net/http integration, handwritten
adapter clarity, validation, error handling, and streaming integration, not
generated line count.

The TypeScript generator/client integration also remains a tool-selection
detail. Compare openapi-typescript/openapi-fetch with Orval as needed for the
actual TanStack Query consumer. Do not install multiple production pipelines.

Before fixing tool versions and an OpenAPI version, exercise released versions
with a concrete templated response, a tagged union, composed models, arbitrary
JSON, absent/null values, common errors, and a JSON event stream. Review the
emitted schema and generated Go/TypeScript behavior. No generator combination
is asserted to have passed this verification by this decision.

## Consequences

- Contracts can be reviewed independently of their Go implementation while
  generated code keeps client/server wire types aligned.
- TypeSpec adds a build-time tool and language, not a Go runtime dependency.
- Keeping OpenAPI provides a practical exit path to another authoring tool;
  migration still loses TypeSpec-specific authoring structure and may cost work.
- Explicit API/service conversion adds code but limits exposure of internal
  representation changes.
- Type composition is constrained by usable wire schemas and generated code.
  Stream recovery and dynamic configuration validation remain explicit work.
- Production CI gains generation and behavioral checks that prevent silent
  contract drift; it does not pin documentation wording or mock structure.

## Alternatives considered

**Handwritten OpenAPI.** Fewer authoring tools, but less concise reusable
definitions. Keep OpenAPI as the generated interoperability boundary.

**Go-first OpenAPI generation, including Huma.** Keeps registration, types,
and validation close to Go code, but makes Go the contract-authoring authority.
Independent schema review is the preferred development workflow.

**Protobuf with gRPC or Connect.** Strong generated RPC contracts and streaming
are viable. The Web UI's JSON values, existing HTTP/SSE services, and browser
delivery favor the chosen boundary. Desktop packaging alone does not require
RPC, and either transport still needs replay and reconnection semantics.

**Direct generation from TypeSpec.** Removes an intermediate artifact but
couples implementation tooling more directly to TypeSpec. OpenAPI provides
an explicit, inspectable interoperability and migration boundary.

## References

- [Web UI design](../design/web-ui.md)
- [TypeSpec language overview](https://typespec.io/docs/language-basics/overview/)
- [TypeSpec OpenAPI emitter](https://typespec.io/docs/emitters/openapi3/openapi/)
- [oapi-codegen](https://github.com/oapi-codegen/oapi-codegen)
- [ogen configuration](https://ogen.dev/docs/config/)
- [freee's TypeSpec-driven development experience](https://developers.freee.co.jp/entry/typespec-install)

The freee report informs the intermediate-OpenAPI approach; its generator
choices and deployment architecture are not adopted as a package.
