// Compile-time structural verification: `tsc --noEmit` failing on this file
// is the TypeScript-side half of "invalid payloads are tested" (the Go side
// is app/internal/webapi/roundtrip_test.go's runtime unmarshal checks).
// This file has no runtime effect and is not imported by production code.
import type { components } from "../generated/typescript/schema.js";

type SessionDetail = components["schemas"]["SessionDetail"];
type SessionListResponse = components["schemas"]["SessionListResponse"];
type NotFoundError = components["schemas"]["NotFoundError"];
type ConflictError = components["schemas"]["ConflictError"];

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

// Referenced so `tsc --noEmit` treats every declaration above as used
// rather than reporting an unrelated "declared but never read" diagnostic
// that would mask the @ts-expect-error checks this file exists for.
export const fixtures = { validDetail, validList, validConflict };
void invalidDetail;
void invalidNotFound;
