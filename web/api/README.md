# Plecture Web API contract

The hand-edited TypeSpec source for the Plecture Web API, and the OpenAPI and
TypeScript artifacts generated from it. See
[the schema-contract ADR](../../docs/adr/2026-09-06-web-api-schema-contract.md)
for the decision this package implements, and
[docs/design/web-ui.md](../../docs/design/web-ui.md) for the UI this contract
serves.

This slice covers Session list/detail, bounded per-session event history,
and common errors only — see [Scope](#scope) below.

`web/` is a pnpm workspace (`web/pnpm-workspace.yaml`) with this package and
[`web/app`](../app/README.md) — the React Web UI shell — as members, sharing
one lockfile at the workspace root. `web/app` depends on this package as
`@plecture/web-api` via `workspace:*` for `client.ts` and the generated
TypeScript types, rather than reimplementing its own HTTP client.

## Layout

| Path | What |
| --- | --- |
| `main.tsp`, `models/`, `routes/` | Hand-edited TypeSpec source. Edit these. |
| `generated/openapi.yaml` | Emitted OpenAPI 3.0 document. Committed, not edited. |
| `generated/typescript/schema.d.ts` | Generated TypeScript types. Committed, not edited. |
| `client.ts` | Hand-written thin `openapi-fetch` client bound to the generated types. |
| `verify/` | TypeScript-side verification: compile-time structural checks (`types.ts`), a fixture round-trip script (`fixtures.mjs`), and a client-transport script (`client-transport.mjs`) — see [Verification](#verification). |
| `../../app/internal/webapi/testdata/` | The one JSON fixture authority, read by both `app/internal/webapi/roundtrip_test.go` (Go) and `verify/fixtures.mjs` (TypeScript, by relative path — not copied). |
| `../../app/internal/webapi/generated/types.gen.go` | Generated Go types (`oapi-codegen`, types only). Committed, not edited. |
| `../../app/internal/webapi/` | Hand-written Go boundary: conversion from/to `service.*`, HTTP handlers, tests. |

## Regenerating

After editing the TypeSpec source:

```sh
cd web
pnpm install --frozen-lockfile   # first time, or after a devDependency change; workspace-wide
cd api
pnpm generate                    # tsp compile -> generated/openapi.yaml -> generated/typescript/schema.d.ts
```

Then regenerate the Go types:

```sh
cd app/internal/webapi
go tool oapi-codegen -config oapi-codegen.yaml ../../../web/api/generated/openapi.yaml
# equivalently: go generate ./app/internal/webapi/...
```

Both steps are deterministic — running them again with no source change
produces byte-identical output. CI's `web-api-contract` job
(`.github/workflows/ci.yml`) runs both regeneration commands on every PR and
fails on any diff against the committed output, the same way
`scripts/check-agent-config.sh` and the config-language corpus harnesses
already fail CI on drift.

Normal Go builds consume the committed `types.gen.go` and need neither the
TypeSpec compiler nor Node — only editing the contract does.

## Tool versions

Pinned exactly in `package.json` + the workspace-root `pnpm-lock.yaml`
(JavaScript side) and as a `go get -tool` dependency in `app/go.mod` (Go
side):

| Tool | Version | Role |
| --- | --- | --- |
| pnpm | 10.33.0 | Package manager, matching the version already pinned elsewhere in this repo (`app/internal/webui/package.json`, `docs/design/web-ui/prototype/package.json`, and `web/app/package.json`). |
| `@typespec/compiler`, `@typespec/http`, `@typespec/openapi`, `@typespec/openapi3` | 1.15.0 | TypeSpec source -> OpenAPI 3.0. |
| `oapi-codegen` (`github.com/oapi-codegen/oapi-codegen/v2`) | v2.8.0 | OpenAPI -> Go types. |
| `openapi-typescript` | 7.13.0 | OpenAPI -> TypeScript types. |
| `openapi-fetch` | 0.17.0 | Thin typed fetch client (`client.ts`). |
| `typescript` | 5.9.3 | Type-checks `verify/types.ts` and this package's other `.ts` files. Pinned to the 5.x line `openapi-typescript@7.13.0` declares as its peer range — TypeScript 7.0 (the Go-ported compiler) is current `latest` on npm at the time of writing but was not exercised against this toolchain, so pinning to it here would be an unverified jump for no present benefit. |

These were proposed as candidates and ratified by the dispatcher on
[issue #400](https://github.com/kecbigmt/plecture/issues/400) before this
package was built on them, per that issue's amendment.

## Scope

Session list/detail, bounded per-session event history (`GET /events`), and
common errors, matching the parent issues' deliverables. Explicitly **not**
in this slice: Tasks, Graph, Terminal, Home/Inbox, or any mutation
(Create/Up/Down/user.emit) — see
[docs/design/web-ui-graph-fields.md](../../docs/design/web-ui-graph-fields.md)
for how Graph inspection maps to existing state ahead of its own task, and
[docs/design/web-ui-event-history.md](../../docs/design/web-ui-event-history.md)
for the event-history read contract and its history/live handoff protocol
(the live SSE stream itself is a later task).

`ApiError`'s four variants (`NotFoundError`, `ValidationError`,
`ConflictError`, `ExecutionError`) cover every code `service.Error` defines
today, not only the ones `GET /sessions`, `GET /sessions/{name}`, and
`GET /events` can currently return — this is the "common errors" half of the
deliverable, meant for later Web API operations to reuse rather than
reinvent, per `app/internal/webapi/errors.go`'s classification table.

## Specified contract properties

- **Names containing `/`.** Session names are the path parameter on `GET
  /sessions/{name}`. OpenAPI 3's parameter object has `allowReserved` for
  exactly this ("send `/` unencoded rather than as `%2F`"), but
  `@typespec/openapi3` 1.15.0 does not currently emit it — declaring it in
  `routes/sessions.tsp` only produced a
  `@typespec/openapi3/path-reserved-expansion` warning and an emitted
  parameter with no `allowReserved` key, so the TypeSpec source does not
  declare it: doing so would assert a contract guarantee the emitted
  document never actually carries. A generated client therefore has no
  signal to send `/` unencoded, and **does not**:
  `verify/client-transport.mjs` proves the committed `client.ts`, built on
  `openapi-fetch` 0.17.0, percent-encodes `team/workspace-a` as
  `team%2Fworkspace-a` (RFC 3986's default path-segment escaping) — that
  script imports and calls `client.ts`'s own `createPlectureWebApiClient`
  against a captured `fetch`, not a reimplementation of the same
  `openapi-fetch` call. The server does not depend on the client doing
  otherwise: `app/internal/webapi/handler.go` routes `GET
  /sessions/{name...}` with Go's own wildcard path capture, matched against
  `net/http`'s already percent-decoded `URL.Path` — so both an unencoded `/`
  and an encoded `%2F` arrive at the handler as the same, correct, full
  session name. `app/internal/webapi/transport_test.go` proves this through
  a real `net/http` server (not a direct handler call, which never
  round-trips a raw request line through URL parsing) for both forms.
- **Absent, null, and empty values.** Every optional field this contract's
  Go source produces is omitted (never emitted as JSON `null`) when unset,
  mirroring its `omitempty` struct tags. A *required* field (e.g.
  `SessionSummary.resourceId`) can still legitimately be an empty string;
  see `session_list.valid.json`'s second item and the round-trip tests that
  assert on it. Nothing stops a non-conformant producer from sending an
  explicit `null` anyway, so both consumers are tested against one:
  `session_detail.explicit_null.json`. Go's decoder unifies null and
  absence for every optional field here — both leave the pointer/slice/map
  nil (`TestRoundTrip_ExplicitNullOptionalFieldsDecodeSameAsAbsent`).
  `JSON.parse` does **not** perform the same unification: `null` stays a
  present key holding the value `null`, distinguishable from an absent key
  via the `in` operator (`verify/fixtures.mjs`'s corresponding check) — and
  the *static* TypeScript type (`string | undefined`, never `| null`)
  rejects an explicit `null` at compile time regardless
  (`verify/types.ts`'s `invalidExplicitNull`), so a producer that respects
  the generated types can never emit one in the first place.
- **Status distinctions.** `SessionRunState` (`up`/`down`) and
  `SessionHealthState` (`healthy`/`unhealthy`/`undeclared`/`stalled`) are
  separate enums on separate fields, matching
  [docs/design/web-ui.md](../../docs/design/web-ui.md)'s dimensions table —
  a session can be `up` and `unhealthy` at once, and this contract does not
  collapse them into one combined status.
- **List ordering.** `GET /sessions` returns entries ascending by session
  name — `service.List`'s own documented and tested sort, which this
  package only passes through
  (`TestHandleList_DoesNotReorderWhatServiceListReturns`). Not "newest
  first": nothing about recency or activity determines order.
- **Authentication/bootstrap.** Documented on the service (`main.tsp`'s
  top-level `@doc`) as the existing `plect_auth` cookie the current
  Go-templated Web UI already enforces at the HTTP boundary
  (`app/internal/webui/security.go`) — generation supplies no
  authentication of its own, and the mounted routes (`webui/server.go`)
  sit behind the same `authMiddleware`/`csrfMiddleware` chain as every
  other route in that server. The React shell's own startup needs (API
  version, CSRF token value) are served by a separate, hand-written `GET
  /api/v1/bootstrap` outside this generated contract — see
  `app/internal/webui/bootstrap.go` and [`web/app/README.md`](../app/README.md).
- **API version.** `/api/v1`, declared via `@server` in `main.tsp` and
  mounted at that literal prefix in `app/internal/webui/server.go`.

## Verification

`app/internal/webapi/roundtrip_test.go` and `verify/fixtures.mjs` +
`verify/types.ts` exercise the same JSON fixtures
(`app/internal/webapi/testdata/`, the one fixture authority — see
[Layout](#layout)) from the Go and TypeScript sides respectively, each
demonstrating and recording an actual result rather than assuming one:

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
  Catching this is explicit request-validation work the schema-contract ADR
  already calls out as ungenerated, and `verify/types.ts` additionally
  proves the compile-time half: `tsc --noEmit` rejects the same omission
  when a literal is typed against the generated `SessionDetail`. On the
  encode side, `TestHandleGet_ResponseJSONOmitsEveryUnsetOptionalKey`
  decodes an actual handler response into a bare `map[string]any` and
  checks which keys are present — proving the wire bytes, not just the Go
  struct's nil pointers, actually omit every unset optional field.
- **Unknown event types and metadata survive the projection.**
  `event_page.valid.json` includes an event whose `type`/`source` no
  producer constant declares and whose `metadata` carries a key
  (`origin_session`) this API does not itself interpret;
  `TestRoundTrip_EventPageUnknownTypeAndMetadataSurviveDecode` and
  `verify/fixtures.mjs` both prove it decodes verbatim rather than being
  dropped, coerced, or requiring a schema update to add a new event type.
  `verify/types.ts` additionally proves `type`/`source` are unconstrained
  strings while `direction` stays a checked enum — see
  [docs/design/web-ui-event-history.md](../../docs/design/web-ui-event-history.md)
  for the read contract and handoff protocol this fixture backs.
- **Transport reality, not just the schema's claim.** `verify/client-transport.mjs`
  and `app/internal/webapi/transport_test.go` (see "Names containing `/`"
  above) exercise the actual committed client and a real HTTP server rather
  than asserting from the OpenAPI document what a client or server *should*
  do.

Run everything:

```sh
cd app && go test ./internal/webapi/... ./internal/webui/...
cd web/api && pnpm verify   # tsc --noEmit, then fixtures.mjs, then client-transport.mjs
```

## What this PR does not claim

Per the schema-contract ADR: this generation chain supplies wire types and
their round-trip behavior. It does not supply authentication, CSRF
protection, or SSE reconnection/replay semantics — those remain explicit,
handwritten, and separately tested at the HTTP boundary.
