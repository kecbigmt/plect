import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { SessionHeader } from "@/components/session/SessionHeader";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function renderHeader(sessionName: string, onOpenDetails = vi.fn()) {
  const queryClient = new QueryClient();
  return {
    onOpenDetails,
    ...render(
      <QueryClientProvider client={queryClient}>
        <SessionHeader sessionName={sessionName} onOpenDetails={onOpenDetails} />
      </QueryClientProvider>,
    ),
  };
}

beforeEach(() => {
  vi.stubGlobal("fetch", vi.fn());
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("SessionHeader", () => {
  it("shows the session name immediately, before the detail request resolves", () => {
    vi.mocked(fetch).mockReturnValue(new Promise(() => {}));
    renderHeader("team/a");
    expect(screen.getByRole("heading", { name: "team/a" })).toBeInTheDocument();
  });

  it("shows the resource once loaded, as a link when it is a web URL", async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({
        sessionName: "team/a",
        run: "up",
        resourceId: "https://example.com/issues/1",
        workspaceDirExists: true,
        createdAt: "2026-01-01T00:00:00Z",
      }),
    );
    renderHeader("team/a");
    const link = await screen.findByRole("link", { name: "https://example.com/issues/1" });
    expect(link).toHaveAttribute("href", "https://example.com/issues/1");
  });

  it("shows the real run and health values verbatim", async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({
        sessionName: "team/a",
        run: "up",
        health: "stalled",
        resourceId: "",
        workspaceDirExists: true,
        createdAt: "2026-01-01T00:00:00Z",
      }),
    );
    renderHeader("team/a");
    expect(await screen.findByText("up")).toBeInTheDocument();
    expect(screen.getByText("stalled")).toBeInTheDocument();
  });

  it("calls onOpenDetails when the Details control is activated", async () => {
    vi.mocked(fetch).mockReturnValue(new Promise(() => {}));
    const user = userEvent.setup();
    const { onOpenDetails } = renderHeader("team/a");
    await user.click(screen.getByRole("button", { name: /details/i }));
    expect(onOpenDetails).toHaveBeenCalled();
  });
});
