import { useEffect, useState } from "react";
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
    // The read contract hands back nextCursor whenever the session's log
    // exists at all, independent of whether that particular page had any
    // events (docs/design/web-ui-event-history.md) — so a page that comes
    // back empty means "caught up for now," not "no cursor was issued".
    // Stopping there rather than following that cursor keeps Load more from
    // becoming a control that re-fetches the same empty tail forever;
    // useLiveEvents below is what picks up new events past this point.
    getNextPageParam: (lastPage) => (lastPage.events.length === 0 ? undefined : lastPage.nextCursor),
    enabled: sessionName !== null,
    retry: false,
  });
}

// Flattens history pages (in fetch order) and any live-stream events after
// them, deduplicating by event ID rather than by content: an ascending page
// can hand back an event an earlier page already returned, and the live
// stream's own replay-then-follow catch-up can re-deliver one a history page
// already showed — but a genuinely distinct record must never collapse just
// because its text happens to match. History wins a collision (it is
// iterated first).
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

// historyReady and resumeCursor are separate: resumeCursor is "" for a
// session with no durable log yet at all (EventPage omits nextCursor only in
// that case), and "" is itself a valid, meaningful resume position — a fresh
// connect — not a stand-in for "the history page hasn't loaded yet". A
// single conflated signal would leave such a session's live subscription
// never opening at all.
//
// A live event invalidates the session detail/list query keys rather than
// updating them directly, since not every state change emits an event.
export function useLiveEvents(sessionName: string | null, historyReady: boolean, resumeCursor: string) {
  const queryClient = useQueryClient();
  const [liveEvents, setLiveEvents] = useState<SessionEvent[]>([]);
  const [state, setState] = useState<EventStreamState>("connecting");
  const [liveEventsOwner, setLiveEventsOwner] = useState(sessionName);

  // Clears liveEvents during render, not in an effect, on a session change:
  // an effect runs after React has already committed (and painted) this
  // render with the previous session's stale events attached to the new
  // one. Calling a setter here instead makes React redo this render before
  // committing, so the browser never paints that intermediate frame.
  if (sessionName !== liveEventsOwner) {
    setLiveEventsOwner(sessionName);
    setLiveEvents([]);
  }

  useEffect(() => {
    if (sessionName === null || !historyReady) {
      return;
    }
    const controller = new AbortController();
    openEventStream(
      sessionName,
      resumeCursor,
      {
        onEvent: (event) => {
          setLiveEvents((prev) => (prev.some((e) => e.id === event.id) ? prev : [...prev, event]));
          queryClient.invalidateQueries({ queryKey: sessionDetailQueryKey(sessionName) });
          queryClient.invalidateQueries({ queryKey: sessionListQueryKey() });
        },
        onStateChange: setState,
      },
      controller.signal,
    );
    return () => controller.abort();
  }, [sessionName, historyReady, resumeCursor, queryClient]);

  return { liveEvents, state };
}
