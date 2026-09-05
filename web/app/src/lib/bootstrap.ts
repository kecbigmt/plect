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

// Distinguished from other failures so a caller can show "sign in" rather
// than "server unavailable" — retrying a 401 will not fix it.
export class BootstrapAuthError extends Error {
  constructor() {
    super("authentication required");
    this.name = "BootstrapAuthError";
  }
}

export async function fetchBootstrap(): Promise<BootstrapInfo> {
  const res = await fetch("/api/v1/bootstrap", { credentials: "same-origin" });
  if (res.status === 401) {
    throw new BootstrapAuthError();
  }
  if (!res.ok) {
    throw new Error(`bootstrap request failed with status ${res.status}`);
  }
  return (await res.json()) as BootstrapInfo;
}
