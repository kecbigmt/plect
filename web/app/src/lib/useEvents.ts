import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { useInfiniteQuery, useQueryClient } from "@tanstack/react-query";

import { fetchEventPage, type SessionEvent, type SessionEventPage } from "@/lib/eventsApi";
import { openEventStream, type EventStreamState } from "@/lib/eventStream";
import { sessionDetailQueryKey, sessionListQueryKey } from "@/lib/useSessions";

export function sessionEventsQueryKey(sessionName: string) {
  return ["events", sessionName] as const;
}

// Mirrors useSessions.ts's useSessionDetail: sessionName is null while
// nothing is selected (enabled: false skips the request), and retry is
// disabled since a session whose log doesn't exist yet answers with an
// empty page rather than an error — a real failure will not go away on
// retry either.
export function useSessionEvents(sessionName: string | null) {
  return useInfiniteQuery({
    queryKey: sessionEventsQueryKey(sessionName ?? ""),
    queryFn: ({ pageParam }) =>
      fetchEventPage(sessionName!, { cursor: pageParam, order: "asc" }),
    initialPageParam: undefined as string | undefined,
    // Following an already-empty page's cursor would refetch the same
    // caught-up tail forever.
    getNextPageParam: (lastPage) => (lastPage.events.length === 0 ? undefined : lastPage.nextCursor),
    enabled: sessionName !== null,
    retry: false,
  });
}

// IDs distinguish otherwise-identical records; history wins the overlap a
// live replay's own catch-up can re-deliver.
export function dedupeEventsById(
  pages: readonly SessionEventPage[] | undefined,
  liveEvents: readonly SessionEvent[] = [],
): SessionEvent[] {
  const byId = new Map<string, SessionEvent>();
  for (const page of pages ?? []) {
    for (const e of page.events) {
      if (!byId.has(e.id)) {
        byId.set(e.id, e);
      }
    }
  }
  for (const e of liveEvents) {
    if (!byId.has(e.id)) {
      byId.set(e.id, e);
    }
  }
  return [...byId.values()];
}

// resumeCursor is "" for a session with no log yet, a valid fresh-stream
// position, so historyReady is a separate signal rather than inferred from it.
export function useLiveEvents(sessionName: string | null, historyReady: boolean, resumeCursor: string) {
  const queryClient = useQueryClient();
  const [liveEvents, setLiveEvents] = useState<SessionEvent[]>([]);
  const [state, setState] = useState<EventStreamState>("connecting");
  const controllerRef = useRef<AbortController | null>(null);
  const mounted = useRef(false);

  // Passive cleanup would leave the obsolete connection active through the next commit.
  useLayoutEffect(() => {
    if (!mounted.current) {
      mounted.current = true;
      return;
    }
    controllerRef.current?.abort();
    setLiveEvents([]);
    setState("connecting");
  }, [sessionName]);

  useEffect(() => {
    if (sessionName === null || !historyReady) {
      return;
    }
    const controller = new AbortController();
    controllerRef.current = controller;
    openEventStream(
      sessionName,
      resumeCursor,
      {
        onEvent: (event) => {
          if (controller.signal.aborted) {
            return;
          }
          setLiveEvents((prev) => (prev.some((e) => e.id === event.id) ? prev : [...prev, event]));
          // Direct derivation would be incomplete: not every state change
          // emits an event.
          queryClient.invalidateQueries({ queryKey: sessionDetailQueryKey(sessionName) });
          queryClient.invalidateQueries({ queryKey: sessionListQueryKey() });
        },
        onStateChange: (s) => {
          if (!controller.signal.aborted) {
            setState(s);
          }
        },
      },
      controller.signal,
    );
    return () => controller.abort();
  }, [sessionName, historyReady, resumeCursor, queryClient]);

  return { liveEvents, state };
}
