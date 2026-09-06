import type { ReactNode } from "react";
import { renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("@/lib/eventStream", () => ({ openEventStream: vi.fn() }));

import { openEventStream } from "@/lib/eventStream";
import { useLiveEvents } from "@/lib/useEvents";

function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient();
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

afterEach(() => {
  vi.mocked(openEventStream).mockReset();
});

describe("useLiveEvents", () => {
  it("aborts its connection on unmount", () => {
    const { unmount } = renderHook(() => useLiveEvents("team/a", true, ""), { wrapper });

    const signal = vi.mocked(openEventStream).mock.calls[0][3];
    expect(signal.aborted).toBe(false);

    unmount();

    expect(signal.aborted).toBe(true);
  });
});
