#!/usr/bin/env node
// Exercises the actual committed client.ts factory (not a re-implementation
// of its openapi-fetch call) against a captured global fetch, to record what
// request path it really sends for a slash-containing session name — rather
// than assuming one. Run with --experimental-strip-types (see package.json's
// verify:client-transport script) so this plain-JS script can import client.ts
// directly.
//
// This is the client-side half of
// app/internal/webapi/transport_test.go's server-side proof.
import assert from "node:assert/strict";
import { createPlectureWebApiClient } from "../client.ts";

let capturedPath = null;
const originalFetch = globalThis.fetch;
globalThis.fetch = async (input) => {
  const url = typeof input === "string" ? input : input.url;
  capturedPath = new URL(url).pathname;
  return new Response("{}", {
    status: 200,
    headers: { "content-type": "application/json" },
  });
};

try {
  const client = createPlectureWebApiClient("http://example.invalid/api/v1");
  await client.GET("/sessions/{name}", {
    params: { path: { name: "team/workspace-a" } },
  });
} finally {
  globalThis.fetch = originalFetch;
}

// openapi-fetch percent-encodes "/" in a path parameter's value — it does
// not send the literal, unencoded session name. app/internal/webapi's
// server tolerates both forms (transport_test.go), so this is safe, but
// web/api/README.md and routes/sessions.tsp's @doc must describe this
// actual behavior rather than the opposite.
assert.equal(capturedPath, "/api/v1/sessions/team%2Fworkspace-a");
console.log(
  "ok - the committed client.ts percent-encodes a slash-containing session name:",
  capturedPath,
);
