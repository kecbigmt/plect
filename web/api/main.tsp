import "@typespec/http";
import "@typespec/openapi";
import "@typespec/openapi3";
import "./models/common.tsp";
import "./models/session.tsp";
import "./routes/sessions.tsp";

using Http;
using OpenAPI;

/// Plecture's Web API: JSON read projections of session state for the
/// browser/desktop UI, over the existing Go service layer, per the
/// schema-contract decision record under docs/adr/. This is the initial,
/// version-1 slice — Session list/detail and common errors only; it does not
/// cover Tasks, Graph, Terminal, or any mutation.
@service(#{ title: "Plecture Web API" })
@server("/api/v1", "The API is served from the same origin as the Web UI, under a fixed version prefix.")
@doc("""
  Authentication and CSRF protection are enforced by the same HTTP-boundary
  middleware the existing Go-templated Web UI uses (an opaque bearer token in
  the `plect_auth` cookie, checked outside of generated code) — this contract
  does not add, replace, or generate that enforcement. Loopback binds with no
  token configured require none; a configured token is mandatory on every
  request. Unauthenticated and unauthorized requests are out of this slice's
  documented response set, matching the existing Web UI's behavior of
  redirecting to a login page rather than returning a JSON error body.
  """)
@useAuth(ApiKeyAuth<ApiKeyLocation.cookie, "plect_auth">)
namespace Plecture.WebApi;
