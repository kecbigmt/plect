import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { DetailPane } from "@/components/shell/DetailPane";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function renderDetail(sessionName: string | null) {
  const queryClient = new QueryClient();
  const utils = render(
    <QueryClientProvider client={queryClient}>
      <DetailPane sessionName={sessionName} />
    </QueryClientProvider>,
  );
  return { queryClient, ...utils };
}

beforeEach(() => {
  vi.stubGlobal("fetch", vi.fn());
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("DetailPane", () => {
  it("shows a placeholder when nothing is selected", () => {
    renderDetail(null);
    expect(screen.getByText(/nothing selected/i)).toBeInTheDocument();
  });

  it("shows a loading state while the detail request is in flight", () => {
    vi.mocked(fetch).mockReturnValue(new Promise(() => {}));
    renderDetail("team/a");
    expect(screen.getByText(/loading/i)).toBeInTheDocument();
  });

  it("shows a not-found state for a 404", async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({ category: "not_found", code: "session_not_found", message: "no such session" }, 404),
    );
    renderDetail("missing");
    expect(await screen.findByText(/not found/i)).toBeInTheDocument();
  });

  it("shows an error state for a server error", async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({ category: "execution", code: "execution_failed", message: "boom" }, 500),
    );
    renderDetail("team/a");
    expect(await screen.findByRole("alert")).toBeInTheDocument();
  });

  it("renders run, health, message, and resource verbatim, with no invented title", async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({
        sessionName: "team/a",
        run: "up",
        health: "stalled",
        resourceId: "https://example.com/issues/1",
        message: { text: "Reviewing changes", updatedAt: "2026-01-02T03:04:00Z" },
        workspaceDirExists: true,
        createdAt: "2026-01-01T00:00:00Z",
      }),
    );
    renderDetail("team/a");
    expect(await screen.findByText("up")).toBeInTheDocument();
    expect(screen.getByText("stalled")).toBeInTheDocument();
    expect(screen.getByText("Reviewing changes")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "https://example.com/issues/1" })).toBeInTheDocument();
    expect(screen.queryByText(/^title$/i)).not.toBeInTheDocument();
  });

  it("shows the recorded title only when the API actually sends one", async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({
        sessionName: "team/a",
        title: "Review the release",
        run: "down",
        resourceId: "",
        workspaceDirExists: false,
        createdAt: "2026-01-01T00:00:00Z",
      }),
    );
    renderDetail("team/a");
    expect(await screen.findByText("Review the release")).toBeInTheDocument();
  });

  it("does not show a previous session's detail while a newly selected session's request is in flight", async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({
        sessionName: "team/a",
        run: "up",
        resourceId: "",
        workspaceDirExists: true,
        createdAt: "2026-01-01T00:00:00Z",
      }),
    );
    const { rerender, queryClient } = renderDetail("team/a");
    await screen.findByText("team/a");

    vi.mocked(fetch).mockReturnValue(new Promise(() => {}));
    rerender(
      <QueryClientProvider client={queryClient}>
        <DetailPane sessionName="team/b" />
      </QueryClientProvider>,
    );
    expect(screen.queryByText("team/a")).not.toBeInTheDocument();
    expect(screen.getByText(/loading/i)).toBeInTheDocument();
  });

  it("does not let a late response for an abandoned selection override the currently selected session", async () => {
    let resolveTeamA!: (res: Response) => void;
    vi.mocked(fetch).mockImplementation(
      () => new Promise<Response>((resolve) => (resolveTeamA = resolve)),
    );
    const { rerender, queryClient } = renderDetail("team/a");
    await screen.findByText(/loading/i);

    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({
        sessionName: "team/b",
        run: "down",
        resourceId: "",
        workspaceDirExists: false,
        createdAt: "2026-01-01T00:00:00Z",
      }),
    );
    rerender(
      <QueryClientProvider client={queryClient}>
        <DetailPane sessionName="team/b" />
      </QueryClientProvider>,
    );
    await screen.findByText("team/b");

    resolveTeamA(
      jsonResponse({
        sessionName: "team/a",
        run: "up",
        resourceId: "",
        workspaceDirExists: true,
        createdAt: "2026-01-01T00:00:00Z",
      }),
    );
    await new Promise((r) => setTimeout(r, 0));
    expect(screen.getByText("team/b")).toBeInTheDocument();
    expect(screen.queryByText("team/a")).not.toBeInTheDocument();
  });
});
