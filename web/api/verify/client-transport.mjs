#!/usr/bin/env node
// Exercises the same openapi-fetch call client.ts's
// createPlectureWebApiClient makes (a bare createClient({ baseUrl }); the
// wrapper adds no runtime logic of its own, so calling the library directly
// here is equivalent for what this script checks) against a captured
// fetch, to record what request path a real generated client actually
// sends for a slash-containing session name — rather than assuming one.
//
// This is the client-side half of
// app/internal/webapi/transport_test.go's server-side proof.
import assert from "node:assert/strict";
import createClient from "openapi-fetch";

let capturedPath = null;
const capturingFetch = async (input) => {
  const url = typeof input === "string" ? input : input.url;
  capturedPath = new URL(url).pathname;
  return new Response("{}", {
    status: 200,
    headers: { "content-type": "application/json" },
  });
};

const client = createClient({
  baseUrl: "http://example.invalid/api/v1",
  fetch: capturingFetch,
});
await client.GET("/sessions/{name}", {
  params: { path: { name: "team/workspace-a" } },
});

// openapi-fetch percent-encodes "/" in a path parameter's value — it does
// not send the literal, unencoded session name. app/internal/webapi's
// server tolerates both forms (transport_test.go), so this is safe, but
// web/api/README.md and routes/sessions.tsp's @doc must describe this
// actual behavior rather than the opposite.
assert.equal(capturedPath, "/api/v1/sessions/team%2Fworkspace-a");
console.log(
  "ok - the committed client percent-encodes a slash-containing session name:",
  capturedPath,
);
