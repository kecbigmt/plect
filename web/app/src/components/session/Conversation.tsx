import type { SessionEvent } from "@/lib/eventsApi";
import { dedupeEventsById, useSessionEvents } from "@/lib/useEvents";
import { Button } from "@/components/ui/button";

// key={sessionName} forces a remount on every selection change, matching
// DetailPane's own pattern: switching sessions resets straight to a fresh
// loading state instead of a stale timeline lingering while the new
// session's first page is still in flight.
export function Conversation({
  sessionName,
  onSelectSession,
}: {
  sessionName: string;
  onSelectSession: (name: string) => void;
}) {
  return (
    <SessionConversation key={sessionName} sessionName={sessionName} onSelectSession={onSelectSession} />
  );
}

function SessionConversation({
  sessionName,
  onSelectSession,
}: {
  sessionName: string;
  onSelectSession: (name: string) => void;
}) {
  const events = useSessionEvents(sessionName);

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
    <div role="region" aria-label="Conversation" className="flex min-h-0 flex-1 flex-col overflow-auto p-3">
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

// Utterance vs. compact is driven only by the event's own recorded
// direction and body — never inferred from type, and never derived from
// the session's current status — so an unrecognized type still renders
// sensibly: a body-bearing inbound/outbound record (a user message, a
// delivered instruction, a pushed kick) reads as a message; everything
// else (internal records, and inbound/outbound records with no body, such
// as a bare terminal.done push) is a compact row that still surfaces its
// type, source, summary, body, and metadata verbatim.
function isUtterance(event: SessionEvent): boolean {
  return (event.direction === "inbound" || event.direction === "outbound") && !!event.body?.trim();
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
      <article className="rounded-md border border-border p-2 text-sm">
        <header className="flex items-center justify-between gap-2 text-xs text-muted-foreground">
          <span>
            {event.source} · {event.direction}
          </span>
          <time dateTime={event.time}>{time}</time>
        </header>
        <p className="mt-1 whitespace-pre-wrap break-words">{event.body}</p>
        {originSession && originSession !== sessionName && (
          <OriginNote origin={originSession} receiver={sessionName} onSelectSession={onSelectSession} />
        )}
        <EventMeta event={event} />
      </article>
    );
  }

  return (
    <article className="flex flex-col gap-1 rounded-md border border-dashed border-border p-2 text-xs">
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
