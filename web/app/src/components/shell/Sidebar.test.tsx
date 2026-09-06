import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { Sidebar } from "@/components/shell/Sidebar";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function renderSidebar(
  props: Partial<React.ComponentProps<typeof Sidebar>> = {},
  queryClient = new QueryClient(),
) {
  const scrollTopRef = props.scrollTopRef ?? { current: 0 };
  const utils = render(
    <QueryClientProvider client={queryClient}>
      <Sidebar
        selectedName={null}
        expandedNames={new Set()}
        query=""
        onQueryChange={vi.fn()}
        onToggleExpanded={vi.fn()}
        onSelect={vi.fn()}
        scrollTopRef={scrollTopRef}
        {...props}
      />
    </QueryClientProvider>,
  );
  return { ...utils, queryClient, scrollTopRef };
}

beforeEach(() => {
  vi.stubGlobal("fetch", vi.fn());
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("Sidebar", () => {
  it("shows a loading state before the session list resolves", () => {
    vi.mocked(fetch).mockReturnValue(new Promise(() => {}));
    renderSidebar();
    expect(screen.getByText(/loading sessions/i)).toBeInTheDocument();
  });

  it("shows an error state when the session list request fails", async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({ category: "execution", code: "execution_failed", message: "boom" }, 500),
    );
    renderSidebar();
    await screen.findByText(/couldn.t load sessions/i);
  });

  it("renders the tree once the session list resolves", async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({
        items: [{ sessionName: "release", displayStatus: "up", run: "up", resourceId: "" }],
        count: 1,
      }),
    );
    renderSidebar();
    expect(await screen.findByRole("treeitem", { name: /release/i })).toBeInTheDocument();
  });

  it("wires the search box to the query prop", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse({ items: [], count: 0 }));
    renderSidebar({ query: "abc" });
    await waitFor(() =>
      expect(screen.getByRole("searchbox", { name: /search sessions/i })).toHaveValue("abc"),
    );
  });

  it("records the scroll offset into scrollTopRef as the user scrolls", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse({ items: [], count: 0 }));
    const { scrollTopRef } = renderSidebar();
    const region = await screen.findByRole("region", { name: /session list/i });
    fireEvent.scroll(region, { target: { scrollTop: 140 } });
    expect(scrollTopRef.current).toBe(140);
  });

  it("restores a previously recorded scroll offset once remounted with the same ref", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse({ items: [], count: 0 }));
    const queryClient = new QueryClient();
    const scrollTopRef = { current: 0 };

    const first = renderSidebar({ scrollTopRef }, queryClient);
    const firstRegion = await screen.findByRole("region", { name: /session list/i });
    fireEvent.scroll(firstRegion, { target: { scrollTop: 90 } });
    expect(scrollTopRef.current).toBe(90);
    first.unmount(); // e.g. the narrow-layout sidebar Sheet closing

    renderSidebar({ scrollTopRef }, queryClient); // e.g. the Sheet reopening
    const secondRegion = await screen.findByRole("region", { name: /session list/i });
    await waitFor(() => expect(secondRegion.scrollTop).toBe(90));
  });
});
