import type { ReactNode } from "react";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("@/lib/eventStream", () => ({ openEventStream: vi.fn(), openAllSessionsEventStream: vi.fn() }));

import { openAllSessionsEventStream, openEventStream } from "@/lib/eventStream";
import type { SessionEvent } from "@/lib/eventsApi";
import { isLifecycleEvent, useLiveEvents, useSessionListLiveFacts } from "@/lib/useEvents";
import { sessionDetailQueryKey, sessionListQueryKey, useSessionList } from "@/lib/useSessions";

function makeWrapper() {
  const queryClient = new QueryClient();
  function wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  }
  return { queryClient, wrapper };
}

function stubEvent(overrides: Partial<SessionEvent> = {}): SessionEvent {
  return {
    id: "evt-1",
    sessionName: "team/a",
    type: "user.emit",
    source: "web",
    direction: "outbound",
    time: new Date().toISOString(),
    summary: "",
    ...overrides,
  } as SessionEvent;
}

afterEach(() => {
  vi.mocked(openEventStream).mockReset();
  vi.mocked(openAllSessionsEventStream).mockReset();
});

describe("isLifecycleEvent", () => {
  it.each([
    ["lifecycle.created", true],
    ["lifecycle.up", true],
    ["lifecycle.task_setup", true],
    ["plect.status_message", false],
    ["user.emit", false],
    ["plect.instruction", false],
    ["plect.judge.recorded", false],
  ])("%s -> %s", (type, want) => {
    expect(isLifecycleEvent(type)).toBe(want);
  });
});

describe("useLiveEvents", () => {
  it("aborts its connection on unmount", () => {
    const { wrapper } = makeWrapper();
    const { unmount } = renderHook(() => useLiveEvents("team/a", true, ""), { wrapper });

    const signal = vi.mocked(openEventStream).mock.calls[0][3];
    expect(signal.aborted).toBe(false);

    unmount();

    expect(signal.aborted).toBe(true);
  });

  it("does not invalidate the session list or detail on a conversational event", () => {
    const { queryClient, wrapper } = makeWrapper();
    const invalidateSpy = vi.spyOn(queryClient, "invalidateQueries");
    renderHook(() => useLiveEvents("team/a", true, ""), { wrapper });

    const handlers = vi.mocked(openEventStream).mock.calls[0][2];
    handlers.onEvent(stubEvent({ type: "user.emit" }));

    expect(invalidateSpy).not.toHaveBeenCalled();
  });

  it("invalidates the session detail (not the list) on a lifecycle event, after a debounce", () => {
    vi.useFakeTimers();
    try {
      const { queryClient, wrapper } = makeWrapper();
      const invalidateSpy = vi.spyOn(queryClient, "invalidateQueries");
      renderHook(() => useLiveEvents("team/a", true, ""), { wrapper });

      const handlers = vi.mocked(openEventStream).mock.calls[0][2];
      handlers.onEvent(stubEvent({ type: "lifecycle.up" }));

      // Still coalescing: no request yet.
      expect(invalidateSpy).not.toHaveBeenCalled();

      vi.runAllTimers();

      // The list has its own invalidation source (useSessionListLiveFacts,
      // below) so a selected session's own lifecycle event never schedules
      // a second, redundant list refetch here.
      expect(invalidateSpy).toHaveBeenCalledTimes(1);
      expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: sessionDetailQueryKey("team/a") });
    } finally {
      vi.useRealTimers();
    }
  });

  it("coalesces a burst of lifecycle events into a single invalidation", () => {
    vi.useFakeTimers();
    try {
      const { queryClient, wrapper } = makeWrapper();
      const invalidateSpy = vi.spyOn(queryClient, "invalidateQueries");
      renderHook(() => useLiveEvents("team/a", true, ""), { wrapper });

      const handlers = vi.mocked(openEventStream).mock.calls[0][2];
      handlers.onEvent(stubEvent({ id: "evt-1", type: "lifecycle.task_setup" }));
      vi.advanceTimersByTime(100);
      handlers.onEvent(stubEvent({ id: "evt-2", type: "lifecycle.task_setup" }));
      vi.advanceTimersByTime(100);
      handlers.onEvent(stubEvent({ id: "evt-3", type: "lifecycle.up" }));
      vi.runAllTimers();

      // One invalidateQueries call, not one per event.
      expect(invalidateSpy).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("patches the cached detail from a status-message event instead of invalidating", () => {
    const { queryClient, wrapper } = makeWrapper();
    queryClient.setQueryData(sessionDetailQueryKey("team/a"), {
      sessionName: "team/a",
      run: "up",
      resourceId: "",
      createdAt: "2026-01-01T00:00:00Z",
      workspaceDirExists: false,
    });
    const invalidateSpy = vi.spyOn(queryClient, "invalidateQueries");
    renderHook(() => useLiveEvents("team/a", true, ""), { wrapper });

    const handlers = vi.mocked(openEventStream).mock.calls[0][2];
    handlers.onEvent(
      stubEvent({
        type: "plect.status_message",
        summary: "reviewing",
        metadata: { text: "reviewing", cleared: "false", previous: "" },
        time: "2026-01-01T01:00:00Z",
      }),
    );

    expect(invalidateSpy).not.toHaveBeenCalled();
    expect(queryClient.getQueryData(sessionDetailQueryKey("team/a"))).toMatchObject({
      message: { text: "reviewing", updatedAt: "2026-01-01T01:00:00Z" },
    });
  });

  // A resume backlog or an actively narrating session can replay/emit many
  // status-message events in a row; none may reach the network once the
  // detail is already cached (the arrives-before-cache-exists case is its
  // own test below).
  it("issues zero session-list requests for a burst of 50 status-message events", async () => {
    const queryClient = new QueryClient();
    function wrapper({ children }: { children: ReactNode }) {
      return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
    }
    queryClient.setQueryData(sessionDetailQueryKey("team/a"), {
      sessionName: "team/a",
      run: "up",
      resourceId: "",
      createdAt: "2026-01-01T00:00:00Z",
      workspaceDirExists: false,
    });
    vi.stubGlobal("fetch", vi.fn());
    vi.mocked(fetch).mockImplementation(() =>
      Promise.resolve(new Response(JSON.stringify({ items: [], count: 0 }), { status: 200 })),
    );

    try {
      const list = renderHook(() => useSessionList(), { wrapper });
      await waitFor(() => expect(list.result.current.isSuccess).toBe(true));
      expect(fetch).toHaveBeenCalledTimes(1);

      renderHook(() => useLiveEvents("team/a", true, ""), { wrapper });
      const handlers = vi.mocked(openEventStream).mock.calls[0][2];
      for (let i = 0; i < 50; i++) {
        handlers.onEvent(
          stubEvent({
            id: `evt-${i}`,
            type: "plect.status_message",
            metadata: { text: `status ${i}`, cleared: "false", previous: "" },
          }),
        );
      }

      expect(fetch).toHaveBeenCalledTimes(1);
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("buffers a status-message event that arrives before the detail is cached, applying it once the detail resolves", () => {
    const { queryClient, wrapper } = makeWrapper();
    const invalidateSpy = vi.spyOn(queryClient, "invalidateQueries");
    renderHook(() => useLiveEvents("team/a", true, ""), { wrapper });

    const handlers = vi.mocked(openEventStream).mock.calls[0][2];
    handlers.onEvent(
      stubEvent({ type: "plect.status_message", metadata: { text: "hi", cleared: "false", previous: "" } }),
    );

    // No cached detail yet: nothing to patch, and no network fallback either.
    expect(invalidateSpy).not.toHaveBeenCalled();
    expect(queryClient.getQueryData(sessionDetailQueryKey("team/a"))).toBeUndefined();

    // The detail query's own fetch lands independently of the live stream.
    queryClient.setQueryData(sessionDetailQueryKey("team/a"), {
      sessionName: "team/a",
      run: "up",
      resourceId: "",
      createdAt: "2026-01-01T00:00:00Z",
      workspaceDirExists: false,
    });

    expect(invalidateSpy).not.toHaveBeenCalled();
    expect(queryClient.getQueryData(sessionDetailQueryKey("team/a"))).toMatchObject({
      message: { text: "hi" },
    });
  });

  it("keeps a pending debounced invalidation alive after unmount instead of dropping it", () => {
    vi.useFakeTimers();
    try {
      const { queryClient, wrapper } = makeWrapper();
      const invalidateSpy = vi.spyOn(queryClient, "invalidateQueries");
      const { unmount } = renderHook(() => useLiveEvents("team/a", true, ""), { wrapper });

      const handlers = vi.mocked(openEventStream).mock.calls[0][2];
      handlers.onEvent(stubEvent({ type: "lifecycle.up" }));
      expect(invalidateSpy).not.toHaveBeenCalled();

      unmount();
      // Unmounting must not cancel it outright...
      expect(invalidateSpy).not.toHaveBeenCalled();

      // ...it still fires on its own original schedule.
      vi.runAllTimers();
      expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: sessionDetailQueryKey("team/a") });
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps a pending debounced invalidation for the outgoing session alive when the selection changes", () => {
    vi.useFakeTimers();
    try {
      const { queryClient, wrapper } = makeWrapper();
      const invalidateSpy = vi.spyOn(queryClient, "invalidateQueries");
      const { rerender } = renderHook(
        ({ sessionName }: { sessionName: string }) => useLiveEvents(sessionName, true, ""),
        { wrapper, initialProps: { sessionName: "team/a" } },
      );

      const handlers = vi.mocked(openEventStream).mock.calls[0][2];
      handlers.onEvent(stubEvent({ type: "lifecycle.up" }));
      expect(invalidateSpy).not.toHaveBeenCalled();

      rerender({ sessionName: "team/b" });
      expect(invalidateSpy).not.toHaveBeenCalled();

      vi.runAllTimers();
      // Still keyed to "team/a", the session that actually observed the event.
      expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: sessionDetailQueryKey("team/a") });
    } finally {
      vi.useRealTimers();
    }
  });

  it("coalesces lifecycle events across rapid session switches into one invalidation, not one per hop", () => {
    vi.useFakeTimers();
    try {
      const { queryClient, wrapper } = makeWrapper();
      const invalidateSpy = vi.spyOn(queryClient, "invalidateQueries");
      const { rerender } = renderHook(
        ({ sessionName }: { sessionName: string }) => useLiveEvents(sessionName, true, ""),
        { wrapper, initialProps: { sessionName: "team/a" } },
      );
      vi.mocked(openEventStream).mock.calls[0][2].onEvent(stubEvent({ type: "lifecycle.up" }));

      rerender({ sessionName: "team/b" });
      vi.mocked(openEventStream).mock.calls[1][2].onEvent(stubEvent({ type: "lifecycle.up" }));

      rerender({ sessionName: "team/c" });
      vi.mocked(openEventStream).mock.calls[2][2].onEvent(stubEvent({ type: "lifecycle.up" }));

      expect(invalidateSpy).not.toHaveBeenCalled();
      vi.runAllTimers();

      // Only the last hop's debounce survives; the earlier two were superseded.
      expect(invalidateSpy).toHaveBeenCalledTimes(1);
      expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: sessionDetailQueryKey("team/c") });
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("useSessionListLiveFacts", () => {
  it("invalidates the list, coalesced, on a lifecycle event for any session", () => {
    vi.useFakeTimers();
    try {
      const { queryClient, wrapper } = makeWrapper();
      const invalidateSpy = vi.spyOn(queryClient, "invalidateQueries");
      renderHook(() => useSessionListLiveFacts(), { wrapper });

      const handlers = vi.mocked(openAllSessionsEventStream).mock.calls[0][0];
      handlers.onEvent(stubEvent({ sessionName: "team/z", type: "lifecycle.up" }));

      expect(invalidateSpy).not.toHaveBeenCalled();
      vi.runAllTimers();
      expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: sessionListQueryKey() });
    } finally {
      vi.useRealTimers();
    }
  });

  it("patches a non-selected session's cached list row from a status-message event, with no refetch", () => {
    const { queryClient, wrapper } = makeWrapper();
    queryClient.setQueryData(sessionListQueryKey(), [
      { sessionName: "team/z", displayStatus: "up", run: "up", resourceId: "" },
    ]);
    const invalidateSpy = vi.spyOn(queryClient, "invalidateQueries");
    renderHook(() => useSessionListLiveFacts(), { wrapper });

    const handlers = vi.mocked(openAllSessionsEventStream).mock.calls[0][0];
    handlers.onEvent(
      stubEvent({
        sessionName: "team/z",
        type: "plect.status_message",
        metadata: { text: "reviewing", cleared: "false", previous: "" },
      }),
    );

    expect(invalidateSpy).not.toHaveBeenCalled();
    const list = queryClient.getQueryData<{ sessionName: string; message?: { text: string } }[]>(
      sessionListQueryKey(),
    );
    expect(list?.[0].message).toMatchObject({ text: "reviewing" });
  });
});
