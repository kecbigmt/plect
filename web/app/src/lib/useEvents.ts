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

// Fires every few seconds per active session, so it's applied from the
// event's own payload below rather than refetched.
const STATUS_MESSAGE_EVENT_TYPE = "plect.status_message";

// Returns whether a cached detail existed to patch; the caller buffers the
// event and retries otherwise, rather than losing it.
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

// Debounces a burst of lifecycle events into one refetch; never cleared by
// an effect's own teardown, so a quick session switch can't force it early.
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
  // A status-message event this session's detail couldn't patch yet.
  const pendingStatusPatchRef = useRef<SessionEvent | null>(null);

  useEffect(() => {
    if (sessionName === null || !historyReady) {
      return;
    }
    const session = sessionName;
    const controller = new AbortController();

    function scheduleInvalidate() {
      if (invalidateTimerRef.current !== null) {
        clearTimeout(invalidateTimerRef.current);
      }
      invalidateTimerRef.current = setTimeout(() => {
        invalidateTimerRef.current = null;
        queryClient.invalidateQueries({ queryKey: sessionDetailQueryKey(session) });
        queryClient.invalidateQueries({ queryKey: sessionListQueryKey() });
      }, INVALIDATE_COALESCE_MS);
    }

    // Applies a buffered status patch once this session's detail actually
    // has data, covering a fetch that was already in flight when it arrived.
    const unsubscribe = queryClient.getQueryCache().subscribe((cacheEvent) => {
      if (cacheEvent.type !== "updated" || cacheEvent.query.state.status !== "success") {
        return;
      }
      const [kind, name] = cacheEvent.query.queryKey;
      if (kind !== "session" || name !== session) {
        return;
      }
      const pending = pendingStatusPatchRef.current;
      if (pending === null) {
        return;
      }
      pendingStatusPatchRef.current = null;
      applyStatusMessagePatch(queryClient, session, pending);
    });

    openEventStream(
      sessionName,
      resumeCursor,
      {
        onEvent: (event) => {
          setLiveEvents((prev) => (prev.some((e) => e.id === event.id) ? prev : [...prev, event]));
          if (event.type === STATUS_MESSAGE_EVENT_TYPE) {
            if (!applyStatusMessagePatch(queryClient, session, event)) {
              pendingStatusPatchRef.current = event;
            }
            return;
          }
          if (!isLifecycleEvent(event.type)) {
            return;
          }
          scheduleInvalidate();
        },
        onStateChange: setState,
      },
      controller.signal,
    );
    return () => {
      controller.abort();
      unsubscribe();
      pendingStatusPatchRef.current = null;
    };
  }, [sessionName, historyReady, resumeCursor, queryClient]);

  return { liveEvents, state };
}

