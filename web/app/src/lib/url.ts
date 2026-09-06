// Mirrors app/internal/webui/server.go's isWebURL: resource ids are
// arbitrary strings (a resolver-less id like "my-experiment", or a non-web
// scheme like "acme:foo"), so only an actual http(s) URL renders as a link
// — docs/design/web-ui.md's "HTTP(S) resources open as links; other
// identifiers remain copyable."
export function isWebUrl(value: string): boolean {
  try {
    const url = new URL(value);
    return (url.protocol === "http:" || url.protocol === "https:") && url.host !== "";
  } catch {
    return false;
  }
}
