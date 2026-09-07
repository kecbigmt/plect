# SQLite durable storage: pre-rollout validation

This is the evidence record for kecbigmt/plecture#437's pre-rollout
validation half: the seven scenarios listed in the issue's 2026-09-07
amendment, run against a **copy** of the devbox host's legacy data
directory, using the **v0.1.0 release archive** (`plect_0.1.0_linux_amd64`)
rather than a source build for scenarios 1-6. It supplements
[the cutover procedure](sqlite-durable-storage-cutover.md) and
[the ADR](../adr/2026-09-06-sqlite-durable-storage.md#upgrade-and-recovery-responsibilities)'s
pre-rollout-validation requirement; it does not itself perform the host
cutover, which stays an operator/owner-run step in the deployment window.

## Setup

Every command below ran with `XDG_DATA_HOME`, `XDG_CONFIG_HOME`,
`XDG_CACHE_HOME`, and `PLECT_CONFIG_HOME` pointed at directories under a
disposable scratch path, never at `~/.local/share/plect` or
`~/.config/plect`. The host's `state.json` and `events/` tree were copied,
never moved or written to:

```console
$ cp -a ~/.local/share/plect/state.json ~/.local/share/plect/events "$ISO/copy/plect/"
$ du -sh "$ISO/copy/plect/state.json" "$ISO/copy/plect/events"
720K   .../copy/plect/state.json
110M   .../copy/plect/events
$ find "$ISO/copy/plect/events" -mindepth 1 -maxdepth 1 -type d | wc -l
1360
```

The release archive was downloaded and its checksum verified against the
release's published `SHA256SUMS` before anything else ran:

```console
$ gh release download v0.1.0 --repo kecbigmt/plecture \
    -p "plect_0.1.0_linux_amd64.tar.gz" -p "SHA256SUMS"
$ grep linux_amd64 SHA256SUMS | sha256sum -c -
plect_0.1.0_linux_amd64.tar.gz: OK
```

(`702c71c1e4bdc3dab7439710ce9d36546f94da67794ca065e3c6c03fb59b8309`, matching
the release API's asset digest and `gh`'s own download verification.)

The archive contains `bin/plect` and `bin/plect-web`, both release-stamped
(`version.Current` set via `-ldflags`, so `IsDevelopmentBuild` is false).

## 1. Initial import

Dry run, then the real import, both against the isolated copy:

```console
$ plect storage import --from "$ISO/copy/plect" --dry-run
sessions=1360 (event-log-only=1273) events=111350 (internal-backfilled=11668) cursors=2464 populations=0 population-members=0 up-reservations=0 unknown-files=0 promoted=false
real  0m24.7s

$ plect storage import --from "$ISO/copy/plect"
sessions=1360 (event-log-only=1273) events=111350 (internal-backfilled=11668) cursors=2464 populations=0 population-members=0 up-reservations=0 unknown-files=0 promoted=true
real  0m26.0s

$ cat "$ISO/data/plect/state.json"
{"version":0,"sessions":{}}
```

`state.json`'s parsed session count is 87 (`state.json`'s own top-level
`sessions` map); the importer's `sessions=1360` also counts the 1273
event-log-only directories (published to but never `plect create`d), which
`docs/migrations/sqlite-durable-storage-cutover.md`'s "what the importer
preserves" section documents as expected. The issue amendment's estimate
("1343 sessions / 107k events") was in the right neighborhood but not
exact; the numbers above are the actual counts from this host copy on
2026-09-07.

Both runs completed cleanly: `promoted=true` on the real run, the legacy
rejection marker written, `storage.db` created (~105 MB).

## 2. Legacy rejection

The installed pre-cutover binary (`/run/current-system/sw/bin/plect` →
`/nix/store/29viabq9x8dlwipka9f0fphsy4l84zd3-plect-0-unstable-42a190a6`),
with only `XDG_DATA_HOME` pointed at the imported store (per the dispatch
instruction — every other env var left at its default), against both a
read/startup path and a mutation path:

```console
$ XDG_DATA_HOME="$ISO/data" plect ls
Error: state schema version mismatch: got 0, want 7; run `go run ./plugins/legacy-migration/cmd/legacy-migration` before using this plect binary
exit=1

$ XDG_DATA_HOME="$ISO/data" plect up smoke-test-resource
Error: state schema version mismatch: got 0, want 7; run `go run ./plugins/legacy-migration/cmd/legacy-migration` before using this plect binary
exit=1
```

`state.json` is byte-for-byte unchanged afterward (only its `.lock`
sidecar appeared, matching every other read); this exactly reproduces the
verification already recorded in the cutover procedure's "Cutover
verification" section, now against the real host data volume rather than
a scratch directory.

## 3. Interrupted migration + restart

A fresh target directory, `SIGKILL`ed mid-import, then rerun:

```console
$ plect storage import --from "$ISO/copy/plect" &
$ sleep 5 && kill -9 $!
$ ls "$ISO/data-interrupt/plect/"
storage.db.importing  storage.db.importing.access.lock  storage.db.importing.coordination.lock
storage.db.importing-shm  storage.db.importing-wal
```

No `storage.db` and no `state.json` marker exist after the kill — matching
`app/internal/legacyimport/import.go`'s marker-before-rename ordering
(the marker and the promoting rename are both the very last two steps).
Rerunning the identical command against the same backup completes cleanly:

```console
$ plect storage import --from "$ISO/copy/plect"
sessions=1360 (event-log-only=1273) events=111350 (internal-backfilled=11668) cursors=2464 populations=0 population-members=0 up-reservations=0 unknown-files=0 promoted=true
real  0m25.7s

$ ls "$ISO/data-interrupt/plect/"
state.json  storage.db
$ cat "$ISO/data-interrupt/plect/state.json"
{"version":0,"sessions":{}}
```

No orphaned `.importing*` files remain; counts match the scenario 1 run
exactly.

## 4. Concurrent startup

Two v0.1.0 `plect ls --json` processes launched simultaneously against the
same already-imported store:

```console
$ plect ls --json > out1.json & plect ls --json > out2.json &
$ wait
exit1=0 exit2=0
$ diff out1.json out2.json && echo identical
identical
$ python3 -c "import json; print(len(json.load(open('out1.json'))))"
1360
```

`storage.db`'s checksum is unchanged before and after
(`037b7d31f3f780cd9223bc8c28edf0d7`), and a subsequent `plect storage
migrate` reports the store already at the latest schema version — no
corruption.

Since v0.1.0 is the release that produced this store's schema, there is no
migration pending for two v0.1.0 processes to race over on it; the above
exercises `persistence`'s normal concurrent shared-access path
(`accessGate.enterShared`), not `accessExclusive`/`migrationWait`. To
exercise the exclusion path itself with real imported data, a copy of the
store was rolled back by exactly one migration (see scenario 7's method
below) and two **development builds** of `origin/main` (same target schema
as v0.1.0 — see scenario 7) raced `storage migrate --allow-dev-build`
against it concurrently:

```console
$ plect-dev-main storage migrate --allow-dev-build > out1.txt &
$ plect-dev-main storage migrate --allow-dev-build > out2.txt &
$ wait
exit1=0 exit2=0
$ cat out1.txt out2.txt
.../storage.db is at schema version 20260907002408
.../storage.db is at schema version 20260907002408
$ plect-dev-main ls --json | python3 -c "import json,sys; print(len(json.load(sys.stdin)))"
1360
```

Both processes succeeded with no error; per `gate.go`'s design, one won the
coordination lock and migrated, the other found `current == target` after
acquiring it and no-opped. Session/event counts read back intact (1360
rows) — no corruption from the race.

## 5. Unsupported-version rejection

A copy of the imported store had a `goose_db_version` row inserted with a
version far ahead of anything v0.1.0's embedded migrations declare
(`99999999999999`), simulating a database written by a newer plect:

```console
$ sqlite-insert-fake-version "$DB" 99999999999999
rows inserted: 1
$ md5sum "$DB"
04a948bca0b5929a0f1476fa39077264

$ XDG_DATA_HOME=... plect ls
Error: state: open database: persistence: database schema version 99999999999999 is newer than this binary supports (max 20260907002408); use a newer plect binary, this one makes no change
exit=1

$ md5sum "$DB"
04a948bca0b5929a0f1476fa39077264
```

Refused without modification (checksum unchanged), matching
`refuseIfNewerThanSupported` in `app/internal/persistence/ensure.go`.

## 6. Browser reconnection

`plect serve` (bus) and `plect-web` (v0.1.0) were started against the
imported store (`XDG_RUNTIME_DIR`, socket, and web port all isolated; the
web UI's own bus socket resolved via `PLECT_BUS_SOCKET`). Tested against
`GET /api/v1/events/stream`, the JSON SSE endpoint the web frontend
actually uses (distinct from the HTML-fragment `/events/stream` endpoint,
which instead honors the `Last-Event-ID` header — the JSON endpoint takes
an explicit `?cursor=` query parameter, matching
`docs/design/web-ui-event-history.md`'s opaque-cursor contract):

- **Fresh connect** (no cursor): replays the tail and follows live;
  captured the last delivered event's cursor
  (`{"v":2,"off":33,"ord":"asc","stream_id":"01M1XJ4Y78SX985ZHQW7NABDM6"}`,
  base64-encoded).
- **Valid resume** (`?cursor=<that cursor>`): only `: connected` is
  emitted, then the connection idles live — the prior 32 events are **not**
  re-delivered:

  ```console
  $ curl -N -G .../api/v1/events/stream --data-urlencode session=... \
      --data-urlencode cursor=eyJ2IjoyLCJvZmYiOjMzLC4uLn0
  : connected
  (idle — no re-delivery)
  ```

- **Invalid cursor, several shapes, all rejected with HTTP 400 and no
  stream committed**:

  | Cursor | Response |
  |---|---|
  | Garbage (`not-a-valid-cursor`) | `400 invalid cursor: invalid character ...` |
  | Well-formed but unknown `stream_id` | `400 cursor expired (stale event stream); restart from the beginning` |
  | `CursorVersion` 1 (pre-cutover format) | `400 cursor expired (version 1, want 2); restart from the beginning` |
  | Raw legacy integer (`12345`) | `400 invalid cursor: illegal base64 data ...` |

This confirms the ADR's requirement directly: an invalid or pre-cutover
`Last-Event-ID`/cursor cannot silently become a valid position — every
malformed or stale shape is rejected, and a valid cursor resumes with no
gap and no re-delivery.

## 7. Old-data upgrade rehearsal

`origin/main` (commit `5923039`, 2026-09-07) carries the identical
migration set as v0.1.0 — no schema change has landed since the release
tag (`git log --oneline v0.1.0..origin/main -- app/internal/persistence/migrations`
is empty). Opening the v0.1.0-imported store with an unstamped
(`0.0.0-dev`) build of `origin/main` therefore finds `current == target`
immediately and succeeds without hitting `EnsureCurrent`'s dev-build
refusal branch — there is nothing pending to refuse:

```console
$ plect-dev-main storage migrate
.../storage.db is at schema version 20260907002408
exit=0
```

To rehearse the refusal-then-forward-migrate mechanism itself against real
imported data (not just the package's unit tests in
`app/internal/persistence/ensure_test.go`), a copy of the imported store
was rolled back by exactly one migration using goose's own `DownTo` (the
last migration, `20260907002408_add_named_columns_and_session_lifecycle`,
carries a `-- +goose Down` section):

```console
$ scratch-rollback "$DB" "$MIGRATIONS_DIR" 20260906231449
current version before rollback: 20260907002408
current version after rollback: 20260906231449
```

Against that store:

```console
$ plect-dev-main storage migrate
Error: persistence: storage.db is at schema 20260906231449; this development build would migrate it to 20260907002408.
Refusing: point XDG_DATA_HOME at a scratch directory, or run `plect storage migrate --allow-dev-build` deliberately.
exit=1

$ plect-dev-main storage migrate --allow-dev-build
.../storage.db is at schema version 20260907002408
exit=0

$ plect-dev-main ls --json | python3 -c "import json,sys; print(len(json.load(sys.stdin)))"
1360
```

The dev build refuses to forward-migrate a database it did not create,
`--allow-dev-build` applies the pending migration, and the store reads
back with the same 1360 sessions as the original import. This exercises
exactly the mechanism `EnsureCurrent`/`Migrate` use for any future
migration (including a real v0.2.0 one, once one exists) — the rollback
substitutes for a migration that does not exist yet, since none has
shipped since v0.1.0.

## Summary

| # | Scenario | Result |
|---|---|---|
| 1 | Initial import | Pass — dry run then real import, clean, `promoted=true` |
| 2 | Legacy rejection | Pass — exact documented error, `ls` and `up`, state.json unmodified |
| 3 | Interrupted migration + restart | Pass — killed mid-run, no partial promotion, clean rerun |
| 4 | Concurrent startup | Pass — two v0.1.0 readers, identical output, no corruption; migration-exclusion path separately verified via two racing dev builds |
| 5 | Unsupported-version rejection | Pass — refused, checksum unchanged |
| 6 | Browser reconnection | Pass — fresh connect, clean resume (no re-delivery), every invalid/stale/pre-cutover cursor shape rejected with 400 |
| 7 | Old-data upgrade rehearsal | Pass — dev-build refusal and `--allow-dev-build` forward migration both verified (via a one-migration rollback, since `origin/main` has no schema change beyond v0.1.0 yet) |

All scenarios ran against a copy of the devbox host's real `state.json` +
`events/` tree (87 sessions in `state.json`, 1360 event-log directories,
111350 events after import). No command in this validation touched
`~/.local/share/plect`, `~/.config/plect`, `~/.cache/plect`, or
`~/devbox`; the host's live daemon (`plect serve`, pid confirmed alive
before and after this work) was not restarted or otherwise affected.

The host cutover itself — backup, import, rebuild with devbox PR #970 +
#971, verification — remains an operator/owner step in the deployment
window, per the issue's scope.
