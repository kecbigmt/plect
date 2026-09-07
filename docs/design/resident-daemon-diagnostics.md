# Resident daemon diagnostics

`plect serve` is a long-running process with no per-invocation boundary to
reload state at, unlike a CLI subcommand. `SIGQUIT` dumps goroutine stacks
to stderr but also kills the process (systemd then restarts it), so it
costs an operator the very state they wanted to inspect. The daemon's UDS
HTTP server carries a diagnostic surface for exactly this: an operator
inspects a running daemon without killing or restarting it.

## Surface

The bus's UDS HTTP server (`app/internal/eventbus`) serves
`net/http/pprof`'s standard handlers under `/debug/pprof/`, registered on
the server's own `http.ServeMux`, not on `net/http/pprof`'s package-level
`DefaultServeMux`. Every pprof route sits behind the same request pipeline
as `/v1/events` and `/v1/stream`.

A goroutine dump and a CPU profile are both available without restarting
the daemon:

```
GET /debug/pprof/goroutine?debug=1   # stack dump, human-readable
GET /debug/pprof/profile?seconds=10  # 10s CPU profile, pprof format
```

The full standard set (`cmdline`, `profile`, `symbol`, `trace`, `goroutine`,
`heap`, `allocs`, `block`, `mutex`, `threadcreate`) is registered; nothing
beyond what `net/http/pprof` itself exposes is added.

## Trust boundary

Identical to every other bus route: the socket is created `0600`, so a
same-user process needs no token, and `PLECT_BUS_TOKEN` additionally
requires `Authorization: Bearer <token>` on every route except `/healthz`
when set (e.g. a bus proxied to a browser). A profile or goroutine dump can
reveal in-flight data (event payloads referenced by live goroutines, request
bodies), so pprof carries the same authentication as the rest of the API,
never a weaker one — there is no route that skips `s.auth`.
