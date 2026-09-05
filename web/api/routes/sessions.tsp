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
    (e.g. `acceptance/web-1`); send it percent-encoded (`%2F`) like any other
    reserved character in a path-parameter value (RFC 3986). The server
    accepts either an encoded or a literal `/` and resolves both to the
    same, correct, full session name.
    """)
  get(
    @path
    @doc("The exact session name, `/` included.")
    name: string,
  ): SessionDetail | SessionNotFoundResponse | InternalErrorResponse;
}
