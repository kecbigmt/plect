import type { ReactNode } from "react";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("@/lib/eventStream", () => ({ openEventStream: vi.fn() }));

import { openEventStream } from "@/lib/eventStream";
import type { SessionEvent } from "@/lib/eventsApi";
import { isLifecycleEvent, useLiveEvents } from "@/lib/useEvents";
import { sessionDetailQueryKey, sessionListQueryKey, useSessionDetail, useSessionList } from "@/lib/useSessions";

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

  it("invalidates the session list and detail on a lifecycle event, after a debounce", () => {
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

      expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: sessionDetailQueryKey("team/a") });
      expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: sessionListQueryKey() });
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

      // One invalidateQueries call per key (detail, list), not one per event.
      expect(invalidateSpy).toHaveBeenCalledTimes(2);
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

  it("schedules exactly one coalesced detail refetch, no list refetch, for a status-message event with no cached detail", async () => {
    const queryClient = new QueryClient();
    function wrapper({ children }: { children: ReactNode }) {
      return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
    }
    const invalidateSpy = vi.spyOn(queryClient, "invalidateQueries");
    vi.stubGlobal("fetch", vi.fn());
    vi.mocked(fetch).mockImplementation(() =>
      Promise.resolve(
        new Response(
          JSON.stringify({
            sessionName: "team/a",
            run: "up",
            resourceId: "",
            createdAt: "2026-01-01T00:00:00Z",
            workspaceDirExists: false,
          }),
          { status: 200 },
        ),
      ),
    );

    try {
      const detail = renderHook(() => useSessionDetail("team/a"), { wrapper });
      renderHook(() => useLiveEvents("team/a", true, ""), { wrapper });

      const handlers = vi.mocked(openEventStream).mock.calls[0][2];
      handlers.onEvent(
        stubEvent({ type: "plect.status_message", metadata: { text: "hi", cleared: "false", previous: "" } }),
      );

      await waitFor(() => expect(detail.result.current.isSuccess).toBe(true));
      const fetchesSoFar = vi.mocked(fetch).mock.calls.length;

      await waitFor(() => expect(vi.mocked(fetch).mock.calls.length).toBeGreaterThan(fetchesSoFar));

      expect(fetch).toHaveBeenCalledTimes(fetchesSoFar + 1);
      expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: sessionDetailQueryKey("team/a") });
      expect(invalidateSpy).not.toHaveBeenCalledWith({ queryKey: sessionListQueryKey() });
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("does not let a slow initial detail fetch's stale snapshot win over a status-message event", async () => {
    vi.useFakeTimers();
    const queryClient = new QueryClient();
    function wrapper({ children }: { children: ReactNode }) {
      return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
    }
    const baseBody = {
      sessionName: "team/a",
      run: "up",
      resourceId: "",
      createdAt: "2026-01-01T00:00:00Z",
      workspaceDirExists: false,
    };
    // The stale response, snapshotted before the event, carries no message;
    // the fresh one, snapshotted after, does — only the fresh one may win.
    const staleBody = baseBody;
    const freshBody = { ...baseBody, message: { text: "hi", updatedAt: "2026-01-01T00:00:01Z" } };
    vi.stubGlobal("fetch", vi.fn());
    let resolveFirst: (value: Response) => void = () => {};
    vi.mocked(fetch)
      .mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveFirst = resolve;
          }),
      )
      .mockImplementation(() => Promise.resolve(new Response(JSON.stringify(freshBody), { status: 200 })));

    try {
      const detail = renderHook(() => useSessionDetail("team/a"), { wrapper });
      renderHook(() => useLiveEvents("team/a", true, ""), { wrapper });

      const handlers = vi.mocked(openEventStream).mock.calls[0][2];
      handlers.onEvent(
        stubEvent({ type: "plect.status_message", metadata: { text: "hi", cleared: "false", previous: "" } }),
      );

      // The coalesced invalidation fires while the very first fetch — its
      // snapshot taken before the event — is still unresolved.
      await vi.advanceTimersByTimeAsync(400);
      expect(fetch).toHaveBeenCalledTimes(2);

      resolveFirst(new Response(JSON.stringify(staleBody), { status: 200 }));
      await vi.waitFor(() => expect(detail.result.current.isSuccess).toBe(true));

      expect(detail.result.current.data).toMatchObject({ message: { text: "hi" } });
    } finally {
      vi.useRealTimers();
      vi.unstubAllGlobals();
    }
  });

  // A resume backlog or an actively narrating session can replay/emit many
  // status-message events in a row; none may reach the network.
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
      expect(invalidateSpy).not.toHaveBeenCalled();

      vi.runAllTimers();
      expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: sessionDetailQueryKey("team/a") });
      expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: sessionListQueryKey() });
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
      expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: sessionListQueryKey() });
    } finally {
      vi.useRealTimers();
    }
  });

  // Conversation.tsx mounts this hook keyed by sessionName, so switching
  // sessions unmounts the old instance and mounts a brand new one — each
  // gets its own timer ref, unlike a rerender of the same instance.
  it("keeps each keyed instance's pending invalidation independent across rapid session switches", () => {
    vi.useFakeTimers();
    try {
      const { queryClient, wrapper } = makeWrapper();
      const invalidateSpy = vi.spyOn(queryClient, "invalidateQueries");

      const a = renderHook(() => useLiveEvents("team/a", true, ""), { wrapper });
      vi.mocked(openEventStream).mock.calls[0][2].onEvent(stubEvent({ type: "lifecycle.up" }));
      a.unmount();

      const b = renderHook(() => useLiveEvents("team/b", true, ""), { wrapper });
      vi.mocked(openEventStream).mock.calls[1][2].onEvent(stubEvent({ type: "lifecycle.up" }));
      b.unmount();

      renderHook(() => useLiveEvents("team/c", true, ""), { wrapper });
      vi.mocked(openEventStream).mock.calls[2][2].onEvent(stubEvent({ type: "lifecycle.up" }));

      expect(invalidateSpy).not.toHaveBeenCalled();
      vi.runAllTimers();

      expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: sessionDetailQueryKey("team/a") });
      expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: sessionDetailQueryKey("team/b") });
      expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: sessionDetailQueryKey("team/c") });
      expect(invalidateSpy).toHaveBeenCalledTimes(6);
    } finally {
      vi.useRealTimers();
    }
  });
});
