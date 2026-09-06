import { createPlectureWebApiClient } from "@plecture/web-api/client";
import type { components } from "@plecture/web-api/generated/typescript/schema";

export type SessionEvent = components["schemas"]["Event"];
export type SessionEventOrder = components["schemas"]["EventOrder"];
export type SessionEventPage = components["schemas"]["EventPage"];

// Resolved to an absolute URL for the same reason sessionsApi.ts's baseUrl
// is: openapi-fetch constructs a Request directly, and Node/undici's
// Request (unlike a browser's) rejects a relative URL outright.
const baseUrl = new URL("/api/v1", window.location.origin).toString();

// Built fresh per call, not cached, matching sessionsApi.ts's own client —
// a cached instance would keep calling whatever globalThis.fetch was at
// construction time instead of a test's per-case stub.
function eventsClient() {
  return createPlectureWebApiClient(baseUrl);
}

export interface FetchEventPageOptions {
  cursor?: string;
  limit?: number;
  order?: SessionEventOrder;
}

export async function fetchEventPage(
  sessionName: string,
  options: FetchEventPageOptions = {},
): Promise<SessionEventPage> {
  const { data, error } = await eventsClient().GET("/events", {
    params: {
      query: {
        session: sessionName,
        cursor: options.cursor,
        limit: options.limit,
        order: options.order,
      },
    },
  });
  if (error) {
    throw new Error(error.message);
  }
  // openapi-fetch trusts the declared response type without validating the
  // parsed JSON shape (see sessionsApi.ts's fetchSessionList for the same
  // guard): a malformed 200 body must not crash the timeline downstream.
  if (!Array.isArray(data.events)) {
    throw new Error("malformed event page response: events is not an array");
  }
  return data;
}
