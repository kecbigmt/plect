import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { Conversation } from "@/components/session/Conversation";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function renderConversation(sessionName: string, onSelectSession: (name: string) => void = vi.fn()) {
  const queryClient = new QueryClient();
  return render(
    <QueryClientProvider client={queryClient}>
      <Conversation sessionName={sessionName} onSelectSession={onSelectSession} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.stubGlobal("fetch", vi.fn());
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("Conversation", () => {
  it("shows a loading state while the first page is in flight", () => {
    vi.mocked(fetch).mockReturnValue(new Promise(() => {}));
    renderConversation("team/a");
    expect(screen.getByText(/loading/i)).toBeInTheDocument();
  });

  it("shows an error state when the history request fails", async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({ category: "execution", code: "execution_failed", message: "boom" }, 500),
    );
    renderConversation("team/a");
    expect(await screen.findByRole("alert")).toBeInTheDocument();
  });

  it("shows an empty state when the session has no recorded events", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse({ events: [] }));
    renderConversation("team/a");
    expect(await screen.findByText(/no events recorded/i)).toBeInTheDocument();
  });

  it("renders an inbound event with a body as a readable utterance", async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({
        events: [
          {
            id: "01",
            sessionName: "team/a",
            time: "2026-01-01T00:00:00Z",
            type: "user.emit",
            source: "cli",
            direction: "inbound",
            summary: "user message",
            body: "Please check the deploy status.",
          },
        ],
      }),
    );
    renderConversation("team/a");
    expect(await screen.findByText("Please check the deploy status.")).toBeInTheDocument();
  });

  it("renders an internal event with no body as a compact row showing type, source, and summary", async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({
        events: [
          {
            id: "01",
            sessionName: "team/a",
            time: "2026-01-01T00:00:00Z",
            type: "lifecycle.created",
            source: "plect",
            direction: "internal",
            summary: "session created",
          },
        ],
      }),
    );
    renderConversation("team/a");
    expect(await screen.findByText("session created")).toBeInTheDocument();
    expect(screen.getByText("lifecycle.created")).toBeInTheDocument();
  });

  it("keeps an unrecognized event type on the compact path with its metadata visible", async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({
        events: [
          {
            id: "01",
            sessionName: "team/a",
            time: "2026-01-01T00:00:00Z",
            type: "acme.custom_event",
            source: "acme-provider",
            direction: "internal",
            summary: "custom provider event",
            metadata: { widget_id: "w-42" },
          },
        ],
      }),
    );
    renderConversation("team/a");
    expect(await screen.findByText("custom provider event")).toBeInTheDocument();
    expect(screen.getByText("acme.custom_event")).toBeInTheDocument();
    expect(screen.getByText("w-42")).toBeInTheDocument();
  });

  it("distinguishes a notification's origin session from the receiving session and offers navigation to it", async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({
        events: [
          {
            id: "01",
            sessionName: "team/a",
            time: "2026-01-01T00:00:00Z",
            type: "plect.terminal.done",
            source: "plect",
            direction: "inbound",
            summary: "done (from team/a/child)",
            metadata: { origin_session: "team/a/child", relation: "child" },
          },
        ],
      }),
    );
    const onSelectSession = vi.fn();
    const user = userEvent.setup();
    renderConversation("team/a", onSelectSession);

    const originButton = await screen.findByRole("button", { name: "team/a/child" });
    expect(screen.getByText(/→ team\/a$/)).toBeInTheDocument();
    await user.click(originButton);
    expect(onSelectSession).toHaveBeenCalledWith("team/a/child");
  });

  it("renders a long body in full, and untrusted-looking content as literal text", async () => {
    const longBody = "line ".repeat(500) + "<img src=x onerror=alert(1)>";
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({
        events: [
          {
            id: "01",
            sessionName: "team/a",
            time: "2026-01-01T00:00:00Z",
            type: "user.emit",
            source: "cli",
            direction: "inbound",
            summary: "user message",
            body: longBody,
          },
        ],
      }),
    );
    renderConversation("team/a");
    expect(await screen.findByText(longBody)).toBeInTheDocument();
    expect(document.querySelector("img")).not.toBeInTheDocument();
  });

  it("loads the next page on demand and dedupes overlapping event IDs by ID", async () => {
    vi.mocked(fetch).mockImplementation((input: RequestInfo | URL) => {
      const url = String(input instanceof Request ? input.url : input);
      if (url.includes("cursor=cur-1")) {
        return Promise.resolve(
          jsonResponse({
            events: [
              { id: "01", sessionName: "team/a", time: "2026-01-01T00:00:01Z", type: "user.note", source: "cli", direction: "internal", summary: "first" },
              { id: "02", sessionName: "team/a", time: "2026-01-01T00:00:02Z", type: "user.note", source: "cli", direction: "internal", summary: "second" },
            ],
          }),
        );
      }
      return Promise.resolve(
        jsonResponse({
          events: [
            { id: "01", sessionName: "team/a", time: "2026-01-01T00:00:01Z", type: "user.note", source: "cli", direction: "internal", summary: "first" },
          ],
          nextCursor: "cur-1",
        }),
      );
    });
    const user = userEvent.setup();
    renderConversation("team/a");

    expect(await screen.findByText("first")).toBeInTheDocument();
    const loadMore = screen.getByRole("button", { name: /load more/i });
    await user.click(loadMore);

    expect(await screen.findByText("second")).toBeInTheDocument();
    expect(screen.getAllByText("first")).toHaveLength(1);
  });

  it("does not reset an already-rendered event when a new page loads", async () => {
    vi.mocked(fetch).mockImplementation((input: RequestInfo | URL) => {
      const url = String(input instanceof Request ? input.url : input);
      if (url.includes("cursor=cur-1")) {
        return Promise.resolve(
          jsonResponse({
            events: [
              { id: "02", sessionName: "team/a", time: "2026-01-01T00:00:02Z", type: "user.note", source: "cli", direction: "internal", summary: "second" },
            ],
          }),
        );
      }
      return Promise.resolve(
        jsonResponse({
          events: [
            { id: "01", sessionName: "team/a", time: "2026-01-01T00:00:01Z", type: "user.note", source: "cli", direction: "internal", summary: "first" },
          ],
          nextCursor: "cur-1",
        }),
      );
    });
    const user = userEvent.setup();
    renderConversation("team/a");

    const first = await screen.findByText("first");
    await user.click(screen.getByRole("button", { name: /load more/i }));
    await screen.findByText("second");

    expect(screen.getByText("first")).toBe(first);
  });
});
