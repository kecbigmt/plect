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
  @doc("List every tracked session, newest name-sort order. No pagination cursor in this slice — see the design's Home/cross-session views for a bounded, cursor-paged read.")
  list(): SessionListResponse | InternalErrorResponse;

  @get
  @doc("A single session's identity and runtime facts. Session names contain `/` (e.g. `acceptance/web-1`); `name` is a reserved-character (`allowReserved`) path parameter, so a `/` in it is sent literally rather than percent-encoded, matching how the session-name space is already addressed elsewhere in this API's server implementation.")
  get(
    @path(#{ allowReserved: true })
    @doc("The exact session name, `/` included. Sent unencoded (`allowReserved`) rather than as `%2F`.")
    name: string,
  ): SessionDetail | SessionNotFoundResponse | InternalErrorResponse;
}
