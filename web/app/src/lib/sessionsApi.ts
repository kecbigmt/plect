// Thin wrapper over the generated @plecture/web-api contract: this file owns
// the fetch calls and error mapping; src/lib/useSessions.ts owns the
// TanStack Query wiring, mirroring bootstrap.ts / useBootstrap.ts's split.
import { createPlectureWebApiClient } from "@plecture/web-api/client";
import type { components } from "@plecture/web-api/generated/typescript/schema";

export type SessionSummary = components["schemas"]["SessionSummary"];
export type SessionDetail = components["schemas"]["SessionDetail"];

// Resolved to an absolute URL, not passed as a bare "/api/v1": openapi-fetch
// constructs a Request directly, and Node/undici's Request (unlike a
// browser's) rejects a relative URL outright, with no document base to
// resolve it against.
const baseUrl = new URL("/api/v1", window.location.origin).toString();

// Built fresh per call rather than cached as a module-level singleton:
// createClient() captures whatever globalThis.fetch is at construction
// time, so a cached instance would keep calling a stale reference — a real
// difference for a test that stubs/restores globalThis.fetch per case, and
// a needless assumption to bake in for production either way.
function sessionsClient() {
  return createPlectureWebApiClient(baseUrl);
}

// Distinguished from a generic failure so a caller can show "not found"
// rather than a transient-looking error — retrying an unknown session name
// will not make it exist.
export class SessionNotFoundError extends Error {
  constructor(public readonly sessionName: string) {
    super(`session not found: ${sessionName}`);
    this.name = "SessionNotFoundError";
  }
}

export async function fetchSessionList(): Promise<SessionSummary[]> {
  const { data, error } = await sessionsClient().GET("/sessions");
  if (error) {
    throw new Error(error.message);
  }
  // openapi-fetch trusts the declared response type; it does not itself
  // validate the parsed JSON shape. A malformed 200 body (a non-conformant
  // server, or a proxy returning something else entirely) must not crash
  // tree-building downstream, so check the one thing that would.
  if (!Array.isArray(data.items)) {
    throw new Error("malformed session list response: items is not an array");
  }
  return data.items;
}

export async function fetchSessionDetail(sessionName: string): Promise<SessionDetail> {
  const { data, error, response } = await sessionsClient().GET("/sessions/{name}", {
    params: { path: { name: sessionName } },
  });
  if (error) {
    if (response.status === 404) {
      throw new SessionNotFoundError(sessionName);
    }
    throw new Error(error.message);
  }
  return data;
}
