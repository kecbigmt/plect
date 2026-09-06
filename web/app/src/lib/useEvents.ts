import { useInfiniteQuery } from "@tanstack/react-query";

import { fetchEventPage, type SessionEvent, type SessionEventPage } from "@/lib/eventsApi";

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
    // The read contract's ascending nextCursor is a forward log position,
    // not a snapshot (docs/design/web-ui-event-history.md): paginating
    // forward with it can only ever surface more of the same history, never
    // "older" events relative to what is already loaded.
    getNextPageParam: (lastPage) => lastPage.nextCursor,
    enabled: sessionName !== null,
    retry: false,
  });
}

// Flattens pages in fetch order, deduplicating by event ID rather than by
// content: an ascending page can hand back an event an earlier page already
// returned (a refetch replays the same forward position), but a genuinely
// distinct record — an origin event and the parent notification it caused —
// must never collapse just because their text happens to match.
export function dedupeEventsById(pages: readonly SessionEventPage[] | undefined): SessionEvent[] {
  if (!pages) {
    return [];
  }
  const byId = new Map<string, SessionEvent>();
  for (const page of pages) {
    for (const e of page.events) {
      if (!byId.has(e.id)) {
        byId.set(e.id, e);
      }
    }
  }
  return [...byId.values()];
}
