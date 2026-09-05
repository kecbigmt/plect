# Plecture Web API contract

The hand-edited TypeSpec source for the Plecture Web API, and the OpenAPI and
TypeScript artifacts generated from it. See
[the schema-contract ADR](../../docs/adr/2026-09-06-web-api-schema-contract.md)
for the decision this package implements, and
[docs/design/web-ui.md](../../docs/design/web-ui.md) for the UI this contract
serves.

This slice covers Session list/detail and common errors only — see
[Scope](#scope) below.

## Layout

| Path | What |
| --- | --- |
| `main.tsp`, `models/`, `routes/` | Hand-edited TypeSpec source. Edit these. |
| `generated/openapi.yaml` | Emitted OpenAPI 3.0 document. Committed, not edited. |
| `generated/typescript/schema.d.ts` | Generated TypeScript types. Committed, not edited. |
| `client.ts` | Hand-written thin `openapi-fetch` client bound to the generated types. |
| `testdata/` | JSON fixtures shared (by content, not by symlink) with `app/internal/webapi/testdata/` — see [Verification](#verification). |
| `verify/` | TypeScript-side verification: compile-time structural checks (`types.ts`) and a runtime fixture script (`fixtures.mjs`). |
| `../../app/internal/webapi/generated/types.gen.go` | Generated Go types (`oapi-codegen`, types only). Committed, not edited. |
| `../../app/internal/webapi/` | Hand-written Go boundary: conversion from/to `service.*`, HTTP handlers, tests. |

## Regenerating

After editing the TypeSpec source:

```sh
cd web/api
pnpm install --frozen-lockfile   # first time, or after a devDependency change
pnpm generate                    # tsp compile -> generated/openapi.yaml -> generated/typescript/schema.d.ts
```

Then regenerate the Go types:

```sh
cd app/internal/webapi
go tool oapi-codegen -config oapi-codegen.yaml ../../../web/api/generated/openapi.yaml
# equivalently: go generate ./app/internal/webapi/...
```

Both steps are deterministic — running them again with no source change
produces byte-identical output (verified for this PR: re-running `pnpm
generate` reproduced `generated/openapi.yaml` and
`generated/typescript/schema.d.ts` unchanged; re-running the `oapi-codegen`
command reproduced `types.gen.go` unchanged). CI should run both and fail on
a diff, the same way `scripts/check-agent-config.sh` and the config-language
corpus harnesses already fail CI on drift — wiring that CI job is left to a
follow-up so this PR stays scoped to the toolchain itself.

Normal Go builds consume the committed `types.gen.go` and need neither the
TypeSpec compiler nor Node — only editing the contract does.

## Tool versions

Pinned exactly in `package.json` + `pnpm-lock.yaml` (JavaScript side) and as
a `go get -tool` dependency in `app/go.mod` (Go side):

| Tool | Version | Role |
| --- | --- | --- |
| pnpm | 10.33.0 | Package manager, matching the version already pinned elsewhere in this repo (`app/internal/webui/package.json`, `docs/design/web-ui/prototype/package.json`). |
| `@typespec/compiler`, `@typespec/http`, `@typespec/openapi`, `@typespec/openapi3` | 1.15.0 | TypeSpec source -> OpenAPI 3.0. |
| `oapi-codegen` (`github.com/oapi-codegen/oapi-codegen/v2`) | v2.8.0 | OpenAPI -> Go types. |
| `openapi-typescript` | 7.13.0 | OpenAPI -> TypeScript types. |
| `openapi-fetch` | 0.17.0 | Thin typed fetch client (`client.ts`). |
| `typescript` | 5.9.3 | Type-checks `verify/types.ts` and this package's other `.ts` files. Pinned to the 5.x line `openapi-typescript@7.13.0` declares as its peer range — TypeScript 7.0 (the Go-ported compiler) is current `latest` on npm at the time of writing but was not exercised against this toolchain, so pinning to it here would be an unverified jump for no present benefit. |

These were proposed as candidates and ratified by the dispatcher on
[issue #400](https://github.com/kecbigmt/plecture/issues/400) before this
package was built on them, per that issue's amendment.

## Scope

Session list/detail and common errors, matching the parent issue's
deliverable. Explicitly **not** in this slice: Tasks, Graph, Terminal,
Home/Inbox, or any mutation (Create/Up/Down/user.emit) — see
[docs/design/web-ui-graph-fields.md](../../docs/design/web-ui-graph-fields.md)
for how Graph inspection maps to existing state ahead of its own task.

`ApiError`'s four variants (`NotFoundError`, `ValidationError`,
`ConflictError`, `ExecutionError`) cover every code `service.Error` defines
today, not only the ones `GET /sessions` and `GET /sessions/{name}` can
currently return — this is the "common errors" half of the deliverable,
meant for later Web API operations to reuse rather than reinvent, per
`app/internal/webapi/errors.go`'s classification table.

## Specified contract properties

- **Names containing `/`.** Session names are the path parameter on `GET
  /sessions/{name}`. OpenAPI 3's parameter object has `allowReserved` for
  exactly this ("send `/` unencoded rather than as `%2F`"), and the
  TypeSpec source declares it (`routes/sessions.tsp`) — but `@typespec/openapi3`
  1.15.0 does not currently emit it (confirmed: compiling produces
  `@typespec/openapi3/path-reserved-expansion` warning, and the emitted
  parameter carries no `allowReserved` key). The Go server does not rely on
  the OpenAPI document for this: `app/internal/webapi/handler.go` routes
  `GET /sessions/{name...}` with Go's own wildcard path capture (the same
  shape `webui`'s existing HTML routes already use for session names), so a
  literal `/` in the request path reaches the handler intact regardless of
  what the OpenAPI parameter object says. A client generated purely from
  `generated/typescript/schema.d.ts` must still send the name unencoded (see
  the parameter's `@doc`) — `openapi-fetch` does this by default, since it
  performs simple, non-percent-encoding string substitution for path
  parameters.
- **Absent, null, and empty values.** Every optional field on this contract
  is omitted (never emitted as JSON `null`) when unset, mirroring the Go
  source's `omitempty` struct tags — this API never distinguishes "unset"
  from "explicitly null" because nothing in the underlying service/state
  layer does either. A *required* field (e.g. `SessionSummary.resourceId`)
  can still legitimately be an empty string; see `testdata/session_list.valid.json`'s
  second item and the round-trip tests that assert on it.
- **Status distinctions.** `SessionRunState` (`up`/`down`) and
  `SessionHealthState` (`healthy`/`unhealthy`/`undeclared`/`stalled`) are
  separate enums on separate fields, matching
  [docs/design/web-ui.md](../../docs/design/web-ui.md)'s dimensions table —
  a session can be `up` and `unhealthy` at once, and this contract does not
  collapse them into one combined status.
- **Authentication/bootstrap.** Documented on the service (`main.tsp`'s
  top-level `@doc`) as the existing `plect_auth` cookie the current
  Go-templated Web UI already enforces at the HTTP boundary
  (`app/internal/webui/security.go`) — generation supplies no
  authentication of its own, and the mounted routes (`webui/server.go`)
  sit behind the same `authMiddleware`/`csrfMiddleware` chain as every
  other route in that server.
- **API version.** `/api/v1`, declared via `@server` in `main.tsp` and
  mounted at that literal prefix in `app/internal/webui/server.go`.

## Verification

`app/internal/webapi/roundtrip_test.go` and `verify/fixtures.mjs` +
`verify/types.ts` exercise the same JSON fixtures (`testdata/`) from the Go
and TypeScript sides respectively, each demonstrating and recording an
actual result rather than assuming one:

- **Template instantiation.** `ListResponse<SessionSummary>` (TypeSpec) ->
  `SessionListResponse` (Go/TS) round-trips as a named type on both sides.
- **Model composition.** `SessionDetail extends SessionIdentity { ...SessionRuntime; ... }`
  emits as OpenAPI `allOf`; `oapi-codegen`'s types-only generation flattens
  it into one Go struct carrying every field from both halves (verified in
  `TestRoundTrip_SessionDetailComposition`), and `openapi-typescript`
  represents it as a TypeScript intersection type with the same effect.
- **Tagged union.** `ApiError`'s `@discriminator("category")` emits as
  OpenAPI `discriminator` + `mapping`; `oapi-codegen` generates four
  distinct Go leaf structs (no shared Go interface/sum type, since nothing
  in this slice's paths declares a `oneOf` response body — each operation's
  error responses are already discriminated by HTTP status instead).
  `errors.go`'s `DecodeApiError` is this package's own read-side dispatch on
  the `category` field, tested against both a recognized and an
  unrecognized category.
- **Dynamic JSON.** `SessionIdentity.inputs` (`Record<unknown>` / `map[string]interface{}`)
  preserves arbitrary user-defined JSON verbatim.
- **Safe numeric handling.** A value past `Number.MAX_SAFE_INTEGER`
  (2^53-1) inside `inputs` decodes identically lossy on both sides — Go's
  `encoding/json` and V8's `JSON.parse` both represent an untyped JSON
  number as an IEEE754 double, so `9007199254740993` becomes
  `9007199254740992` under both. This is an existing, shared limitation of
  representing arbitrary user JSON as a generic map, not something this PR
  introduces a decorator to paper over — the schema-contract ADR asks for a
  demonstrated requirement before adding one, and Session list/detail has
  no fixed-schema field needing int64-as-string encoding today.
- **Valid/invalid payloads.** `encoding/json.Unmarshal` and `JSON.parse`
  neither one enforces OpenAPI's `required` list — a payload missing `run`
  decodes without error on both sides, leaving it at the Go zero value
  (`""`, `Valid() == false`) or simply absent from the parsed JS object.
  Catching this is explicit request-validation work `docs/adr/2026-09-06-web-api-schema-contract.md`
  already calls out as ungenerated, and `verify/types.ts` additionally
  proves the compile-time half: `tsc --noEmit` rejects the same omission
  when a literal is typed against the generated `SessionDetail`.

Run everything:

```sh
cd app && go test ./internal/webapi/... ./internal/webui/...
cd web/api && pnpm verify   # tsc --noEmit, then verify/fixtures.mjs
```

## What this PR does not claim

Per the schema-contract ADR: this generation chain supplies wire types and
their round-trip behavior. It does not supply authentication, CSRF
protection, or SSE reconnection/replay semantics — those remain explicit,
handwritten, and separately tested at the HTTP boundary.
