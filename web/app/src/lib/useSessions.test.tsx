import type { ReactNode } from "react";
import { renderHook, waitFor } from "@testing-library/react";
import { focusManager, onlineManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { useSessionDetail, useSessionList } from "@/lib/useSessions";

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

function makeWrapper() {
  const queryClient = new QueryClient();
  function wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }
  return { queryClient, wrapper };
}

beforeEach(() => {
  vi.stubGlobal("fetch", vi.fn());
  vi.mocked(fetch).mockImplementation(() => Promise.resolve(jsonResponse({ items: [], count: 0 })));
});

afterEach(() => {
  vi.unstubAllGlobals();
  focusManager.setFocused(undefined);
  onlineManager.setOnline(true);
});

// A stream-driven invalidation is the only thing that should ever re-fetch
// the list or a session's detail.
describe("useSessionList", () => {
  it("does not refetch on window refocus or network reconnect", async () => {
    const { wrapper } = makeWrapper();
    const { result } = renderHook(() => useSessionList(), { wrapper });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(fetch).toHaveBeenCalledTimes(1);

    focusManager.setFocused(false);
    focusManager.setFocused(true);
    onlineManager.setOnline(false);
    onlineManager.setOnline(true);

    // Give any (incorrectly) scheduled refetch a turn to start.
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(fetch).toHaveBeenCalledTimes(1);
  });

  it("refetches every 60s while visible, and not sooner", async () => {
    vi.useFakeTimers();
    try {
      const { wrapper } = makeWrapper();
      const { result } = renderHook(() => useSessionList(), { wrapper });

      await vi.waitFor(() => expect(result.current.isSuccess).toBe(true));
      expect(fetch).toHaveBeenCalledTimes(1);

      await vi.advanceTimersByTimeAsync(59_000);
      expect(fetch).toHaveBeenCalledTimes(1);

      await vi.advanceTimersByTimeAsync(1_000);
      expect(fetch).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("useSessionDetail", () => {
  it("does not refetch on window refocus or network reconnect", async () => {
    vi.mocked(fetch).mockImplementation(() =>
      Promise.resolve(jsonResponse({ sessionName: "team/a", run: "up", resourceId: "", createdAt: new Date().toISOString(), workspaceDirExists: false })),
    );
    const { wrapper } = makeWrapper();
    const { result } = renderHook(() => useSessionDetail("team/a"), { wrapper });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(fetch).toHaveBeenCalledTimes(1);

    focusManager.setFocused(false);
    focusManager.setFocused(true);
    onlineManager.setOnline(false);
    onlineManager.setOnline(true);

    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(fetch).toHaveBeenCalledTimes(1);
  });
});
