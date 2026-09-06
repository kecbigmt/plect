import "@typespec/http";

using Http;

namespace Plecture.WebApi;

@error
model EventValidationErrorResponse {
  @statusCode statusCode: 400;
  @body body: ValidationError;
}

@route("/events")
@tag("Events")
interface SessionEvents {
  @get
  @doc("""
    One page of a session's durable event history —
    `app/internal/service/event.go`'s `EventPage`, unchanged. `session` rides
    as a query value (not a path segment): session names contain `/`, and
    this contract has no need to reserve a literal path segment (`events`)
    that could collide with one.

    Unlike `GET /sessions/{name}`, an unknown session is not a 404: the event
    log is independent of session-tree membership (a destroyed session keeps
    its log), so a session that was never created simply has an empty
    log — a 200 with an empty `events` array and no `nextCursor`, the same
    "missing is empty, not an error" answer the CLI's own event commands
    already give.

    `order` defaults to `asc` (oldest first): it paginates forward from the
    log's head, or from `cursor` (a prior page's `nextCursor`, opaque — never
    constructed or inspected by a client), and returns a `nextCursor`
    whenever the log exists, independent of whether that particular page had
    any matching events. `desc` (newest first) returns only the single most
    recent page bounded by `limit` and never a `nextCursor`, matching
    `EventPage`'s v1 contract exactly — no backward pagination is promised.

    A `cursor` from a mismatched `order`, or one issued against a log
    generation that no longer exists (the log rotated), is rejected as
    invalid input rather than silently resolved against the wrong position —
    the client's only recovery is to drop it and restart from the beginning
    (equivalently, refetch with no `cursor`).
    """)
  list(
    @query
    @doc("The exact session name, `/` included, sent unencoded as a query value.")
    session: string,

    @query
    @doc("A prior page's `nextCursor`. Omitted requests the first page.")
    cursor?: string,

    @query
    @doc("Page size. Omitted or non-positive uses the server's existing default.")
    limit?: int32,

    @query
    order?: EventOrder,
  ): EventPage | EventValidationErrorResponse | InternalErrorResponse;
}
