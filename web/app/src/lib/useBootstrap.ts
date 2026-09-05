import { useQuery } from "@tanstack/react-query";

import { fetchBootstrap } from "@/lib/bootstrap";

export function useBootstrap() {
  return useQuery({
    queryKey: ["bootstrap"],
    queryFn: fetchBootstrap,
    // The unavailable state already offers an explicit Retry action, and
    // TanStack Query refetches on its own on window refocus/reconnect;
    // stacking automatic exponential-backoff retries on top of both would
    // only make a genuine failure slower to surface.
    retry: false,
  });
}
