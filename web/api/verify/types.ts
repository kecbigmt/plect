// Compile-time structural verification: `tsc --noEmit` failing on this file
// is the TypeScript-side half of "invalid payloads are tested" (the Go side
// is app/internal/webapi/roundtrip_test.go's runtime unmarshal checks).
// This file has no runtime effect and is not imported by production code.
import type { components } from "../generated/typescript/schema.js";

type SessionDetail = components["schemas"]["SessionDetail"];
type SessionListResponse = components["schemas"]["SessionListResponse"];
type NotFoundError = components["schemas"]["NotFoundError"];
type ConflictError = components["schemas"]["ConflictError"];
type Event = components["schemas"]["Event"];
type EventPage = components["schemas"]["EventPage"];

// Valid: every required field present, several optional fields absent.
const validDetail: SessionDetail = {
  sessionName: "team/workspace-a",
  createdAt: "2026-09-06T00:00:00Z",
  run: "up",
  workspaceDirExists: true,
};

// Valid: the template-instantiated list envelope.
const validList: SessionListResponse = {
  items: [
    {
      sessionName: "team/workspace-a",
      displayStatus: "up",
      run: "up",
      resourceId: "",
    },
  ],
  count: 1,
};

// Invalid: `run` (required, and not one of SessionRunState's members) is
// missing — the compiler must reject this, proving the generated type
// actually enforces what SessionDetail's schema requires.
// @ts-expect-error - "run" is required
const invalidDetail: SessionDetail = {
  sessionName: "team/workspace-a",
  createdAt: "2026-09-06T00:00:00Z",
  workspaceDirExists: true,
};

// Invalid: the tagged union's discriminant fixes each leaf's `code` set —
// a NotFoundError's code cannot be a ConflictError code.
const invalidNotFound: NotFoundError = {
  category: "not_found",
  // @ts-expect-error - "has_children" is not a NotFoundError code
  code: "has_children",
  message: "wrong leaf",
};

// Valid: the same union member with its own code is fine.
const validConflict: ConflictError = {
  category: "conflict",
  code: "has_children",
  message: "session has children",
};

// Invalid: `branch` is typed `string | undefined`, not `string | null`, so
// an explicit null (which verify/fixtures.mjs proves JSON.parse would
// happily hand back at runtime) must be rejected here — the static type is
// stricter than the untyped runtime value this contract's own fixture
// (../../app/internal/webapi/testdata/session_detail.explicit_null.json)
// demonstrates. This is not a bug in the generated type: a producer that
// respects it can never emit null in the first place.
const invalidExplicitNull: SessionDetail = {
  sessionName: "team/workspace-a",
  createdAt: "2026-09-06T00:00:00Z",
  run: "up",
  workspaceDirExists: true,
  // @ts-expect-error - "branch" does not accept null, only string | undefined
  branch: null,
};

// Valid: `type`/`source` are plain strings, so a value no producer constant
// declares is not a type error — this contract does not enumerate them.
const eventWithUnknownType: Event = {
  id: "01JXAMPLE0000000000000020",
  sessionName: "team/parent",
  time: "2026-09-06T00:00:00Z",
  type: "acme.custom_provider.widget_moved",
  source: "acme-provider",
  direction: "inbound",
  summary: "widget moved",
  // Passed through verbatim, including a key no schema field promotes —
  // origin_session distinguishes this record's emitter from its own
  // sessionName (the receiver) purely by convention, not by a typed field.
  metadata: { origin_session: "team/child", widget_id: "w-42" },
};

// Invalid: `direction` is a fixed enum (unlike `type`/`source`) — an
// unrecognized value must be rejected at compile time.
const invalidDirection: Event = {
  id: "01JXAMPLE0000000000000021",
  sessionName: "team/workspace-a",
  time: "2026-09-06T00:00:00Z",
  type: "user.note",
  source: "cli",
  // @ts-expect-error - "sideways" is not an EventDirection member
  direction: "sideways",
  summary: "",
};

// Valid: the page envelope, nextCursor absent (a descending or exhausted page).
const validEventPage: EventPage = {
  events: [eventWithUnknownType],
};

// Referenced so `tsc --noEmit` treats every declaration above as used
// rather than reporting an unrelated "declared but never read" diagnostic
// that would mask the @ts-expect-error checks this file exists for.
export const fixtures = { validDetail, validList, validConflict, validEventPage };
void invalidDetail;
void invalidNotFound;
void invalidExplicitNull;
void invalidDirection;
