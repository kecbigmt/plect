# Plecture Web UI shell

The production React/TypeScript Web UI shell — see
[docs/design/web-ui.md](../../docs/design/web-ui.md) for the interaction
design and [the client/server boundary ADR](../../docs/adr/2026-09-05-web-ui-client-server-boundary.md)
for the architecture decision this package implements.

This package is a member of the `web/` pnpm workspace alongside
[`web/api`](../api/README.md), and depends on it as `@plecture/web-api` via
`workspace:*` for the generated Session contract types and the
`openapi-fetch` client.

## Entry points

- `/` (production) still serves the existing Go-templated htmx UI
  (`app/internal/webui`) — unchanged, and remains the production entry until
  an explicit later cutover.
- `/app/` (this package's development/review entry) serves this shell,
  embedded from a build produced at release time (see Build below). It
  carries no compatibility promise beyond this milestone.

Both sit behind the same `authMiddleware`/`csrfMiddleware` chain
(`app/internal/webui/security.go`): an unauthenticated navigation to `/app/`
redirects to `/login?next=/app/` exactly as `/` does.

## Development

`pnpm dev` runs the Vite dev server (default `http://127.0.0.1:5173`) with
`/api`, `/login`, and `/healthz` proxied to a separately running
`plect-web`, so the browser sees one origin throughout — the
`plect_csrf`/`plect_auth` cookies are `HttpOnly` and `SameSite=Strict`, so a
cross-origin dev server could neither receive nor resend them.

```sh
go run ./app/cmd/plect-web &            # the real backend, default :8787
cd web && pnpm install --frozen-lockfile
cd app && pnpm dev                      # http://127.0.0.1:5173/app/
```

Set `PLECT_WEB_ORIGIN` (see `vite.config.ts`) if the backend runs somewhere
other than the default `http://127.0.0.1:8787`.

## CSRF

The `plect_csrf` cookie is `HttpOnly`, so this app cannot read it directly.
`GET /api/v1/bootstrap` (hand-written — see `app/internal/webui/bootstrap.go`
and [`web/api/README.md`](../api/README.md)'s "Authentication/bootstrap"
note) mints/returns the cookie's current value in its JSON body; a future
mutating request echoes it back as `X-CSRF-Token`. `src/lib/bootstrap.ts` and
`src/lib/useBootstrap.ts` are this app's own client for it.

## Build

```sh
pnpm build   # tsc --noEmit && vite build -> ../../app/internal/webui/webapp/dist/
```

The output is gitignored (only a `dist/.gitkeep` placeholder is committed,
so `go install`'s installability invariant still compiles from committed
module source alone — see `app/internal/webui/webapp/embed.go`). Without
this build, `plect-web` serves a plain "Web UI not built" notice on every
`/app/` route instead of the shell. CI's `web-app-build` job
(`.github/workflows/ci.yml`) builds and tests this package from source on
every production-path change, without comparing its output to Git.

## Testing

```sh
pnpm test   # vitest run
```
