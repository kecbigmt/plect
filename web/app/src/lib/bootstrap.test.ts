import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { BootstrapAuthError, IncompatibleApiError, fetchBootstrap } from "@/lib/bootstrap";

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

describe("fetchBootstrap", () => {
  it("resolves for the supported API version", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse({ apiVersion: "v1", csrfToken: "tok" }));
    await expect(fetchBootstrap()).resolves.toEqual({ apiVersion: "v1", csrfToken: "tok" });
  });

  it("throws BootstrapAuthError on 401", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse({}, 401));
    await expect(fetchBootstrap()).rejects.toBeInstanceOf(BootstrapAuthError);
  });

  it("rejects a newer/older apiVersion this build does not speak", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse({ apiVersion: "v2", csrfToken: "tok" }));
    const err = await fetchBootstrap().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(IncompatibleApiError);
    expect((err as IncompatibleApiError).reportedVersion).toBe("v2");
  });

  it("rejects a response missing apiVersion entirely", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse({ csrfToken: "tok" }));
    await expect(fetchBootstrap()).rejects.toBeInstanceOf(IncompatibleApiError);
  });

  it("rejects a response missing csrfToken", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse({ apiVersion: "v1" }));
    await expect(fetchBootstrap()).rejects.toBeInstanceOf(IncompatibleApiError);
  });

  it("rejects a malformed (non-object) body instead of trusting it", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse("not an object"));
    await expect(fetchBootstrap()).rejects.toBeInstanceOf(IncompatibleApiError);
  });

  it("rejects a non-string apiVersion", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse({ apiVersion: 1, csrfToken: "tok" }));
    await expect(fetchBootstrap()).rejects.toBeInstanceOf(IncompatibleApiError);
  });
});
