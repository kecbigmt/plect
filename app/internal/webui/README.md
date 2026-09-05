# plect-web (webui)

A control-plane web UI for plect session management. The `plect-web` command
embeds this directory's `assets/` via `//go:embed` and serves them.

- **Stack**: Go `html/template` + [htmx](https://htmx.org/) + [Tailwind v4](https://tailwindcss.com/). No React, single binary.
- **Design**: shadcn-inspired. Tokens are generated from [`DESIGN.md`](./DESIGN.md) as the single source.

## Setup

Generating CSS and icons requires Node 22 and pnpm (`corepack pnpm` works if you
don't have pnpm installed otherwise). Building and running the binary itself is
Go-only — generated assets are committed to the repo.

```bash
pnpm install            # dev dependencies only
```

## Running

```bash
go run ./app/cmd/plect-web                          # http://127.0.0.1:8787 (default: loopback)
go run ./app/cmd/plect-web --host 0.0.0.0 -p 8799   # expose on a private network / VPN
```

The bind address is set via `--host` / `--port` (`-p`), or `listen_addr` in
`~/.config/plect-web/config.toml`. It defaults to loopback so a fresh install
doesn't accidentally expose itself on every interface.

## React shell (`/app/`)

`GET /app/` serves the embedded production build of the React/TypeScript
shell in [`web/app`](../../../web/app/README.md) (the `webapp` subpackage's
`//go:embed`) — a development/review entry alongside this package's own
production UI at `/`, per the client/server boundary ADR. It sits behind the
same `authMiddleware`/`csrfMiddleware` chain as every other route, and
`GET /api/v1/bootstrap` (`bootstrap.go`) is that shell's own hand-written
entry for its startup needs (API version, CSRF token value) — see that
file's doc comment for why it stays outside the generated Session contract
in `app/internal/webapi`.

## Security (mutating operations)

create / up / down / destroy change state. Defense in depth lives in `security.go`:

- **CSRF** (always on): mutating POSTs require **same-origin** (Origin/Referer
  host == Host) and a **double-submit token** (`plect_csrf` cookie ==
  `X-CSRF-Token` header). The token is issued when a GET page renders and baked
  into `<body hx-headers>` so every htmx request carries it automatically. The
  cookie is SameSite=Strict + HttpOnly. `/login` is exempt from CSRF since it's
  pre-auth.
- **auth_token** (optional): setting `auth_token` in `config.toml` locks the
  whole UI behind authentication. Unauthenticated GETs redirect to
  `/login?next=<original path>` (so a successful login returns where the
  caller was headed); everything else gets 401. A request under `/api/`
  always gets a JSON 401 instead of the HTML redirect/error a browser
  navigation gets — a JSON client following a redirect would otherwise parse
  a sign-in page as its response. Passes with `Authorization: Bearer <token>`
  or the login cookie. `/login`, `/static`, and `/healthz` are exempt (`/app/`
  is not — it redirects to sign-in like `/`). If unset, the UI trusts
  whatever network it's reachable on — additional defense for when it's
  exposed over a private network / VPN.

## Build (CSS / icons)

After changing templates, `DESIGN.md`, or the icon list, regenerate the build
artifacts and commit them.

```bash
pnpm build              # icons → tokens → app.css generation + htmx copy (all-in-one)
pnpm icons              #   lucide-static → components/icons/*.html
pnpm tokens             #   DESIGN.md → theme.generated.css (+ WCAG contrast lint)
```

```
DESIGN.md ─tokens→ theme.generated.css ┐
lucide-static ─icons→ components/icons/ ├─build→ assets/static/app.css (+ htmx copy)
input.css + templates ──────────────────┘
```

Generated artifacts (`assets/static/app.css` / `htmx.min.js` /
`theme.generated.css` / `components/icons/`) are committed to the repo. The
`plect-web` CI workflow runs `pnpm build` and fails if it produces an
uncommitted diff. The Go build itself never invokes pnpm/node — it only embeds
the committed assets.

## Directory layout

```
DESIGN.md             single source for design tokens (colors / typography / rounded / components)
input.css             Tailwind entry point. Pulls in theme.generated.css and all templates
scripts/gen-icons.mjs generates icon partials from lucide-static
assets/
  static/             embedded/served build output (app.css, htmx.min.js)
  templates/
    *.html            pages and partials (list shell / rows / detail-pane / login / error / mutations)
    components/       reusable partials, one file per component (badge / button / card / input / dialog)
    components/icons/ generated icon partials
*.go                  server, routing, handlers, service seam, template FuncMap
theme.generated.css   generated file (do not edit by hand)
```

Templates are referenced by their `{{define}}` name, so file layout is free-form.
`server.go`'s ParseFS reads `assets/templates` recursively, and `input.css`'s
`@source` glob is `**/*.html`, so adding a subdirectory is picked up automatically.

## Common tasks

- **Change a token** — edit `DESIGN.md` → `pnpm build` → commit the generated files.
- **Add a component** — add `{{define "<name>"}}…{{end}}` in `components/<name>.html`.
  html/template has no slots, so props are passed via `dict`:
  `{{ template "button" (dict "variant" "outline" "label" "…") }}`. Run `pnpm build`
  if you used a new utility class.
- **Add an icon** — add a [lucide](https://lucide.dev/icons/) name to `ICONS` in
  `scripts/gen-icons.mjs` and run `pnpm build`. Use it as
  `{{ template "icon-git-branch" "size-3" }}` (the class argument controls
  size/color; default is `size-4`, `currentColor`, `aria-hidden`).
- **Add a page** — drop a `.html` file in `templates/` and register a handler in
  `server.go`'s `Routes()`.

## Accessibility baseline

Semantic HTML, appropriate `role`/`aria-*`, `<label>` associations,
`:focus-visible` rings, WCAG AA contrast, native `<dialog>` for modals. The
5-second auto-refreshing list intentionally avoids `aria-live` so it doesn't
spam screen readers. New UI should meet this same bar.

## Testing

```bash
go test ./app/internal/webui/                       # unit (template Execute + role/aria assertions)
go test -tags integration ./app/internal/webui/     # acceptance tests against a real state store
```

Handlers call `service.*` only through the `SessionService` seam, not directly,
so they can be tested without git / tmux / state.json side effects
(`handlers_test.go`'s `fakeService` injects a fake).
