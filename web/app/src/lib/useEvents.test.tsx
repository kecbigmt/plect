import type { ReactNode } from "react";
import { act, renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("@/lib/eventStream", () => ({ openEventStream: vi.fn() }));

import { openEventStream, type EventStreamHandlers } from "@/lib/eventStream";
import { useLiveEvents } from "@/lib/useEvents";

function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient();
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

afterEach(() => {
  vi.mocked(openEventStream).mockReset();
});

describe("useLiveEvents", () => {
  it("ignores a callback from the previous session's connection once ownership has moved on, independent of that connection's own AbortSignal", () => {
    const captured: EventStreamHandlers[] = [];
    vi.mocked(openEventStream).mockImplementation((_sessionName, _cursor, handlers) => {
      captured.push(handlers);
    });

    const { result, rerender } = renderHook(({ sessionName }) => useLiveEvents(sessionName, true, ""), {
      initialProps: { sessionName: "team/a" },
      wrapper,
    });

    rerender({ sessionName: "team/b" });

    // Session A's own AbortSignal is never fired here (openEventStream is
    // mocked, so nothing calls it) — this isolates the ownership check from
    // abort timing, proving it alone rejects a callback from a superseded
    // connection.
    const aHandlers = captured[0];
    act(() => {
      aHandlers.onEvent({
        id: "late",
        sessionName: "team/a",
        time: "2026-01-01T00:00:00Z",
        type: "user.note",
        source: "cli",
        direction: "internal",
        summary: "late",
      });
      aHandlers.onStateChange("unavailable");
    });

    expect(result.current.liveEvents).toEqual([]);
    expect(result.current.state).not.toBe("unavailable");
  });
});
