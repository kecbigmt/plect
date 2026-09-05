// GET /api/v1/bootstrap is hand-written (see app/internal/webui/bootstrap.go),
// not part of the generated Session contract in @plecture/web-api: it exists
// only for this client's own startup needs — the API version it is talking
// to, and the CSRF token value the plect_csrf cookie carries. The cookie
// itself is HttpOnly, so this response is the only way the client can learn
// that value to echo back as X-CSRF-Token on a future mutation.
export interface BootstrapInfo {
  apiVersion: string;
  csrfToken: string;
}

// The one version this build was written against. docs/design/web-ui.md
// requires "explicit compatibility checks for independently updated clients
// and servers" — an older/newer plect-web build's bootstrap response must be
// rejected here rather than trusted, since nothing else in this response
// shape changes when the API does.
const SUPPORTED_API_VERSION = "v1";

// Distinguished from other failures so a caller can show "sign in" rather
// than "server unavailable" — retrying a 401 will not fix it.
export class BootstrapAuthError extends Error {
  constructor() {
    super("authentication required");
    this.name = "BootstrapAuthError";
  }
}

// Distinguished from a plain connectivity failure: retrying will not fix a
// version this build does not know how to speak to. Carries whatever the
// server actually sent (unknown, since a malformed body can't be trusted to
// carry a string) so the UI can report it.
export class IncompatibleApiError extends Error {
  constructor(public readonly reportedVersion: unknown) {
    super(`unsupported API version: ${JSON.stringify(reportedVersion)}`);
    this.name = "IncompatibleApiError";
  }
}

function isBootstrapInfo(value: unknown): value is BootstrapInfo {
  return (
    typeof value === "object" &&
    value !== null &&
    typeof (value as Record<string, unknown>).apiVersion === "string" &&
    typeof (value as Record<string, unknown>).csrfToken === "string"
  );
}

export async function fetchBootstrap(): Promise<BootstrapInfo> {
  const res = await fetch("/api/v1/bootstrap", { credentials: "same-origin" });
  if (res.status === 401) {
    throw new BootstrapAuthError();
  }
  if (!res.ok) {
    throw new Error(`bootstrap request failed with status ${res.status}`);
  }
  const body: unknown = await res.json();
  if (!isBootstrapInfo(body)) {
    const reported = (body as { apiVersion?: unknown } | null)?.apiVersion;
    throw new IncompatibleApiError(reported);
  }
  if (body.apiVersion !== SUPPORTED_API_VERSION) {
    throw new IncompatibleApiError(body.apiVersion);
  }
  return body;
}
