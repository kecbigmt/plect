import { useEffect, useRef, useState } from "react";
import { useInfiniteQuery, useQueryClient, type QueryClient } from "@tanstack/react-query";

import { fetchEventPage, type SessionEvent, type SessionEventPage } from "@/lib/eventsApi";
import { openEventStream, type EventStreamState } from "@/lib/eventStream";
import type { SessionDetail } from "@/lib/sessionsApi";
import { sessionDetailQueryKey, sessionListQueryKey } from "@/lib/useSessions";

// Run/health are server-probed and only a lifecycle.* event means either changed.
const LIFECYCLE_EVENT_PREFIX = "lifecycle.";

export function isLifecycleEvent(type: string): boolean {
  return type.startsWith(LIFECYCLE_EVENT_PREFIX);
}

// Fires every few seconds per session, so it's applied from its own payload
// rather than refetched.
const STATUS_MESSAGE_EVENT_TYPE = "plect.status_message";

// True if a cached detail existed to patch; the caller invalidates otherwise.
function applyStatusMessagePatch(queryClient: QueryClient, sessionName: string, event: SessionEvent): boolean {
  let patched = false;
  queryClient.setQueryData(sessionDetailQueryKey(sessionName), (prev: SessionDetail | undefined) => {
    if (!prev) {
      return prev;
    }
    patched = true;
    const cleared = event.metadata?.cleared === "true";
    if (cleared) {
      return { ...prev, message: undefined };
    }
    return {
      ...prev,
      message: { text: event.metadata?.text ?? event.summary, updatedAt: event.time },
    };
  });
  return patched;
}

// Debounces lifecycle-event bursts into one refetch that survives teardown, so a quick switch can't force it early.
const INVALIDATE_COALESCE_MS = 300;

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
// The caller mounts one instance per session (keyed by sessionName), so a
// callback from a superseded connection can only reach an already-unmounted
// instance's state, never the newly selected session's.
export function useLiveEvents(sessionName: string | null, historyReady: boolean, resumeCursor: string) {
  const queryClient = useQueryClient();
  const [liveEvents, setLiveEvents] = useState<SessionEvent[]>([]);
  const [state, setState] = useState<EventStreamState>("connecting");
  const invalidateTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // OR'd so a status-only trigger can't drop a pending lifecycle trigger's list invalidation.
  const invalidateListTooRef = useRef(false);

  useEffect(() => {
    if (sessionName === null || !historyReady) {
      return;
    }
    const session = sessionName;
    const controller = new AbortController();

    function scheduleInvalidate(includeList: boolean) {
      invalidateListTooRef.current = invalidateListTooRef.current || includeList;
      if (invalidateTimerRef.current !== null) {
        clearTimeout(invalidateTimerRef.current);
      }
      invalidateTimerRef.current = setTimeout(() => {
        invalidateTimerRef.current = null;
        // invalidateQueries alone dedupes onto an in-flight fetch instead of
        // starting a fresh one, so a slow, stale-snapshotting request could
        // still win; cancelling it first forces a genuinely fresh fetch.
        queryClient.cancelQueries({ queryKey: sessionDetailQueryKey(session) });
        queryClient.invalidateQueries({ queryKey: sessionDetailQueryKey(session) });
        if (invalidateListTooRef.current) {
          queryClient.invalidateQueries({ queryKey: sessionListQueryKey() });
        }
        invalidateListTooRef.current = false;
      }, INVALIDATE_COALESCE_MS);
    }

    openEventStream(
      sessionName,
      resumeCursor,
      {
        onEvent: (event) => {
          setLiveEvents((prev) => (prev.some((e) => e.id === event.id) ? prev : [...prev, event]));
          if (event.type === STATUS_MESSAGE_EVENT_TYPE) {
            if (!applyStatusMessagePatch(queryClient, session, event)) {
              scheduleInvalidate(false);
            }
            return;
          }
          if (!isLifecycleEvent(event.type)) {
            return;
          }
          scheduleInvalidate(true);
        },
        onStateChange: setState,
      },
      controller.signal,
    );
    return () => controller.abort();
  }, [sessionName, historyReady, resumeCursor, queryClient]);

  return { liveEvents, state };
}

