# SQLite sidecar-state migration

This procedure moves post-cutover `tombstone.json`, `chain_attempts.json`,
and `pending_delivery.json` sidecars into `storage.db`. The updated binary
applies the append-only schema migration and performs the import when it first
opens the database.

## Before upgrading

Stop every plect process that uses the data directory. Back up the whole data
directory before starting the updated binary:

```sh
DATA_DIR="${PLECT_DATA_HOME:-${XDG_DATA_HOME:-$HOME/.local/share}/plect}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
cp -a "$DATA_DIR" "$DATA_DIR.sidecar-state-backup.$STAMP"
```

The importer maps each sidecar session name to its live `sessions.id`, or to
the most recently destroyed row of that name. A name with neither row, a
malformed sidecar, an unexpected path in `events/`, or a chain-attempt
generation that identifies another session stops the import and leaves every
source sidecar in place. Correct the source or restore the backup before
retrying.

## Upgrade and verify

Run any normal command from the updated release. Its storage opener applies
the migration, imports valid rows transactionally, and only then removes the
source sidecars. An interrupted cleanup is safe to rerun because the rows use
primary-key upserts before files are removed.

```sh
plect status
find "$DATA_DIR" -maxdepth 3 -type f | sort
```

The listing contains `storage.db`, its SQLite and persistence-gate sidecars,
and any `delivery-locks/<escaped-session>.lock` files. It contains no
`events/` tree, `pending_delivery.json`, `tombstone.json`, or
`chain_attempts.json`.

## Rollback

Stop plect again, move the migrated data directory aside, and restore the
backup created before the upgrade:

```sh
mv "$DATA_DIR" "$DATA_DIR.sidecar-state-failed.$STAMP"
mv "$DATA_DIR.sidecar-state-backup.$STAMP" "$DATA_DIR"
```

The schema migration is append-only, so restoring the backup is the rollback
mechanism for both the database schema and the removed sidecars.
