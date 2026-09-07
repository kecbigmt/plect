# Plecture

**Give autonomous work a place to go.**

Plecture is a local-first system for giving autonomous work durable structure. It ships as a CLI called `plect`.

`plect` gives autonomous work **identity**, **lifecycle**, **relationships**, **observation**, **verification**, and **handoff**. Work lives in inspectable, portable state that you own — a place to exist outside any single agent's context, and a place to move on from: back to a worker, on to a reviewer, up to a human, through verification to done.

It is not an agent framework. It is not a workflow engine that dictates execution. It does not hide your tools behind a common abstraction. Plecture owns the semantics of work; the mechanisms that carry it out remain yours to choose and to swap. Claude Code or Codex. Git or another VCS. Worktrees or containers. tmux or another process host. These are choices at the edge, not assumptions at the core.

## Install

Download a release archive for your platform from the
[latest release](https://github.com/kecbigmt/plecture/releases/latest) and
extract `bin/plect` (and `bin/plect-web`, the Web UI server) onto your
`PATH`. Verify the download against the release's `SHA256SUMS`.

Alternatively, build `plect` from source — this requires a Go toolchain and
the C compiler `github.com/mattn/go-sqlite3` needs, and it does not build
`plect-web` (its Web UI is not part of a plain source checkout; install it
from a release archive instead):

```bash
go install github.com/kecbigmt/plecture/app/cmd/plect@latest
```

A plugin's own executables (e.g. a GitHub or Slack adapter) are built by
`plect` from its pinned catalog on the host; a Go toolchain remains a host
requirement for any enabled catalog plugin with a build step.

## Quick start

```bash
plect --help
```

## Layout

```
app/         CLI + MCP server: session lifecycle, task DAG, state, dispatch
contracts/   Shared data contracts between the CLI and plugins
plugins/     Distributable packages: executable adapters and shipped config
             for a particular technology (GitHub, Slack, tmux-based agent
             runtimes, ...) — see plugins/catalog.toml
```

`app`, `contracts/*`, and `plugins/*` are independent Go modules wired
together by the repository's `go.work`.

## Build

`app` and each package under `contracts/` and `plugins/` is its own Go
module — `go.work` at the repository root wires them together for editor
tooling, but a bare `go build ./...` from the root doesn't resolve (the root
itself isn't a module). Build per module instead:

```bash
go build ./app/...
go build ./contracts/atomicfile/...
go build ./contracts/channel-protocol/...
go build ./contracts/event/...
go build ./contracts/state/...
go build ./plugins/claude/src/channel-server/...
go build ./plugins/github/src/...
go build ./plugins/legacy-migration/...
go build ./plugins/okf/src/...
go build ./plugins/slack/src/slack-adapter/...
```

## Test

```bash
go test ./app/...
go test ./contracts/atomicfile/...
go test ./contracts/channel-protocol/...
go test ./contracts/event/...
go test ./contracts/state/...
go test ./plugins/claude/src/channel-server/...
go test ./plugins/github/src/...
go test ./plugins/legacy-migration/...
go test ./plugins/okf/src/...
go test ./plugins/slack/src/slack-adapter/...
```

## Deploy

[`deploy/docker/`](deploy/docker/) builds a standalone container image
running `plect serve` — see [`deploy/docker/README.md`](deploy/docker/README.md)
for the build, runtime layout, and first-boot procedure.

## License

Apache License 2.0 — see [LICENSE](./LICENSE).
