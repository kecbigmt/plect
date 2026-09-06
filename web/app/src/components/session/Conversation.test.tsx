import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { Conversation } from "@/components/session/Conversation";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function sseResponse(frames: string): Response {
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(new TextEncoder().encode(frames));
      controller.close();
    },
  });
  return new Response(stream, { status: 200 });
}

function requestUrl(input: RequestInfo | URL): URL {
  return new URL(String(input instanceof Request ? input.url : input), "http://localhost");
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

  it("renders a user.emit event with a body as a readable utterance", async () => {
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
    const { container } = renderConversation("team/a");
    expect(await screen.findByText("Please check the deploy status.")).toBeInTheDocument();
    expect(container.querySelector('[data-event-style="utterance"]')).toBeInTheDocument();
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

  it("keeps an unrecognized inbound event with a body on the compact path, with its type and summary still visible", async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({
        events: [
          {
            id: "01",
            sessionName: "team/a",
            time: "2026-01-01T00:00:00Z",
            type: "acme.chat_message",
            source: "acme-provider",
            direction: "inbound",
            summary: "acme message received",
            body: "Hello from acme",
          },
        ],
      }),
    );
    const { container } = renderConversation("team/a");
    expect(await screen.findByText("Hello from acme")).toBeInTheDocument();
    expect(screen.getByText("acme.chat_message")).toBeInTheDocument();
    expect(screen.getByText("acme message received")).toBeInTheDocument();
    expect(container.querySelector('[data-event-style="compact"]')).toBeInTheDocument();
    expect(container.querySelector('[data-event-style="utterance"]')).not.toBeInTheDocument();
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

  it("stops offering Load more once a page comes back empty, even though the server still returns a cursor", async () => {
    // The read contract hands back nextCursor whenever the log exists at
    // all, independent of whether that page had any events — so a
    // caught-up tail still carries one. Following it anyway would turn
    // Load more into a control that never terminates.
    vi.mocked(fetch).mockImplementation((input: RequestInfo | URL) => {
      const url = String(input instanceof Request ? input.url : input);
      if (url.includes("cursor=cur-1")) {
        return Promise.resolve(jsonResponse({ events: [], nextCursor: "cur-1" }));
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

    await screen.findByText("first");
    await user.click(screen.getByRole("button", { name: /load more/i }));

    await waitFor(() =>
      expect(screen.queryByRole("button", { name: /load more/i })).not.toBeInTheDocument(),
    );
  });

  it("restores a session's own scroll position after switching away and back", async () => {
    // A Response body can only be read once; mockResolvedValue would hand
    // out the same already-consumed Response for every one of the three
    // fetches this test triggers, so each call gets a fresh one instead.
    vi.mocked(fetch).mockImplementation(() => Promise.resolve(jsonResponse({ events: [] })));
    const queryClient = new QueryClient();
    const { rerender } = render(
      <QueryClientProvider client={queryClient}>
        <Conversation sessionName="team/a" onSelectSession={vi.fn()} />
      </QueryClientProvider>,
    );

    const region = await screen.findByRole("region", { name: /conversation/i });
    fireEvent.scroll(region, { target: { scrollTop: 120 } });

    rerender(
      <QueryClientProvider client={queryClient}>
        <Conversation sessionName="team/b" onSelectSession={vi.fn()} />
      </QueryClientProvider>,
    );
    await screen.findByRole("region", { name: /conversation/i });

    rerender(
      <QueryClientProvider client={queryClient}>
        <Conversation sessionName="team/a" onSelectSession={vi.fn()} />
      </QueryClientProvider>,
    );
    const restoredRegion = await screen.findByRole("region", { name: /conversation/i });
    expect(restoredRegion.scrollTop).toBe(120);
  });

  it("shows an event delivered via the live stream, deduplicated against history by ID", async () => {
    vi.mocked(fetch).mockImplementation((input) => {
      if (requestUrl(input).pathname.endsWith("/events/stream")) {
        return Promise.resolve(
          sseResponse(
            'id: cur-2\ndata: {"id":"01","sessionName":"team/a","time":"2026-01-01T00:00:01Z","type":"user.note","source":"cli","direction":"internal","summary":"first"}\n\n' +
              'id: cur-3\ndata: {"id":"02","sessionName":"team/a","time":"2026-01-01T00:00:02Z","type":"user.note","source":"cli","direction":"internal","summary":"live-arrived"}\n\n',
          ),
        );
      }
      return Promise.resolve(
        jsonResponse({
          events: [
            {
              id: "01",
              sessionName: "team/a",
              time: "2026-01-01T00:00:01Z",
              type: "user.note",
              source: "cli",
              direction: "internal",
              summary: "first",
            },
          ],
          nextCursor: "cur-1",
        }),
      );
    });
    renderConversation("team/a");

    expect(await screen.findByText("first")).toBeInTheDocument();
    expect(await screen.findByText("live-arrived")).toBeInTheDocument();
    // The stream's own replay-then-follow catch-up re-delivered "first"
    // (same ID history already showed) — it must not render twice.
    expect(screen.getAllByText("first")).toHaveLength(1);
  });

  it("cancels the previous session's stream on switch, so a delayed frame never enters the new session's timeline", async () => {
    let pushLate!: (text: string) => void;
    const teamAStream = new ReadableStream<Uint8Array>({
      start(controller) {
        // Nothing enqueued yet: team/a's stream stays open (in flight) until
        // the test pushes a frame explicitly, after the switch to team/b.
        pushLate = (text) => controller.enqueue(new TextEncoder().encode(text));
      },
    });

    vi.mocked(fetch).mockImplementation((input) => {
      const url = requestUrl(input);
      const session = url.searchParams.get("session");
      if (url.pathname.endsWith("/events/stream")) {
        return Promise.resolve(session === "team/a" ? new Response(teamAStream, { status: 200 }) : sseResponse(""));
      }
      return Promise.resolve(
        jsonResponse({
          events: [
            {
              id: session === "team/a" ? "a1" : "b1",
              sessionName: session ?? "",
              time: "2026-01-01T00:00:01Z",
              type: "user.note",
              source: "cli",
              direction: "internal",
              summary: session === "team/a" ? "first-a" : "first-b",
            },
          ],
          nextCursor: "cur-1",
        }),
      );
    });

    const queryClient = new QueryClient();
    const { rerender } = render(
      <QueryClientProvider client={queryClient}>
        <Conversation sessionName="team/a" onSelectSession={vi.fn()} />
      </QueryClientProvider>,
    );
    await screen.findByText("first-a");

    rerender(
      <QueryClientProvider client={queryClient}>
        <Conversation sessionName="team/b" onSelectSession={vi.fn()} />
      </QueryClientProvider>,
    );
    await screen.findByText("first-b");

    // The delayed frame arrives on team/a's (already-cancelled) connection
    // only now — after the switch — simulating a response that was already
    // in flight at the moment of cancellation.
    pushLate(
      'id: cur-late\ndata: {"id":"late","sessionName":"team/a","time":"2026-01-01T00:00:03Z","type":"user.note","source":"cli","direction":"internal","summary":"late-arrival"}\n\n',
    );
    // A real delay, not a negative waitFor (which would pass immediately by
    // just checking "not yet in the DOM" before the pushed frame has even
    // had a chance to propagate): give the read loop's promise chain every
    // opportunity to run before asserting it produced nothing.
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(screen.queryByText("late-arrival")).not.toBeInTheDocument();
  });

  it("opens the live subscription even when the session's history starts out empty", async () => {
    let sawStreamCursorParam: string | null = "unset";
    vi.mocked(fetch).mockImplementation((input) => {
      const url = requestUrl(input);
      if (url.pathname.endsWith("/events/stream")) {
        sawStreamCursorParam = url.searchParams.get("cursor");
        return Promise.resolve(
          sseResponse(
            'id: cur-1\ndata: {"id":"01","sessionName":"team/a","time":"2026-01-01T00:00:01Z","type":"user.note","source":"cli","direction":"internal","summary":"first-live-event"}\n\n',
          ),
        );
      }
      return Promise.resolve(jsonResponse({ events: [] })); // no nextCursor: no log exists yet
    });

    renderConversation("team/a");

    expect(await screen.findByText("first-live-event")).toBeInTheDocument();
    expect(sawStreamCursorParam).toBeNull();
  });

  it("clears a previous session's live events when switching to another session whose own history is also empty", async () => {
    vi.mocked(fetch).mockImplementation((input) => {
      const url = requestUrl(input);
      const session = url.searchParams.get("session");
      if (url.pathname.endsWith("/events/stream")) {
        if (session === "team/a") {
          return Promise.resolve(
            sseResponse(
              'id: cur-1\ndata: {"id":"01","sessionName":"team/a","time":"2026-01-01T00:00:01Z","type":"user.note","source":"cli","direction":"internal","summary":"team-a-live"}\n\n',
            ),
          );
        }
        return Promise.resolve(sseResponse(""));
      }
      return Promise.resolve(jsonResponse({ events: [] })); // both sessions: no nextCursor
    });

    const queryClient = new QueryClient();
    const { rerender } = render(
      <QueryClientProvider client={queryClient}>
        <Conversation sessionName="team/a" onSelectSession={vi.fn()} />
      </QueryClientProvider>,
    );
    await screen.findByText("team-a-live");

    rerender(
      <QueryClientProvider client={queryClient}>
        <Conversation sessionName="team/b" onSelectSession={vi.fn()} />
      </QueryClientProvider>,
    );

    await screen.findByText(/no events recorded/i);
    expect(screen.queryByText("team-a-live")).not.toBeInTheDocument();
  });
});
