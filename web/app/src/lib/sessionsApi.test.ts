import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { SessionNotFoundError, fetchSessionDetail, fetchSessionList } from "@/lib/sessionsApi";

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

describe("fetchSessionList", () => {
  it("returns the items array on success", async () => {
    const items = [{ sessionName: "a", displayStatus: "up", run: "up", resourceId: "" }];
    vi.mocked(fetch).mockResolvedValue(jsonResponse({ items, count: 1 }));
    await expect(fetchSessionList()).resolves.toEqual(items);
  });

  it("requests the fixed /api/v1 prefix", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse({ items: [], count: 0 }));
    await fetchSessionList();
    expect(vi.mocked(fetch).mock.calls[0][0]).toMatchObject({ url: expect.stringContaining("/api/v1/sessions") });
  });

  it("throws on a 500 execution error", async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({ category: "execution", code: "execution_failed", message: "boom" }, 500),
    );
    await expect(fetchSessionList()).rejects.toThrow(/boom/);
  });

  it("throws rather than trusting a malformed 200 body with no items array", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse({ apiVersion: "v1" }));
    await expect(fetchSessionList()).rejects.toThrow(/items/);
  });
});

describe("fetchSessionDetail", () => {
  it("returns the session detail on success", async () => {
    const detail = { sessionName: "a", run: "up", workspaceDirExists: true, createdAt: "2026-01-01T00:00:00Z" };
    vi.mocked(fetch).mockResolvedValue(jsonResponse(detail));
    await expect(fetchSessionDetail("a")).resolves.toEqual(detail);
  });

  it("percent-encodes a session name containing /", async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({ sessionName: "team/a", run: "up", workspaceDirExists: true, createdAt: "2026-01-01T00:00:00Z" }),
    );
    await fetchSessionDetail("team/a");
    const request = vi.mocked(fetch).mock.calls[0][0] as Request;
    expect(request.url).toContain("team%2Fa");
  });

  it("throws SessionNotFoundError on 404", async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({ category: "not_found", code: "session_not_found", message: "no such session" }, 404),
    );
    const err = await fetchSessionDetail("missing").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(SessionNotFoundError);
    expect((err as SessionNotFoundError).sessionName).toBe("missing");
  });

  it("throws a plain error on a 500 execution error", async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({ category: "execution", code: "execution_failed", message: "boom" }, 500),
    );
    const err = await fetchSessionDetail("a").catch((e: unknown) => e);
    expect(err).not.toBeInstanceOf(SessionNotFoundError);
    expect((err as Error).message).toMatch(/boom/);
  });
});
