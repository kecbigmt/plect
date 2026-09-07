# Container image (phase 1: standalone ECS deployment)

A generic base image for running `plect serve` as an independent container,
per the investigation and owner ruling on
[#309](https://github.com/kecbigmt/plecture/issues/309) and
[#313](https://github.com/kecbigmt/plecture/issues/313). It ships `plect`,
the official catalog's plugin executables, the agent CLIs, an outer process
supervisor, and a neutral example config baked at `/etc/plect` — enough to
build, run, and inspect locally. It is not, by itself, a deployment: the
actual AWS/ECS Terraform, and any team-specific workflow config, are
out of scope here (see "Extending this image" below).

## What's in the image

- `plect`, built from this repository's `app/` module.
- The official catalog's `tmux`, `claude`, and `github` plugins, enabled
  against a `path://` catalog baked into the image (see the Dockerfile's own
  comments for why a path catalog, not `git+https://`, is the right choice
  for a container whose config never changes without a rebuild).
- `git`, `gh`, `jq`, `tmux`, Node.js (only to run the two agent CLIs below),
  Claude Code, and Codex CLI — every version pinned explicitly in the
  Dockerfile, no `@latest`/`next`.
- `runit` as the outer supervisor, and a small `entrypoint.sh` that owns PID
  1 (see "Process model" below).
- `/etc/plect`: a neutral example config — `config.toml`, `catalogs.toml`,
  `plect.lock`, and one example workflow
  (`workflows/example_work.toml`) that composes the enabled plugins into a
  dispatchable session (GitHub issue/PR worktree → tmux pane → Claude Code,
  authenticated as a GitHub App installation via `gh_app_guard` instead of
  an operator's personal `gh auth login`).

## Build

Pass the exact commit as a build arg — the Dockerfile requires
`PLECT_SOURCE_SHA` (a well-formed 40-character commit SHA) and fetches the
source it packages (`app/`, `contracts/`, `plugins/`, and this directory's
own scripts/example config) directly from `PLECT_SOURCE_REPO` at that
commit, rather than reading the local build context — so a locally-edited
or stale working tree can never produce an image that stamps a SHA it
didn't actually build from:

```bash
docker build -f deploy/docker/Dockerfile \
  --build-arg PLECT_SOURCE_SHA="$(git rev-parse HEAD)" \
  -t plecture-base:<tag> .
```

(`PLECT_SOURCE_REPO` defaults to this repository's own GitHub URL; a
build from a fork or a private mirror overrides it as a second
`--build-arg`.)

Pass `PLECT_VERSION` (e.g. `0.2.0`) to stamp the built `plect` binary as a
release build, the same `-ldflags -X
github.com/kecbigmt/plecture/app/internal/version.Current=<version>` that
`.github/workflows/release.yml` stamps into the released archives:

```bash
docker build -f deploy/docker/Dockerfile \
  --build-arg PLECT_SOURCE_SHA="$(git rev-parse HEAD)" \
  --build-arg PLECT_VERSION=0.2.0 \
  -t plecture-base:<tag> .
```

Left unset (the default), the image keeps `Current`'s `"0.0.0-dev"`
placeholder, so `version.IsDevelopmentBuild()` stays true and the
migration access gate (`docs/design/sqlite-persistence.md`'s "Migration
access gate") refuses to forward-migrate `storage.db` against an existing
schema — the right default for an image built from a non-release
`PLECT_SOURCE_SHA`. Only set `PLECT_VERSION` when `PLECT_SOURCE_SHA` names
an actual release tag's commit.

That SHA is stamped into the built image (the `org.opencontainers.image.revision`
label, and a plain `/etc/plecture-source-revision` file readable from inside
the running container) — a way to confirm, after the fact, exactly which
commit a given image or running container was built from:

```bash
docker inspect --format='{{index .Config.Labels "org.opencontainers.image.revision"}}' <image>
docker exec <container> cat /etc/plecture-source-revision
```

The build otherwise needs only network access (to fetch that source, Go
modules, the `gh`/Node.js release tarballs, and the two npm packages) — no
local checkout of this repository is required at all, only the Dockerfile
itself.

## Process model

One container, one supervised top-level process: `plect serve`. It owns
plugin `[[services]]` itself (their lifecycle is unaffected by anything in
this directory — see `app/commands/serve.go`); nothing else needs a
top-level supervisor slot in phase 1 (no `plect-web`, no resident
`plect mcp listen` — see the owner ruling on #309).

Supervisor choice: **runit**, over supervisord and s6-overlay. With exactly
one supervised process, the job reduces to "start it, restart it on crash,
and let a graceful SIGTERM through" — `runsvdir`/`runsv`/`chpst` are three
small, dependency-free binaries that do exactly that, next to supervisord's
whole Python runtime and s6-overlay's larger bundle/dependency-file ceremony
built for coordinating many services.

`entrypoint.sh` is PID 1 itself (not `tini` or another minimal init): it has
to run once as root to fix ownership on a freshly attached ECS volume before
anything drops privilege, and modern bash (≥ 4.4) already reaps arbitrary
reparented children when it runs as PID 1 — the one property a real init
would otherwise be needed for here. It traps `SIGTERM`/`SIGINT` and turns
them into `sv down` on the `plect-serve` service, because `runsvdir` does not
itself forward signals to services it supervises (runit's model routes
control through `sv`); without that trap, `plect serve`'s own graceful
shutdown (`app/commands/serve.go`'s `signal.NotifyContext`) would never see
the signal ECS sends on task stop, and every stop would end in a SIGKILL
after the stop timeout instead of an orderly drain.

## Runtime layout and persistence

Mount one volume at `/var/lib/plect`. Everything under it survives a
container restart; everything else is recreated on boot.

| Env var | Value | Persisted? |
|---|---|---|
| `PLECT_CONFIG_HOME` | `/etc/plect` | Baked into the image, not the volume — a config change is an image rebuild (see "Extending this image"). |
| `HOME` | `/var/lib/plect/home` | Yes — agent CLI login/session history (`~/.claude`, `~/.codex`), any plugin cache under `~/.cache`. |
| `XDG_DATA_HOME` | `/var/lib/plect/data` | Yes — `plect` durable state (`storage.db`) and plugin durable data (e.g. `github-watcher`'s subscription registry). |
| `XDG_STATE_HOME` | `/var/lib/plect/state` | Yes — plugin runtime state (e.g. agent activity-probe records). |
| `XDG_RUNTIME_DIR` | `/run/plect` | **No** — UDS paths (the bus socket). Recreated fresh every boot by `entrypoint.sh`; a stale socket path from a previous boot must never be reused. |
| `PLECT_BUS_SOCKET` | `/run/plect/bus.sock` | No (under `XDG_RUNTIME_DIR`). |
| — | `/var/lib/plect/workspace_dirs` | Yes — git worktrees (`workspace_dirs_root` in the example `config.toml`). |
| — | `/var/lib/plect/codex-exec` | Yes, if a downstream config wires `official.codex.exec_runtime`'s `state_root` here — otherwise unused; created regardless so that wiring needs no image change. |

Restart behavior to expect: `plect serve` and plugin services restart and
resume from persisted state; the bus healthcheck passes again once `plect
serve` is back up. tmux panes, agent CLI processes, and anything under
`/run/plect` do not survive — an existing session's runtime (its pane, its
agent process) needs `plect up <session>` after a restart, the same as after
any host reboot. This is exactly the #309 investigation's "Restart mid-
session" section; nothing about running in a container changes it.

## Secrets and configuration

Nothing is required at boot for the image to come up healthy — `plect
serve` and `github-watcher` (this example's only plugin service with no
`required_env`) start regardless. Everything below is required only to
actually *use* what it gates:

| Input | Gates | Notes |
|---|---|---|
| GitHub App id/installation id/private key path | The example workflow's `gh_app_guard` node, and `official.github.worktree`'s own git/metadata App auth | Operator-provisioned, never committed — see `plugins/github/README.md`'s "App auth" section. Passed as session inputs at `plect up` time in this example; a downstream deployment may instead hardcode its own App identity into its own overlay workflow. |
| `GITHUB_WATCHER_APP_ID` (+ `GITHUB_WATCHER_APP_INSTALLATION_ID`/`_OWNER`/`_REPO`/`_PRIVATE_KEY_PATH`) | `github-watcher serve`'s resident polling and its `gh-api` helper | Deployment-level container env vars (`docker run -e ...`, or the task definition's own env), not session inputs — one GitHub App installation shared by every session's subscriptions. See `plugins/github/README.md`'s "App auth" section. Unset means the watcher falls back to ambient `gh auth` (next row); set, it fails startup loudly on an unreadable key or a rejected mint rather than staying process-healthy while every fetch dies. |
| `SLACK_BOT_TOKEN` (+ `SLACK_APP_TOKEN` for inbound) | The `slack-adapter` plugin service, if a downstream deployment enables the `slack` plugin | Not enabled in this image's example config. |
| Agent CLI login (`claude`, `codex`) | Actually launching a session | Interactive, via CloudShell — see "First boot" below. Not an env var; it's state written under the persisted `$HOME`. |
| `gh auth login` | `github-watcher`'s own GitHub API calls, only if `GITHUB_WATCHER_APP_ID` (above) is unset | Fallback only. A fully App-only deployment sets the `GITHUB_WATCHER_APP_*` env vars instead and never runs this — see `plugins/github/README.md`'s "One credential story for an App-only deployment". If run, it's interactive, under the persisted `$HOME`, same as agent CLI login. |

No secret is ever baked into the image or into `/etc/plect`; the example
workflow's `private_key_path` input, and the `GITHUB_WATCHER_APP_PRIVATE_KEY_PATH`
env var if the deployment sets watcher App auth, are each a path an
operator provides at deploy/dispatch time, read at 0600 from wherever
Secrets Manager (or an equivalent) lands it on the container's filesystem.

## First boot

1. Deploy the container with the persistent volume mounted and start it —
   `plect serve` comes up with no session yet.
2. Exec into the running container as the `plect` user (CloudShell, or
   locally: `docker exec -u plect -it <container> bash`).
3. Run `claude login` (or `codex login`, if the deployment uses Codex). It
   persists under `$HOME` (`/var/lib/plect/home`), which is on the volume.
   An App-only deployment needs no further step here: `github-watcher`
   authenticates from the `GITHUB_WATCHER_APP_*` env vars set on the
   container (see "Secrets and configuration" above), never from `$HOME`
   state. Only a deployment that left those env vars unset runs
   `gh auth login` here, persisting the same way as the agent CLI login.
4. Restart the container once and confirm the CLIs still see their auth —
   `claude --version`/re-running the login command should now report an
   existing session rather than prompting again.
5. Dispatch a real session against a throwaway/disposable resource to
   confirm the whole chain — worktree acquisition, guarded `gh`, agent
   launch, event delivery — actually works end to end:
   `plect up <issue-or-pr-url> --workflow example_work --inputs '{"app_id":"...","private_key_path":"...","prompt":"..."}' --detach`,
   then `plect status <session>`.

## Deploying

Manual, from the owner's machine, for phase 1 (no CI image-build workflow —
see the issue's own "Verification is one-time PR evidence" acceptance
criterion; this PR does not add one):

```bash
docker build -f deploy/docker/Dockerfile \
  --build-arg PLECT_SOURCE_SHA="$(git rev-parse HEAD)" \
  --build-arg PLECT_VERSION=<tag without the leading v, e.g. 0.2.0> \
  -t <registry>/<repo>:<tag> .
docker push <registry>/<repo>:<tag>
# then: update the ECS service/task definition to the new tag (Terraform,
# in the team's own deployment repository — out of scope here).
```

`PLECT_VERSION` only makes sense when `PLECT_SOURCE_SHA` names a released
tag's commit — see "Build" above.

## Extending this image

The base image built here ships a **neutral** example. A downstream
deployment (its own team-specific workflow config and tuning) builds its own
image `FROM` this one and overlays `/etc/plect`:

```dockerfile
FROM <registry>/<repo>:<tag>
COPY --chown=root:root your-config/ /etc/plect/
```

That downstream `COPY` can add new files under `/etc/plect/workflows/`,
replace `config.toml`'s `resource_allowlist`/`workspace_dirs_root`, or add a
same-id override in a new file (user-owned config always wins over a plugin
layer's, see `docs/design/plugin-packaging.md`'s Shadowing and precedence
section) — none of it requires a change in this repository. Enabling a
plugin this base image doesn't already mount is the one case that needs its
own `plect catalog add`/`plect plugin add` step in the downstream
Dockerfile, against either this image's own baked catalog
(`path:///opt/plect-repo`, `subdir=plugins`) or a catalog of the downstream
deployment's own choosing.

Design test (per the issue's AMENDED scope): a downstream config change —
a different `resource_allowlist`, a new workflow, a team's own
`gh_app_guard` App identity — must need zero commits in this repository.
Anything a downstream deployment would need to fork this repository for is
a design smell in this image, not in the downstream one.

## Operations

- Container stdout/stderr (both `plect serve` and every plugin service log
  there — see `app/commands/serve.go`) is the log stream; point it at
  whatever the deployment's log driver sends to CloudWatch.
- Health: `HEALTHCHECK` runs `curl --unix-socket "$PLECT_BUS_SOCKET"
  http://localhost/healthz` — the one route the bus server never gates on
  `PLECT_BUS_TOKEN`.
- CLI diagnostics from CloudShell (as the `plect` user):
  `plect ls`, `plect status <session> --json`, `plect event list <session>`,
  `github-watcher list`.

## Local verification (PR evidence)

Built and run locally (not on ECS) against this Dockerfile, with a Docker
named volume standing in for the ECS volume:

```bash
docker build -f deploy/docker/Dockerfile \
  --build-arg PLECT_SOURCE_SHA="$(git rev-parse HEAD)" \
  -t plecture-base:test .
docker volume create plecture-test-data
docker run -d --name plecture-test -v plecture-test-data:/var/lib/plect plecture-base:test
```

Observed:

- Build completed with every pin resolved (no `@latest`), producing an
  image with `plect`, the three enabled plugin executables (including the
  two Go-built ones, `channel-server` and the `github` plugin's binaries),
  and the agent CLIs.
- `plect serve` came up and logged its listen line; `github-watcher` (the
  one enabled plugin service with no `required_env`) started under it;
  `channel-server` (required env intentionally never satisfied by the
  resident process — see `plugins/claude/plugin.toml`) correctly did not.
- Docker's `HEALTHCHECK` reported `healthy` within its `start-period`.
- Process tree: `entrypoint.sh` (root, PID 1) → `runsvdir`/`runsv` (root) →
  `plect serve` and its child `github-watcher` (both uid 10000, the `plect`
  user) — no root-owned application process, no stray zombie or extra
  service (the `runit` package's own placeholder `default-syslog` service
  is explicitly removed in the Dockerfile).
- `docker stop -t 30` exited in under a second with exit code 0 (a graceful
  shutdown via the `SIGTERM` → `sv down` path, not a SIGKILL after the
  timeout).
- `docker start` on the same volume: `plect serve` and `github-watcher` came
  back up, the healthcheck went `healthy` again, and a file written under
  `/var/lib/plect/data` before the stop was still present after the
  restart.
- Building without `PLECT_SOURCE_SHA`, or with a value that isn't a
  40-character hex SHA, fails the build at that check, before anything
  else runs; a well-formed but nonexistent SHA fails at the `git fetch`
  step instead (`upload-pack: not our ref`). Building with a real, pushed
  commit SHA — deliberately one behind the current, uncommitted working
  tree, to prove the source stage isn't reading local disk — produced an
  image whose `org.opencontainers.image.revision` label,
  `/etc/plecture-source-revision`, and packaged `entrypoint.sh` content
  all matched that older commit exactly, not the newer local edits.
- `entrypoint.sh`'s path guard: a normal boot (no env override) is
  unaffected; `-e HOME=/etc`, `-e HOME=/var/lib/plect/../../etc` (a `..`
  traversal that resolves outside `/var/lib/plect` despite the literal
  string starting with it), and `-e XDG_RUNTIME_DIR=/` are each refused
  with a loud error before any `mkdir`/`chown` runs, instead of the
  container silently touching a path outside its expected roots.
