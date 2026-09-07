import type { SessionEvent } from "@/lib/eventsApi";

// Reconnect policy (docs/design/web-ui-event-history.md's "The live
// subscription"):
//   initial   BACKOFF_INITIAL_MS      0.5s
//   factor    BACKOFF_FACTOR          x2 per attempt
//   cap       BACKOFF_MAX_MS          15s
//   give-up   MAX_RECONNECT_ATTEMPTS  8 consecutive failures -> "unavailable"
// A 401 reports "auth-expired" immediately, with no retry.

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

function sessionStreamUrl(sessionName: string, cursor: string): string {
  const url = new URL("/api/v1/events/stream", window.location.origin);
  url.searchParams.set("session", sessionName);
  if (cursor) {
    url.searchParams.set("cursor", cursor);
  }
  return url.toString();
}

// No session, no cursor: the list's cross-session facts source
// (events_stream_all.go) has no resume token — a reconnect just replays its
// filtered history again rather than continuing from where it left off.
function allSessionsStreamUrl(): string {
  return new URL("/api/v1/events/stream/all", window.location.origin).toString();
}

function isAbortError(err: unknown): boolean {
  return err instanceof DOMException && err.name === "AbortError";
}

// Full jitter prevents every open pane from retrying in lockstep.
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

// One malformed upstream frame must not terminate the stream.
function dispatchFrame(raw: string, onEvent: (event: SessionEvent) => void): void {
  try {
    onEvent(JSON.parse(raw) as SessionEvent);
  } catch {}
}

async function connectOnce(
  urlFor: (cursor: string) => string,
  cursor: { value: string },
  handlers: EventStreamHandlers,
  signal: AbortSignal,
  onConnected: () => void,
): Promise<ConnectOutcome> {
  let response: Response;
  try {
    response = await fetch(urlFor(cursor.value), {
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
      // A queued read can resolve after abort.
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
          // Committing before the frame terminator would skip a truncated event.
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
          continue;
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
  return { kind: "retry" };
}

function openStream(
  urlFor: (cursor: string) => string,
  initialCursor: string,
  handlers: EventStreamHandlers,
  signal: AbortSignal,
): void {
  const cursor = { value: initialCursor };
  void (async () => {
    let attempt = 0;
    handlers.onStateChange("connecting");
    while (!signal.aborted) {
      const outcome = await connectOnce(urlFor, cursor, handlers, signal, () => {
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

export function openEventStream(
  sessionName: string,
  initialCursor: string,
  handlers: EventStreamHandlers,
  signal: AbortSignal,
): void {
  openStream((cursor) => sessionStreamUrl(sessionName, cursor), initialCursor, handlers, signal);
}

// The list's cross-session facts source: same reconnect/backoff/parsing
// engine as openEventStream, over the one unchanging all-sessions URL
// instead of one session's.
export function openAllSessionsEventStream(handlers: EventStreamHandlers, signal: AbortSignal): void {
  openStream(allSessionsStreamUrl, "", handlers, signal);
}
