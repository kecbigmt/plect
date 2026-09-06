import type { SessionEvent } from "@/lib/eventsApi";

// The live-timeline connection module (docs/design/web-ui-event-history.md's
// "The live subscription" — see there for why this hand-rolls fetch() +
// ReadableStream instead of using EventSource).
//
// Reconnect: full-jitter backoff, BACKOFF_INITIAL_MS (0.5s) doubling by
// BACKOFF_FACTOR up to BACKOFF_MAX_MS (15s); the attempt budget resets once a
// connection is actually accepted. After MAX_RECONNECT_ATTEMPTS (8)
// consecutive failures to connect, this gives up and reports "unavailable"
// rather than retrying forever silently — a caller that wants to keep trying
// calls openEventStream again, which resets the budget. A 401 reports
// "auth-expired" immediately with no retry.

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
  if (!response.ok || !response.body || response.bodyUsed) {
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
  let pendingId: string | null = null;
  try {
    while (true) {
      const { done, value } = await reader.read();
      // Re-checked after every read, not just relied on via fetch's own
      // AbortSignal wiring: a frame already in flight when the caller
      // cancels (a session switch) can still resolve after signal.aborted
      // flips, and dispatching it would let it cross into whatever session
      // opened next.
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
          // A frame's id becomes the resume cursor only once its terminating
          // blank line arrives — committing it at the "id:" line itself
          // would advance past an event whose "data:" line never showed up
          // because the connection dropped mid-frame, permanently skipping
          // it on reconnect.
          if (dataLines.length > 0) {
            dispatchFrame(dataLines.join("\n"), handlers.onEvent);
            dataLines = [];
          }
          if (pendingId !== null) {
            cursor.value = pendingId;
            pendingId = null;
          }
          continue;
        }
        if (line.startsWith(":")) {
          continue; // keepalive comment
        }
        if (line.startsWith("id:")) {
          pendingId = line.slice(3).trim();
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

// initialCursor seeds only the first connection attempt; every reconnect
// after that resumes from whatever cursor.value last advanced to.
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
