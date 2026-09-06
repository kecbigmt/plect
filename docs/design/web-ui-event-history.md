# Event history and the history/live handoff protocol

[docs/design/web-ui.md](web-ui.md)'s Conversation section specifies what the
session timeline shows. This document specifies the read contract behind it
(`GET /events`, `web/api/routes/events.tsp`) and the protocol a client uses to
move from a bounded history read to a live subscription
([the following SSE task](../adr/2026-09-05-web-ui-client-server-boundary.md))
without losing or duplicating an event that arrives in between. It does not
define the SSE endpoint itself.

## The read contract

`GET /events?session=<name>&cursor=&limit=&order=` returns one page of a
single session's durable event log, unchanged from
`app/internal/service/event.go`'s `EventPage`. `session` rides as a query
value rather than a path segment: session names contain `/`, and this
contract has no fixed literal path segment (`events`) to reserve against a
collision with the wildcard route `GET /sessions/{name...}` already uses for
session detail.

`order` defaults to `asc` (oldest first): it paginates forward from the log's
head, or from `cursor` (a prior page's opaque `nextCursor`), and returns a
`nextCursor` whenever the session's log exists, independent of whether the
returned page happened to contain any events. `desc` (newest first) returns
only the single most recent page bounded by `limit` and never a `nextCursor`
— no backward pagination is promised, matching `EventPage`'s own v1 contract.

Every field of `contracts/event.Event` is projected onto the wire verbatim:
`type` and `source` are untyped strings (a producer's own namespace, not this
API's to enumerate), and `metadata` passes through every key the log holds,
known or not — including `origin_session`
(`contracts/event.MetaOriginSession`), which names the event's emitter and is
always distinct from the record's own `sessionName`, the session whose log
holds it (the receiver, for a notification pushed one hop by the terminal-
event-propagation ADR). No metadata key is promoted to a typed field. Durable
event contracts and event publishing behavior are unchanged by this read.

A session no event was ever published to answers with a 200 and an empty
page, not a 404: the event log is independent of session-tree membership (a
destroyed session keeps its log; `EventList`'s CLI-facing doc already states
this), so "missing" and "empty" collapse to the same response here — unlike
`GET /sessions/{name}`, where a session absent from state is a 404. A cursor
issued for a different order, or against a log generation that no longer
exists (the log rotated), is rejected as invalid input; the client's only
recovery is to drop it and restart from the beginning.

## The history/live handoff protocol

A client opens a session's timeline in two steps: fetch a bounded history
page, then open a live subscription that continues from where the history
left off. Between those two steps, the session's log keeps accepting new
events — nothing pauses it for the handoff, and nothing in this contract
promises an atomic snapshot cursor that marks "the moment history was read"
against a global position. The protocol closes that gap using two properties
already true of the existing contracts, not a new mechanism:

- **An ascending cursor is a forward position in an append-only log, not a
  snapshot bound to when it was issued.** Re-querying with the same
  `nextCursor` after any amount of time returns every event appended since,
  including ones that did not exist when the cursor was handed out. A client
  that fetches history in `asc` order and remembers the last page's
  `nextCursor` therefore has an exact resume point for a live subscription:
  opening the live stream from that same position (once the SSE task defines
  how a cursor maps to the stream's own resume token) cannot skip an event
  published in the interval, because the interval is not what the cursor
  encodes — the log position is.
- **Event IDs are globally unique and monotonic (ULIDs), so overlap is safe
  to discard.** `docs/design/web-ui.md`'s Home section already states the
  rule this protocol relies on: deduplicate by event ID. A live subscription
  that conservatively replays a small tail before following forward (the same
  shape the existing bus SSE stream and the Go-templated Web UI's relay
  already use, `app/internal/eventbus/server.go` and
  `app/internal/webui/events_stream.go`) may re-deliver an event the history
  page already showed; the client discards it by ID rather than trusting
  either read's timing.

No condition in this handoff is unrecoverable without an explicit path back
to this same endpoint. A live subscription that cannot honor its own resume
cursor (a reconnect after the log rotated) has the identical fallback a
client already needs for a stale REST cursor: drop it and refetch — `order:
desc, limit: N` for "resynchronize the visible window", reconciled against
already-rendered events by ID, or `order: asc` with no cursor to restart
history from the beginning. The read contract this document specifies is
that refetch path; the live task does not need to invent a second one.

`app/internal/webui/acceptance_test.go`'s
`TestAcceptance_ApiV1EventsCursorClosesTheHistoryLiveHandoffGap` demonstrates
the first property against the real service and event-log stack: it fetches
a page and its cursor, publishes another event (the race), then shows that
re-querying with the already-issued cursor returns exactly that event.
