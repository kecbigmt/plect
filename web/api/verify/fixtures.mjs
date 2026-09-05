#!/usr/bin/env node
// Runtime round-trip verification, the TypeScript-side counterpart to
// app/internal/webapi/roundtrip_test.go. Plain Node + node:assert rather
// than a test framework: this slice did not get sign-off to add a
// JavaScript test runner as a second ratified tool, and these are simple
// value assertions, not a suite that benefits from one.
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import path from "node:path";

const here = path.dirname(fileURLToPath(import.meta.url));
const testdata = path.join(here, "..", "testdata");

async function readFixture(name) {
  return JSON.parse(await readFile(path.join(testdata, name), "utf8"));
}

let failures = 0;

function check(name, fn) {
  try {
    fn();
    console.log(`ok - ${name}`);
  } catch (err) {
    failures += 1;
    console.error(`FAIL - ${name}`);
    console.error(err);
  }
}

const list = await readFixture("session_list.valid.json");
check("session list envelope has the template's two fields", () => {
  assert.equal(list.count, 2);
  assert.equal(list.items.length, 2);
});
check("a required field can legitimately be an empty string", () => {
  assert.equal(list.items[1].resourceId, "");
});
check("an absent optional field is simply missing, not null", () => {
  assert.equal("branch" in list.items[1], false);
});

const detail = await readFixture("session_detail.valid.json");
check("SessionDetail carries fields from both composed halves", () => {
  assert.equal(detail.sessionName, "team/workspace-a"); // SessionIdentity
  assert.equal(detail.workspaceDirExists, true); // SessionRuntime
});
check(
  "dynamic inputs JSON: a safe integer round-trips exactly through JSON.parse",
  () => {
    assert.equal(detail.inputs.retries, 3);
  },
);
check(
  "dynamic inputs JSON: an integer past 2^53-1 loses precision under JSON.parse, matching Go's float64 decode",
  () => {
    // 9007199254740993 (2^53+1) is not representable as an IEEE754 double;
    // V8's JSON.parse rounds it to the nearest representable value,
    // 9007199254740992 (2^53) — the same value
    // app/internal/webapi/roundtrip_test.go asserts for Go's own decode.
    assert.equal(detail.inputs.budget_bytes, 9007199254740992);
    assert.equal(Number.isSafeInteger(detail.inputs.budget_bytes), false);
  },
);

const invalidDetail = await readFixture(
  "session_detail.invalid_missing_run.json",
);
check(
  "JSON.parse does not enforce required fields either — 'run' is simply absent, not an error",
  () => {
    assert.equal("run" in invalidDetail, false);
  },
);

const notFoundError = await readFixture("error_not_found.json");
const conflictError = await readFixture("error_conflict.json");
check("ApiError tagged union: category selects the code's own enum", () => {
  assert.equal(notFoundError.category, "not_found");
  assert.equal(notFoundError.code, "session_not_found");
  assert.equal(conflictError.category, "conflict");
  assert.equal(conflictError.code, "has_children");
});

if (failures > 0) {
  console.error(`\n${failures} check(s) failed`);
  process.exit(1);
}
console.log(`\nall checks passed`);
