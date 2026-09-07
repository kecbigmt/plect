namespace Plecture.WebApi;

/// The event's flow relative to the plect session — `contracts/event.Direction`.
enum EventDirection {
  inbound,
  outbound,
  internal,
}

/// Whether a terminal event was pushed one hop to a parent/ancestor, or is an
/// ordinary pull-only record read via this history endpoint. Derived from
/// whether `type` carries the `plect.terminal.` prefix
/// (`contracts/event.TypeTerminalPrefix`), not a stored field. Absent
/// normalizes to pull.
enum EventDeliveryMode {
  push,
  pull,
}

/// Paging direction for a session's event history — `contracts/event.Order`.
enum EventOrder {
  asc,
  desc,
}

/// One record from a session's durable, append-only event log —
/// `contracts/event.Event`, passed through verbatim. Every field carries the
/// log's own value with no reinterpretation: an unrecognized `type`, an
/// unrecognized `source`, or an unknown `metadata` key survives this
/// projection unchanged, since neither this API nor the underlying log
/// interprets them.
model Event {
  id: string;
  sessionName: string;
  time: utcDateTime;

  @doc("A free-form dotted topic (e.g. `user.emit`, `lifecycle.created`). Not an enum: producers define their own types, and this API does not enumerate or restrict them.")
  type: string;

  @doc("Who produced the event (`plect`, `web`, `cli`, `mcp`, or a provider's own source string). Not an enum for the same reason as `type`.")
  source: string;
  direction: EventDirection;
  summary: string;

  @doc("Absent and empty are equivalent, as with every optional field on this contract.")
  body?: string;

  @doc("""
    Arbitrary event metadata, passed through verbatim including keys this API
    does not itself interpret — e.g. `origin_session`
    (`contracts/event.MetaOriginSession`), the session that produced a
    notification pushed into a receiving session's own log, which is
    distinct from this record's own `sessionName` (the receiver).
    """)
  metadata?: Record<string>;

  deliveryMode?: EventDeliveryMode;
}

/// One page of a session's event history — `service.EventPageResult`.
/// `nextCursor` is present only for an ascending page (once the session's log
/// exists at all); a descending page never carries one in this v1 contract.
model EventPage {
  events: Event[];
  nextCursor?: string;
}
