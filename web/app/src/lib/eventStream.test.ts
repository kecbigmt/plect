import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  BACKOFF_INITIAL_MS,
  BACKOFF_MAX_MS,
  MAX_RECONNECT_ATTEMPTS,
  backoffDelayMs,
  openEventStream,
  type EventStreamState,
} from "@/lib/eventStream";

function sseResponse(frames: string, status = 200): Response {
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(new TextEncoder().encode(frames));
      controller.close();
    },
  });
  return new Response(stream, { status });
}

function emptyResponse(status: number): Response {
  return new Response(null, { status });
}

function cursorOf(input: RequestInfo | URL): string {
  const url = new URL(String(input instanceof Request ? input.url : input));
  return url.searchParams.get("cursor") ?? "";
}

describe("backoffDelayMs", () => {
  beforeEach(() => {
    vi.spyOn(Math, "random").mockReturnValue(1); // full jitter's upper bound, for a deterministic assertion
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("doubles the delay each attempt", () => {
    expect(backoffDelayMs(1)).toBe(BACKOFF_INITIAL_MS);
    expect(backoffDelayMs(2)).toBe(BACKOFF_INITIAL_MS * 2);
    expect(backoffDelayMs(3)).toBe(BACKOFF_INITIAL_MS * 4);
  });

  it("saturates at BACKOFF_MAX_MS instead of growing unbounded", () => {
    expect(backoffDelayMs(20)).toBe(BACKOFF_MAX_MS);
  });
});

describe("openEventStream", () => {
  beforeEach(() => {
    vi.stubGlobal("fetch", vi.fn());
    vi.spyOn(Math, "random").mockReturnValue(0); // minimal (zero) backoff wait, for fast deterministic tests
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it("dispatches parsed events and resumes a reconnect from the last id it saw, not the seed cursor", async () => {
    const events: unknown[] = [];
    const states: EventStreamState[] = [];
    const controller = new AbortController();
    const seenCursors: string[] = [];

    vi.mocked(fetch).mockImplementation((input) => {
      seenCursors.push(cursorOf(input as RequestInfo | URL));
      if (seenCursors.length === 1) {
        return Promise.resolve(sseResponse('id: cur-1\ndata: {"id":"E1","summary":"first"}\n\n'));
      }
      controller.abort(); // stop the test deterministically once the resumed reconnect is observed
      return Promise.reject(new DOMException("aborted", "AbortError"));
    });

    openEventStream(
      "team/a",
      "seed-cursor",
      { onEvent: (e) => events.push(e), onStateChange: (s) => states.push(s) },
      controller.signal,
    );

    await vi.advanceTimersByTimeAsync(BACKOFF_MAX_MS);

    expect(seenCursors[0]).toBe("seed-cursor");
    expect(seenCursors[1]).toBe("cur-1");
    expect(events).toEqual([{ id: "E1", summary: "first" }]);
    expect(states.slice(0, 3)).toEqual(["connecting", "live", "reconnecting"]);
  });

  it("reports auth-expired on a 401 and does not reconnect", async () => {
    const states: EventStreamState[] = [];
    vi.mocked(fetch).mockResolvedValue(emptyResponse(401));
    const controller = new AbortController();

    openEventStream("team/a", "", { onEvent: () => {}, onStateChange: (s) => states.push(s) }, controller.signal);
    await vi.advanceTimersByTimeAsync(BACKOFF_MAX_MS * 2);

    expect(states).toEqual(["connecting", "auth-expired"]);
    expect(vi.mocked(fetch)).toHaveBeenCalledTimes(1);
  });

  it("gives up and reports unavailable after MAX_RECONNECT_ATTEMPTS consecutive failures", async () => {
    const states: EventStreamState[] = [];
    vi.mocked(fetch).mockResolvedValue(emptyResponse(502));
    const controller = new AbortController();

    openEventStream("team/a", "", { onEvent: () => {}, onStateChange: (s) => states.push(s) }, controller.signal);

    for (let i = 0; i < MAX_RECONNECT_ATTEMPTS + 1; i++) {
      await vi.advanceTimersByTimeAsync(BACKOFF_MAX_MS);
    }

    expect(states.at(-1)).toBe("unavailable");
    expect(vi.mocked(fetch)).toHaveBeenCalledTimes(MAX_RECONNECT_ATTEMPTS + 1);
  });

  it("drops a frame that resolves after abort instead of dispatching it", async () => {
    const events: unknown[] = [];
    const controller = new AbortController();
    let push!: (text: string) => void;
    const stream = new ReadableStream<Uint8Array>({
      start(streamController) {
        // Nothing enqueued yet: the reader's first read() stays pending
        // until the test calls push below, simulating a frame that is
        // genuinely still in flight over the wire at the moment of abort.
        push = (text) => streamController.enqueue(new TextEncoder().encode(text));
      },
    });
    vi.mocked(fetch).mockResolvedValue(new Response(stream, { status: 200 }));

    openEventStream("team/a", "", { onEvent: (e) => events.push(e), onStateChange: () => {} }, controller.signal);
    await vi.advanceTimersByTimeAsync(0); // let fetch resolve and the reader start awaiting its first read()

    // The cancellation (a session switch) happens while a frame is already
    // in flight — only after that does the delayed frame actually arrive.
    controller.abort();
    push('id: cur-1\ndata: {"id":"E1","summary":"late"}\n\n');
    await vi.advanceTimersByTimeAsync(0);

    expect(events).toEqual([]);
  });

  it("stops immediately on abort, mid-backoff, with no further reconnect attempt", async () => {
    const states: EventStreamState[] = [];
    vi.mocked(fetch).mockResolvedValue(emptyResponse(502));
    const controller = new AbortController();

    openEventStream("team/a", "", { onEvent: () => {}, onStateChange: (s) => states.push(s) }, controller.signal);
    await vi.advanceTimersByTimeAsync(0); // let the first (failing) fetch settle, entering backoff
    const callsBeforeAbort = vi.mocked(fetch).mock.calls.length;
    controller.abort();
    await vi.advanceTimersByTimeAsync(BACKOFF_MAX_MS * 4);

    expect(vi.mocked(fetch).mock.calls.length).toBe(callsBeforeAbort);
    expect(states).not.toContain("unavailable");
  });
});
