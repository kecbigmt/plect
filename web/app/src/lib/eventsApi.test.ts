import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { fetchEventPage } from "@/lib/eventsApi";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

beforeEach(() => {
  vi.stubGlobal("fetch", vi.fn());
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("fetchEventPage", () => {
  it("returns the page on success", async () => {
    const events = [
      {
        id: "01",
        sessionName: "team/a",
        time: "2026-01-01T00:00:00Z",
        type: "user.note",
        source: "cli",
        direction: "internal",
        summary: "first",
      },
    ];
    vi.mocked(fetch).mockResolvedValue(jsonResponse({ events, nextCursor: "cur-1" }));
    await expect(fetchEventPage("team/a")).resolves.toEqual({ events, nextCursor: "cur-1" });
  });

  it("requests the fixed /api/v1/events path with the session as a query value", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse({ events: [] }));
    await fetchEventPage("team/a");
    const request = vi.mocked(fetch).mock.calls[0][0] as Request;
    expect(request.url).toContain("/api/v1/events");
    expect(request.url).toContain("session=team%2Fa");
  });

  it("passes cursor and order through as query values", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse({ events: [] }));
    await fetchEventPage("team/a", { cursor: "cur-1", order: "asc", limit: 50 });
    const request = vi.mocked(fetch).mock.calls[0][0] as Request;
    expect(request.url).toContain("cursor=cur-1");
    expect(request.url).toContain("order=asc");
    expect(request.url).toContain("limit=50");
  });

  it("throws on a 500 execution error", async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({ category: "execution", code: "execution_failed", message: "boom" }, 500),
    );
    await expect(fetchEventPage("team/a")).rejects.toThrow(/boom/);
  });

  it("throws rather than trusting a malformed 200 body with no events array", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse({ nextCursor: "cur-1" }));
    await expect(fetchEventPage("team/a")).rejects.toThrow(/events/);
  });
});
