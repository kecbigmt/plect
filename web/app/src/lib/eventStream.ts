import type { SessionEvent } from "@/lib/eventsApi";

// The live-timeline connection module (docs/design/web-ui-event-history.md's
// history/live handoff; issue #407's ratified decision to hand-roll this over
// `fetch()` + `ReadableStream` rather than the browser's native
// `EventSource`). `EventSource` cannot read a response's HTTP status or body
// on failure, so a 401 (auth expired), a 502 (bus unavailable), and a
// transient network blip would all surface identically — this module reads
// `response.status` directly, matching how `bootstrap.ts` already
// distinguishes those cases for the bootstrap read. It also owns its own
// resume cursor end-to-end (the wire's opaque `id:` per frame — the same
// format `GET /api/v1/events` returns as `nextCursor`), so a caller never
// re-derives a resume position after the first connect.
//
// Reconnect uses full-jitter exponential backoff: `BACKOFF_INITIAL_MS` (0.5s)
// doubling each attempt (`BACKOFF_FACTOR`) up to `BACKOFF_MAX_MS` (15s), reset
// to zero attempts as soon as a connection is actually accepted (a long-lived
// stream that later drops does not inherit an old backoff count from a prior
// blip). After `MAX_RECONNECT_ATTEMPTS` (8) consecutive failures to even
// connect, the module gives up and reports "unavailable" rather than retrying
// forever silently — the acceptance criteria call for a "usable client
// state," not an endless invisible retry loop; a caller that wants to keep
// trying re-opens the connection (a fresh `openEventStream` call), which
// resets the count. A `401` never retries at all: it reports "auth-expired"
// immediately, since retrying will not fix an expired session.

export const BACKOFF_INITIAL_MS = 500;
export const BACKOFF_FACTOR = 2;
export const BACKOFF_MAX_MS = 15_000;
export const MAX_RECONNECT_ATTEMPTS = 8;

export type EventStreamState =
  | "connecting"
  | "live"
  | "reconnecting"
  | "unavailable"
  | "auth-expired";

export interface EventStreamHandlers {
  onEvent: (event: SessionEvent) => void;
  onStateChange: (state: EventStreamState) => void;
}

function streamUrl(sessionName: string, cursor: string): string {
  const url = new URL("/api/v1/events/stream", window.location.origin);
  url.searchParams.set("session", sessionName);
  if (cursor) {
    url.searchParams.set("cursor", cursor);
  }
  return url.toString();
}

function isAbortError(err: unknown): boolean {
  return err instanceof DOMException && err.name === "AbortError";
}

// backoffDelayMs computes the full-jitter delay before reconnect attempt N
// (1-indexed): a random value in [0, min(INITIAL * FACTOR^(N-1), MAX)],
// spreading simultaneous reconnects (e.g. after a bus restart) rather than
// having every open pane retry in lockstep.
export function backoffDelayMs(attempt: number): number {
  const cap = Math.min(BACKOFF_INITIAL_MS * BACKOFF_FACTOR ** (attempt - 1), BACKOFF_MAX_MS);
  return Math.random() * cap;
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) {
      resolve();
      return;
    }
    const timer = setTimeout(resolve, ms);
    signal.addEventListener("abort", () => {
      clearTimeout(timer);
      resolve();
    }, { once: true });
  });
}

type ConnectOutcome = { kind: "aborted" } | { kind: "auth-expired" } | { kind: "retry" };

// dispatchFrame parses one SSE data block. A malformed frame is dropped
// rather than crashing the stream — the same best-effort behavior the Go
// relay's own JSON unmarshal already has (events_stream_json.go).
function dispatchFrame(raw: string, onEvent: (event: SessionEvent) => void): void {
  try {
    onEvent(JSON.parse(raw) as SessionEvent);
  } catch {
    // Dropped: see comment above.
  }
}

// connectOnce opens one fetch-based SSE connection and reads it to
// completion (server close, network failure, or abort), parsing the same
// line-oriented framing the Go relay writes (a blank line dispatches the
// buffered frame; ":"-prefixed lines are keepalive comments; "id:" advances
// cursor.value so a later reconnect resumes from the last frame actually
// processed, not just where this connection started).
async function connectOnce(
  sessionName: string,
  cursor: { value: string },
  handlers: EventStreamHandlers,
  signal: AbortSignal,
  onConnected: () => void,
): Promise<ConnectOutcome> {
  let response: Response;
  try {
    response = await fetch(streamUrl(sessionName, cursor.value), {
      signal,
      credentials: "same-origin",
    });
  } catch (err) {
    return isAbortError(err) ? { kind: "aborted" } : { kind: "retry" };
  }
  if (response.status === 401) {
    return { kind: "auth-expired" };
  }
  if (!response.ok || !response.body) {
    return { kind: "retry" };
  }

  // The connection was accepted: reset the reconnect-attempt budget before
  // reading a single frame, so a long-lived stream's eventual, unrelated
  // disconnect starts a fresh backoff series rather than inheriting whatever
  // count a prior blip left behind.
  onConnected();
  handlers.onStateChange("live");
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let dataLines: string[] = [];
  try {
    while (true) {
      const { done, value } = await reader.read();
      // Re-checked after every read, not just relied on via `fetch`'s own
      // AbortSignal wiring: a frame already in flight when the caller
      // cancels (a session switch) can still resolve after `signal.aborted`
      // flips, and dispatching it would let a delayed response cross into
      // whatever session opened next — the exact hazard the acceptance
      // criteria call out by name.
      if (signal.aborted) {
        return { kind: "aborted" };
      }
      if (done) {
        break;
      }
      buffer += decoder.decode(value, { stream: true });
      const lines = buffer.split("\n");
      buffer = lines.pop() ?? "";
      for (const rawLine of lines) {
        const line = rawLine.endsWith("\r") ? rawLine.slice(0, -1) : rawLine;
        if (line === "") {
          if (dataLines.length > 0) {
            dispatchFrame(dataLines.join("\n"), handlers.onEvent);
            dataLines = [];
          }
          continue;
        }
        if (line.startsWith(":")) {
          continue; // keepalive comment
        }
        if (line.startsWith("id:")) {
          cursor.value = line.slice(3).trim();
          continue;
        }
        if (line.startsWith("data:")) {
          dataLines.push(line.slice(5).replace(/^ /, ""));
        }
      }
    }
  } catch (err) {
    return isAbortError(err) ? { kind: "aborted" } : { kind: "retry" };
  }
  return { kind: "retry" }; // the stream ended (server close/bus restart); reconnect from cursor.value
}

// openEventStream starts the connection loop and returns immediately; it
// runs until `signal` aborts (the caller's cleanup — a session switch or
// unmount) or the module itself gives up (auth expiry, or exhausted
// reconnect attempts). `initialCursor` is the seed only: every reconnect
// after the first uses whatever cursor.value advanced to, from the frames
// actually processed.
export function openEventStream(
  sessionName: string,
  initialCursor: string,
  handlers: EventStreamHandlers,
  signal: AbortSignal,
): void {
  const cursor = { value: initialCursor };
  void (async () => {
    let attempt = 0;
    handlers.onStateChange("connecting");
    while (!signal.aborted) {
      const outcome = await connectOnce(sessionName, cursor, handlers, signal, () => {
        attempt = 0;
      });
      if (signal.aborted || outcome.kind === "aborted") {
        return;
      }
      if (outcome.kind === "auth-expired") {
        handlers.onStateChange("auth-expired");
        return;
      }
      attempt += 1;
      if (attempt > MAX_RECONNECT_ATTEMPTS) {
        handlers.onStateChange("unavailable");
        return;
      }
      handlers.onStateChange("reconnecting");
      await sleep(backoffDelayMs(attempt), signal);
    }
  })();
}
