import { useLayoutEffect, useRef } from "react";

import type { SessionEvent } from "@/lib/eventsApi";
import { dedupeEventsById, useSessionEvents } from "@/lib/useEvents";
import { Button } from "@/components/ui/button";

// Unlike DetailPane's key={sessionName} remount, this component never
// unmounts on selection change: a fresh queryKey already renders isPending
// with no data (TanStack Query's own default), so nothing here needs a
// remount to avoid leaking a previous session's events, and keeping the one
// scroll container alive across a switch is what lets restoreScrollPosition
// below put a revisited session back where its reading left off.
export function Conversation({
  sessionName,
  onSelectSession,
}: {
  sessionName: string;
  onSelectSession: (name: string) => void;
}) {
  const events = useSessionEvents(sessionName);
  const scrollRef = useRef<HTMLDivElement>(null);
  // Keyed by session name so switching away and back restores that
  // session's own offset instead of carrying over whichever session was
  // viewed last; survives because this component itself never remounts.
  const scrollPositions = useRef<Map<string, number>>(new Map());
  // Guards against re-applying a saved offset on every render once a
  // session has already been restored (e.g. a background refetch, or
  // fetchNextPage appending a page) — only an actual switch to a
  // not-yet-restored session should move the scroll position.
  const restoredForRef = useRef<string | null>(null);

  useLayoutEffect(() => {
    if (events.isPending || restoredForRef.current === sessionName) {
      return;
    }
    if (scrollRef.current) {
      scrollRef.current.scrollTop = scrollPositions.current.get(sessionName) ?? 0;
    }
    restoredForRef.current = sessionName;
  }, [sessionName, events.isPending]);

  if (events.isPending) {
    return (
      <div className="flex-1 p-3 text-sm text-muted-foreground">
        <p>Loading…</p>
      </div>
    );
  }
  if (events.isError) {
    return (
      <div role="alert" className="flex-1 p-3 text-sm text-muted-foreground">
        <p>Couldn’t load the conversation.</p>
      </div>
    );
  }

  const items = dedupeEventsById(events.data.pages);

  return (
    // overflow-auto (not the ancestor) owns the scroll container, so
    // appending a loaded page never disturbs the reading position: new
    // events land after existing ones in normal document flow, and nothing
    // here scrolls the view on its own.
    <div
      ref={scrollRef}
      role="region"
      aria-label="Conversation"
      className="flex min-h-0 flex-1 flex-col overflow-auto p-3"
      onScroll={(e) => scrollPositions.current.set(sessionName, e.currentTarget.scrollTop)}
    >
      {items.length === 0 ? (
        <p className="text-sm text-muted-foreground">No events recorded yet.</p>
      ) : (
        <ul className="flex flex-col gap-2">
          {items.map((e) => (
            <li key={e.id}>
              <EventRow event={e} sessionName={sessionName} onSelectSession={onSelectSession} />
            </li>
          ))}
        </ul>
      )}
      {events.hasNextPage && (
        <Button
          type="button"
          variant="outline"
          size="default"
          className="mt-3 self-center"
          disabled={events.isFetchingNextPage}
          onClick={() => void events.fetchNextPage()}
        >
          {events.isFetchingNextPage ? "Loading…" : "Load more"}
        </Button>
      )}
    </div>
  );
}

// Only the two conversational types contracts/event itself defines get
// message styling; a producer-defined type this UI does not recognize
// stays on the compact path by default, rather than adopting message
// styling on the strength of direction and body alone.
const UTTERANCE_TYPES: ReadonlySet<string> = new Set(["user.emit", "plect.instruction"]);

function isUtterance(event: SessionEvent): boolean {
  return UTTERANCE_TYPES.has(event.type) && !!event.body?.trim();
}

function EventRow({
  event,
  sessionName,
  onSelectSession,
}: {
  event: SessionEvent;
  sessionName: string;
  onSelectSession: (name: string) => void;
}) {
  const time = new Date(event.time).toLocaleString();
  const originSession = event.metadata?.origin_session;

  if (isUtterance(event)) {
    return (
      <article data-event-style="utterance" className="rounded-md border border-border p-2 text-sm">
        <header className="flex items-center justify-between gap-2 text-xs text-muted-foreground">
          <span>
            <code>{event.type}</code> · {event.source} · {event.direction}
          </span>
          <time dateTime={event.time}>{time}</time>
        </header>
        {event.summary && <p className="mt-0.5 text-xs font-medium text-foreground">{event.summary}</p>}
        <p className="mt-1 whitespace-pre-wrap break-words">{event.body}</p>
        {originSession && originSession !== sessionName && (
          <OriginNote origin={originSession} receiver={sessionName} onSelectSession={onSelectSession} />
        )}
        <EventMeta event={event} />
      </article>
    );
  }

  return (
    <article data-event-style="compact" className="flex flex-col gap-1 rounded-md border border-dashed border-border p-2 text-xs">
      <div className="flex items-center justify-between gap-2">
        <span className="font-medium text-foreground">{event.summary || event.type}</span>
        <time dateTime={event.time} className="shrink-0 text-muted-foreground">
          {time}
        </time>
      </div>
      <div className="text-muted-foreground">
        <code>{event.type}</code> · {event.source}
      </div>
      {event.body && <p className="whitespace-pre-wrap break-words text-foreground">{event.body}</p>}
      {originSession && originSession !== sessionName && (
        <OriginNote origin={originSession} receiver={sessionName} onSelectSession={onSelectSession} />
      )}
      <EventMeta event={event} />
    </article>
  );
}

// Distinguishes the receiving session_name (this timeline) from the
// notification's origin_session, and offers the one navigation this
// read-only milestone already has: selecting another session in the tree.
function OriginNote({
  origin,
  receiver,
  onSelectSession,
}: {
  origin: string;
  receiver: string;
  onSelectSession: (name: string) => void;
}) {
  return (
    <div className="flex flex-wrap items-center gap-1 text-muted-foreground">
      <span>From</span>
      <Button
        type="button"
        variant="ghost"
        size="default"
        className="h-auto px-1 py-0 text-xs underline underline-offset-2"
        onClick={() => onSelectSession(origin)}
      >
        {origin}
      </Button>
      <span>→ {receiver}</span>
    </div>
  );
}

// Every metadata key survives this projection unchanged — including keys
// this UI does not itself interpret — so an unrecognized event kind never
// silently drops information (origin_session already renders via
// OriginNote, so it is omitted here to avoid saying the same thing twice).
function EventMeta({ event }: { event: SessionEvent }) {
  const entries = Object.entries(event.metadata ?? {}).filter(([key]) => key !== "origin_session");
  if (entries.length === 0) {
    return null;
  }
  return (
    <dl className="mt-1 flex flex-wrap gap-x-3 gap-y-0.5 text-muted-foreground">
      {entries.map(([key, value]) => (
        <div key={key} className="flex gap-1">
          <dt>{key}:</dt>
          <dd className="break-all text-foreground">{value}</dd>
        </div>
      ))}
    </dl>
  );
}
