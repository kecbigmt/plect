import { useQuery } from "@tanstack/react-query";

import { fetchSessionDetail, fetchSessionList } from "@/lib/sessionsApi";

export function sessionDetailQueryKey(sessionName: string) {
  return ["session", sessionName] as const;
}

export function useSessionList() {
  return useQuery({
    queryKey: ["sessions"],
    queryFn: fetchSessionList,
    // TanStack Query's default retries + backoff can outlast a test's
    // findBy* timeout before isError ever turns true; matches
    // useBootstrap's own reasoning for disabling it.
    retry: false,
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
  });
}
