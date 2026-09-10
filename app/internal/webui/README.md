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

A `plect-web` run this way (or built to any path other than a release
archive's `bin/plect-web`, alongside its `bin/plect`) has no `plect` CLI
binary shipped next to it: build the matching `plect` CLI from the same
source tree and set `PLECT_BIN` to its absolute path —

```bash
go build -o /tmp/plect ./app/cmd/plect
PLECT_BIN=/tmp/plect go run ./app/cmd/plect-web
```

— rather than pointing it at an already-installed `plect`, which may be a
different version. Without a matching `PLECT_BIN`, a session `plect-web`
starts (`up` from the session tree, a pane, a channel delivery) resolves a
bare `plect` to nothing and cannot reach this process's own store (see
`docs/design/sqlite-persistence.md`, "Data-home resolution").

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
go test -tags browser ./app/internal/webui/         # browser acceptance against the real /app/ build
```

Handlers call `service.*` only through the `SessionService` seam, not directly,
so they can be tested without git / tmux / state.json side effects
(`handlers_test.go`'s `fakeService` injects a fake).

### Browser acceptance (`//go:build browser`)

`browser_test.go` drives a real headless Chromium, via
[`playwright-go`](https://github.com/mxschmitt/playwright-go), against the
committed `/app/` React build — the same real service/state-store seam
`acceptance_test.go` already exercises over plain HTTP
(`httptest.NewServer(New(svc).Routes())`), with a browser on top. It covers
the first Web UI milestone's read/live acceptance scenario: sign-in,
hierarchy navigation, session detail/resource inspection, history reading,
a live event arriving without reload, reconnect after an interruption,
session-switch isolation, keyboard navigation, and a narrow-viewport layout.
Fixtures are seeded directly through `state.Store`/`service.PublishEvent`,
never through a mutation UI.

The `browser` tag is never part of the default `go build`/`go test`
(`playwright-go` is a test-only import: an untagged build or test run never
links it), and needs the pinned Playwright driver + Chromium installed
first:

```bash
go run github.com/mxschmitt/playwright-go/cmd/playwright@v0.6201.1 install --with-deps chromium
```

`--with-deps` needs a package manager Playwright recognizes (CI's
`ubuntu-latest` runner); on a distro it doesn't officially support, drop
`--with-deps` and install the browser's runtime libraries yourself, or set
`PLAYWRIGHT_CLI_PATH`/`PLAYWRIGHT_NODEJS_PATH` to a distro-packaged
Playwright driver (see `playwright-go`'s own `RunOptions` doc comment).

### Manual smoke test (real host, loopback and VPN)

The commands above run against fixtures, not a real host with real
sessions. This is the real-runtime procedure referenced from this
milestone's PR body; it needs an operator with a real `plect-web` deployment
to run and observe, and cannot be automated as a CI check.

1. **Generate an isolated auth token, config, and env file**, so the
   sign-in step below is actually exercised (network trust with no
   `auth_token` is the default, but this milestone's login criterion needs
   the gated path). `plect-web`'s own `config.toml` is the only thing this
   touches: it reads `$HOME/.config/plect-web/config.toml` with no override
   of its own, so a throwaway `$HOME` isolates it completely — nothing
   existing is overwritten:
   ```bash
   smoke_home="$(mktemp -d)"
   smoke_env="$(mktemp)"
   (umask 077 && mkdir -p "$smoke_home/.config/plect-web")
   token="$(head -c32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
   (umask 077 && printf 'auth_token = "%s"\n' "$token" > "$smoke_home/.config/plect-web/config.toml")
   cat > "$smoke_env" <<ENV
   HOME=$smoke_home
   XDG_CONFIG_HOME=$HOME/.config
   XDG_DATA_HOME=$HOME/.local/share
   XDG_CACHE_HOME=$HOME/.cache
   ENV
   printf 'auth_token: %s\n' "$token"   # sign in with this value in step 3
   ```
   `$smoke_env`'s `HOME` isolates only `plect-web`'s own config; its
   `XDG_CONFIG_HOME`/`XDG_DATA_HOME`/`XDG_CACHE_HOME` are captured from the
   real `$HOME` above (before the heredoc), so the real plect declarations,
   session state, and plugin cache this procedure is meant to exercise stay
   real. Only running `plect-web` itself needs `$smoke_env` (step 2, and
   step 8's second bind — reuse the same file rather than generating a new
   one, which would pick a different, token-less `$smoke_home`). Step 5's
   `plect event publish` is a plain CLI command that never reads
   `plect-web`'s config, so it needs none of this, in any terminal.
2. **Build and run** against real state, sourcing `$smoke_env` in a
   subshell so its variables apply only to this one process, never to the
   surrounding shell:
   ```bash
   go build -o /tmp/plect-web ./app/cmd/plect-web
   (set -a && source "$smoke_env" && set +a && /tmp/plect-web --host 127.0.0.1 --port 8787)
   ```
3. **Sign in.** Navigate to `http://127.0.0.1:8787/app/`, confirm the
   redirect to `/login`, sign in with the token step 1 printed, and confirm
   landing back on `/app/`.
4. **Hierarchy and detail.** Confirm the real session tree renders,
   expand/collapse a parent with children, select a session with a real
   `resource_id`/branch, open Details, and confirm the resource renders as
   a link (HTTP(S)) or copyable text (everything else).
5. **History and live update.** Open a session with recorded history,
   confirm it renders, then from another terminal publish a real event
   (`plect event publish <session> --type user.emit --summary "smoke test"`)
   and confirm it appears without a page reload.
6. **Interrupt and recover.** While a session's conversation is open,
   disconnect the host's network (or block the port), publish another event
   from a machine that still has state-store access, reconnect, and confirm
   the event appears exactly once with no gap in what was already shown.
7. **Switch sessions.** Select a session, then before it finishes loading
   select a different one; confirm no cross-session content leaks, then
   switch back and confirm the first session's reading position (scroll
   offset) and selection are unchanged.
8. **VPN access.** Bind to the host's private network address, reusing
   `$smoke_env` from step 1:
   ```bash
   (set -a && source "$smoke_env" && set +a && /tmp/plect-web --host <vpn-ip> --port <port>)
   ```
   Confirm a second device on that network can sign in and reach the same
   milestone. Never commit the actual VPN address used for this check.
9. **Keyboard and narrow layout.** Tab into the session tree, drive it with
   arrow keys/Home/End/Enter, and confirm visible focus rings throughout.
   Resize the browser (or use a phone on the VPN) to a narrow width and
   confirm the sidebar and detail pane become overlays, not persistent
   columns.
10. **Dense tree / long timeline.** Against a real deployment with many
    sessions or a long-running session's history, confirm the tree and
    timeline remain usable (no visible hang, no missing rows) rather than
    only ever exercising the small fixtures above.
11. **Clean up.** Remove the token-bearing directory and env file step 1
    created — nothing else was touched, so this is the only cleanup:
    ```bash
    rm -rf "$smoke_home" "$smoke_env"
    ```

This procedure has no automated pass/fail signal; each step's expected
observation is stated so it can be followed without this repository's issue
tracker.
