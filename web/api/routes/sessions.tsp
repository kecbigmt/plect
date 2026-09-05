import "@typespec/http";

using Http;

namespace Plecture.WebApi;

model SessionListResponse is ListResponse<SessionSummary> {}

@error
model InternalErrorResponse {
  @statusCode statusCode: 500;
  @body body: ExecutionError;
}

@error
model SessionNotFoundResponse {
  @statusCode statusCode: 404;
  @body body: NotFoundError;
}

@route("/sessions")
@tag("Sessions")
interface Sessions {
  @get
  @doc("List every tracked session, ordered ascending by session name (not by recency or activity). No pagination cursor in this slice — see the design's Home/cross-session views for a bounded, cursor-paged read.")
  list(): SessionListResponse | InternalErrorResponse;

  @get
  @doc("""
    A single session's identity and runtime facts. Session names contain `/`
    (e.g. `acceptance/web-1`). This parameter does not declare
    `allowReserved`: `@typespec/openapi3` does not currently emit that
    OpenAPI 3 property (compiling this source warns and drops it), so
    declaring it here would assert a contract guarantee the emitted document
    does not actually carry. A generated client therefore percent-encodes
    `/` as `%2F` per RFC 3986's ordinary path-segment escaping — verified for
    the committed client in web/api/verify/client-transport.mjs. The server
    does not depend on a client doing otherwise: it matches this route
    against the request path after the transport layer has already
    percent-decoded it, so both an unencoded `/` and an encoded `%2F` reach
    the handler as the same, correct, full session name — verified in
    app/internal/webapi/transport_test.go through a real HTTP server.
    """)
  get(
    @path
    @doc("The exact session name, `/` included.")
    name: string,
  ): SessionDetail | SessionNotFoundResponse | InternalErrorResponse;
}
