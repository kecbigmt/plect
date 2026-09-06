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
    // becoming a control that re-fetches the same empty tail forever; this
    // PR renders history pages only; a live subscription (#407) is what
    // picks up new events past this point.
    getNextPageParam: (lastPage) => (lastPage.events.length === 0 ? undefined : lastPage.nextCursor),
    enabled: sessionName !== null,
    retry: false,
  });
}

// Flattens history pages (in fetch order) and any live-stream events after
// them, deduplicating by event ID rather than by content: an ascending page
// can hand back an event an earlier page already returned (a refetch
// replays the same forward position), and the live stream's own replay-then-
// follow catch-up can re-deliver one a history page already showed (the
// history/live handoff protocol's documented overlap,
// docs/design/web-ui-event-history.md) — but a genuinely distinct record —
// an origin event and the parent notification it caused — must never
// collapse just because their text happens to match. History wins a
// collision (it is iterated first): a live redelivery of an already-shown
// event is simply dropped, not treated as a second occurrence.
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

// useLiveEvents opens the live subscription once the first history page has
// settled, per the history/live handoff protocol
// (docs/design/web-ui-event-history.md): that page's own nextCursor is the
// exact resume position, so no separate "wait until history is exhausted"
// step is needed before going live — anything between that position and now
// (whether genuinely new or merely not yet paged into view) arrives through
// this same stream, and dedupeEventsById reconciles it against whatever
// pages a manual "Load more" also fetched.
//
// A live event never updates this session's own detail/list rows directly
// (the issue's ratified decision): it invalidates those query keys so the
// next read re-derives status from the service layer, since not every state
// change emits an event.
export function useLiveEvents(sessionName: string | null, firstPageCursor: string | undefined) {
  const queryClient = useQueryClient();
  const [liveEvents, setLiveEvents] = useState<SessionEvent[]>([]);
  const [state, setState] = useState<EventStreamState>("connecting");

  useEffect(() => {
    if (sessionName === null || firstPageCursor === undefined) {
      return;
    }
    setLiveEvents([]);
    const controller = new AbortController();
    openEventStream(
      sessionName,
      firstPageCursor,
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
    // Switching sessions (a dependency change) or unmounting aborts the
    // in-flight connection — including a response already being read — so a
    // delayed frame for an abandoned session can never reach the new
    // session's timeline (connectOnce's reader.read() rejects/returns once
    // the signal fires, per eventStream.ts).
    return () => controller.abort();
  }, [sessionName, firstPageCursor, queryClient]);

  return { liveEvents, state };
}
