import type { SessionEvent } from "@/lib/eventsApi";

export const MESSAGE_EVENT_TYPE = "plect.message";
export const MESSAGE_DELTA_EVENT_TYPE = "plect.message_delta";

// docs/language/events.md rule 3: history under these retired producers is
// never rewritten, so it keeps rendering as finished messages.
const LEGACY_MESSAGE_TYPES: ReadonlySet<string> = new Set([
  "claude.reply",
  "codex.reply",
  "claude.message_display",
]);

// A reasoning delta is opt-in per emitter and a consumer may ignore it.
const DELTA_KIND_TEXT = "text";

export interface ConversationMessage {
  readonly kind: "message";
  readonly id: string;
  readonly time: string;
  readonly source?: string;
  readonly turnId?: string;
  readonly text: string;
  // True once the delta stream itself reports done (metadata final=true)
  // or the canonical plect.message has arrived — either closes the
  // streaming indicator, but only the canonical message's body is
  // authoritative: it still replaces delta-built text whenever it arrives.
  readonly final: boolean;
  readonly legacy: boolean;
}

export interface ConversationEventEntry {
  readonly kind: "event";
  readonly event: SessionEvent;
}

export type ConversationEntry = ConversationMessage | ConversationEventEntry;

interface MessageBuild {
  index: number;
  time: string;
  source?: string;
  turnId?: string;
  deltas: Map<number, string>;
  finalText?: string;
  deltaClosed: boolean;
}

function deltaText(build: MessageBuild): string {
  return [...build.deltas.entries()]
    .sort(([a], [b]) => a - b)
    .map(([, text]) => text)
    .join("");
}

// A message stays at its first-seen position: a later chunk for the same
// message_id extends it in place rather than moving or duplicating a row.
export function buildConversationTimeline(events: readonly SessionEvent[]): ConversationEntry[] {
  const entries: ConversationEntry[] = [];
  const messages = new Map<string, MessageBuild>();

  function upsert(messageId: string, time: string): MessageBuild {
    let build = messages.get(messageId);
    if (build === undefined) {
      build = { index: entries.length, time, deltas: new Map(), deltaClosed: false };
      messages.set(messageId, build);
      entries.push({ kind: "message", id: messageId, time, text: "", final: false, legacy: false });
    }
    return build;
  }

  function commit(messageId: string, build: MessageBuild) {
    entries[build.index] = {
      kind: "message",
      id: messageId,
      time: build.time,
      source: build.source,
      turnId: build.turnId,
      text: build.finalText ?? deltaText(build),
      final: build.finalText !== undefined || build.deltaClosed,
      legacy: false,
    };
  }

  for (const event of events) {
    if (event.type === MESSAGE_DELTA_EVENT_TYPE || event.type === MESSAGE_EVENT_TYPE) {
      const messageId = event.metadata?.message_id;
      if (!messageId) {
        entries.push({ kind: "event", event });
        continue;
      }
      const build = upsert(messageId, event.time);
      build.source ??= event.metadata?.source;
      build.turnId ??= event.metadata?.turn_id;
      if (event.type === MESSAGE_EVENT_TYPE) {
        build.finalText = event.body ?? "";
      } else {
        if ((event.metadata?.kind ?? DELTA_KIND_TEXT) === DELTA_KIND_TEXT && event.body) {
          build.deltas.set(Number(event.metadata?.index ?? "0"), event.body);
        }
        if (event.metadata?.final === "true") {
          build.deltaClosed = true;
        }
      }
      commit(messageId, build);
      continue;
    }
    if (LEGACY_MESSAGE_TYPES.has(event.type)) {
      entries.push({
        kind: "message",
        id: event.id,
        time: event.time,
        source: event.source,
        text: event.body || event.summary,
        final: true,
        legacy: true,
      });
      continue;
    }
    entries.push({ kind: "event", event });
  }

  return entries;
}
