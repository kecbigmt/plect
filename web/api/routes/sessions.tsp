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
    (e.g. `acceptance/web-1`). A conformant client percent-encodes `/` as
    `%2F` when substituting a path parameter (RFC 3986) regardless of this
    parameter's `allowReserved` marking — `@typespec/openapi3` does not
    currently emit `allowReserved` into the generated OpenAPI document, so
    generated clients have no signal to do otherwise. The server does not
    depend on that signal either way: it matches this route against the
    request path after the transport layer has already percent-decoded it,
    so both an unencoded `/` and an encoded `%2F` reach the handler as the
    same, correct, full session name — see
    app/internal/webapi/handler_test.go's transport-level test.
    """)
  get(
    @path(#{ allowReserved: true })
    @doc("The exact session name, `/` included.")
    name: string,
  ): SessionDetail | SessionNotFoundResponse | InternalErrorResponse;
}
