import { useQuery } from "@tanstack/react-query";

import { fetchSessionDetail, fetchSessionList } from "@/lib/sessionsApi";

export function sessionDetailQueryKey(sessionName: string) {
  return ["session", sessionName] as const;
}

export function sessionListQueryKey() {
  return ["sessions"] as const;
}

// Only useLiveEvents (useEvents.ts) marks either query stale; an implicit
// staleTime/focus/reconnect refetch would repeat the request on a schedule
// unrelated to whether anything actually changed.
const NO_IMPLICIT_REFETCH = {
  staleTime: Infinity,
  refetchOnWindowFocus: false,
  refetchOnReconnect: false,
} as const;

export function useSessionList() {
  return useQuery({
    queryKey: sessionListQueryKey(),
    queryFn: fetchSessionList,
    // TanStack Query's default retries + backoff can outlast a test's
    // findBy* timeout before isError ever turns true; matches
    // useBootstrap's own reasoning for disabling it.
    retry: false,
    ...NO_IMPLICIT_REFETCH,
    // Interim cross-session fallback (docs/design/web-ui.md, #488): nothing
    // yet tells this query about a session that isn't selected changing, so
    // it polls at a low rate instead — only while the tab is visible, so a
    // backgrounded tab doesn't keep polling for no one to see it.
    refetchInterval: 60_000,
    refetchIntervalInBackground: false,
  });
}

// sessionName is null while nothing is selected: `enabled: false` then skips
// the request entirely rather than fetching an arbitrary/previous session's
// detail. A distinct query key per session name is what keeps rapid
// switching correct — TanStack Query never lets an in-flight response for a
// since-abandoned name overwrite the now-selected one's cached data.
export function useSessionDetail(sessionName: string | null) {
  return useQuery({
    queryKey: sessionDetailQueryKey(sessionName ?? ""),
    queryFn: () => fetchSessionDetail(sessionName!),
    enabled: sessionName !== null,
    // A session that no longer exists will not start existing on retry, and
    // (matching useSessionList/useBootstrap) automatic retry+backoff would
    // only delay a genuine failure surfacing to the user.
    retry: false,
    ...NO_IMPLICIT_REFETCH,
  });
}
