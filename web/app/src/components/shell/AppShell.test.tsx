import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { AppShell } from "@/components/shell/AppShell";

const bootstrap = { apiVersion: "v1", csrfToken: "tok-1" };

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function sessionListBody(items: unknown[]) {
  return { items, count: items.length };
}

// A Response body can only be read once; AppShell and Sidebar both query
// the session list, and (unlike a single simultaneous mount, which
// TanStack Query dedupes into one request) a later mount — e.g. opening
// the narrow-layout sidebar Sheet after the initial render already fetched
// — issues its own request. mockResolvedValue would hand out the very same
// already-consumed Response a second time, so every fetch call here gets a
// fresh one instead.
function mockFetchJson(body: unknown, status = 200) {
  vi.mocked(fetch).mockImplementation(() => Promise.resolve(jsonResponse(body, status)));
}

function stubMatchMedia(matches: boolean) {
  vi.stubGlobal(
    "matchMedia",
    vi.fn().mockReturnValue({
      matches,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    }),
  );
}

function renderShell() {
  const queryClient = new QueryClient();
  return render(
    <QueryClientProvider client={queryClient}>
      <AppShell bootstrap={bootstrap} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.stubGlobal("fetch", vi.fn());
  mockFetchJson(sessionListBody([]));
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("AppShell wide layout", () => {
  beforeEach(() => stubMatchMedia(true));

  it("keeps the sidebar as a persistent column, not an overlay trigger", () => {
    renderShell();
    expect(screen.getByRole("navigation", { name: /sessions/i })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /open sessions/i })).not.toBeInTheDocument();
  });

  it("opens the detail pane as a persistent column on toggle", async () => {
    const user = userEvent.setup();
    renderShell();
    expect(screen.queryByRole("complementary", { name: /details/i })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /toggle details/i }));
    expect(screen.getByRole("complementary", { name: /details/i })).toBeInTheDocument();
  });
});

describe("AppShell narrow layout", () => {
  beforeEach(() => stubMatchMedia(false));

  it("hides the sidebar behind an overlay trigger instead of a persistent column", () => {
    renderShell();
    expect(screen.queryByRole("navigation", { name: /sessions/i })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /open sessions/i })).toBeInTheDocument();
  });

  it("opens the sidebar overlay from its trigger", async () => {
    const user = userEvent.setup();
    renderShell();
    await user.click(screen.getByRole("button", { name: /open sessions/i }));
    expect(await screen.findByRole("navigation", { name: /sessions/i })).toBeInTheDocument();
  });

  it("closes the sidebar overlay once a session is selected from it", async () => {
    mockFetchJson(sessionListBody([{ sessionName: "team/a", displayStatus: "up", run: "up", resourceId: "" }]));
    const user = userEvent.setup();
    renderShell();
    await user.click(screen.getByRole("button", { name: /open sessions/i }));
    await user.click(await screen.findByRole("treeitem", { name: /team\/a/i }));
    expect(screen.queryByRole("navigation", { name: /sessions/i })).not.toBeInTheDocument();
    expect(await screen.findByRole("heading", { name: "team/a" })).toBeInTheDocument();
  });
});

describe("AppShell session selection", () => {
  beforeEach(() => stubMatchMedia(true));

  it("shows a placeholder in the main area until a session is selected", async () => {
    mockFetchJson(sessionListBody([{ sessionName: "team/a", displayStatus: "up", run: "up", resourceId: "" }]));
    renderShell();
    expect(screen.getByText(/select a session/i)).toBeInTheDocument();
    await screen.findByRole("treeitem", { name: /team\/a/i });
  });

  it("selecting a session shows its name in the header and in the detail pane, without leaking a previous session's detail", async () => {
    vi.mocked(fetch).mockImplementation((input: RequestInfo | URL) => {
      const url = String(input instanceof Request ? input.url : input);
      if (url.includes("/sessions/team%2Fb")) {
        return new Promise(() => {}); // never resolves, to prove no stale detail leaks in
      }
      if (url.includes("/sessions/team%2Fa")) {
        return Promise.resolve(
          jsonResponse({
            sessionName: "team/a",
            run: "up",
            resourceId: "",
            workspaceDirExists: true,
            createdAt: "2026-01-01T00:00:00Z",
          }),
        );
      }
      return Promise.resolve(
        jsonResponse(
          sessionListBody([
            { sessionName: "team/a", displayStatus: "up", run: "up", resourceId: "" },
            { sessionName: "team/b", displayStatus: "up", run: "up", resourceId: "" },
          ]),
        ),
      );
    });
    const user = userEvent.setup();
    renderShell();

    await user.click(await screen.findByRole("treeitem", { name: /^team\/a$/ }));
    expect(await screen.findByRole("heading", { name: "team/a" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /toggle details/i }));
    expect(await screen.findByText("team/a", { selector: "h2" })).toBeInTheDocument();

    await user.click(screen.getByRole("treeitem", { name: /^team\/b$/ }));
    expect(screen.getByRole("heading", { name: "team/b" })).toBeInTheDocument();
    // team/b's own detail request never resolves; the pane must show its own
    // loading state, not team/a's now-stale detail.
    expect(screen.queryByText("team/a", { selector: "h2" })).not.toBeInTheDocument();
    expect(screen.getByText(/loading/i)).toBeInTheDocument();
  });

  it("expands a session's ancestors when it is selected, so it stays visible after a search that revealed it is cleared", async () => {
    mockFetchJson(
      sessionListBody([
        { sessionName: "release", displayStatus: "up", run: "up", resourceId: "" },
        {
          sessionName: "release/config",
          displayStatus: "up",
          run: "up",
          resourceId: "",
          parentSession: "release",
        },
      ]),
    );
    const user = userEvent.setup();
    renderShell();

    // release/config starts hidden: release is collapsed by default.
    await screen.findByRole("treeitem", { name: /^release$/ });
    expect(screen.queryByRole("treeitem", { name: /release\/config$/ })).not.toBeInTheDocument();

    // A search reveals it (force-expanding its ancestor), independent of
    // the real expandedNames state.
    await user.type(screen.getByRole("searchbox", { name: /search sessions/i }), "config");
    await user.click(await screen.findByRole("treeitem", { name: /release\/config$/ }));

    // Selecting it is what should have durably expanded "release" — clearing
    // the search now falls back to real expandedNames, not the search's
    // transient force-expansion.
    await user.clear(screen.getByRole("searchbox", { name: /search sessions/i }));
    expect(screen.getByRole("treeitem", { name: /release\/config$/ })).toBeInTheDocument();
  });
});
