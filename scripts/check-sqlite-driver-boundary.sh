#!/usr/bin/env bash
# The SQLite driver (github.com/mattn/go-sqlite3) is a core-only,
# cgo-requiring dependency: docs/design/sqlite-persistence.md requires that
# plugins and contracts/* stay independently buildable without a C
# toolchain, so the driver must never appear in their build lists.
#
# check-provider-boundary.sh catches a provider NAME leaking into core; this
# is the reverse direction, a driver dependency leaking OUT of core into a
# module that must not need cgo. It resolves each module's actual build
# list with `go list -m all` rather than grepping its go.mod's own require
# block, so a driver pulled in only indirectly (through some other
# dependency) is caught too, not just a direct requirement.
set -euo pipefail

root="${DRIVER_BOUNDARY_CHECK_ROOT:-$(pwd)}"
cd "$root"

driver="github.com/mattn/go-sqlite3"

fail=0
count=0
while IFS= read -r gomod; do
  [ -z "$gomod" ] && continue
  count=$((count + 1))
  moddir="$(dirname "$gomod")"
  # GOWORK=off: the repository's go.work unions every workspace module's
  # build list, so `go list -m all` inside one `use`d module would
  # otherwise report the combined graph instead of that module's own — the
  # opposite of what a real standalone `go install` of that module sees.
  if (cd "$moddir" && GOWORK=off go list -m all 2>/dev/null) | grep -q "^$driver "; then
    echo "$gomod: depends on $driver, a core-only cgo-requiring dependency" >&2
    fail=1
  fi
done < <(
  scan_dirs=()
  for d in contracts plugins; do
    [ -d "$d" ] && scan_dirs+=("$d")
  done
  # find exits non-zero if any argument path is missing, even when the
  # others matched fine; filtering to only existing directories first
  # keeps a fixture missing one of the two (e.g. a plugins-only test
  # fixture) from silently short-circuiting this scan under set -e.
  if [ "${#scan_dirs[@]}" -gt 0 ]; then
    find "${scan_dirs[@]}" -name go.mod | sort
  fi
)

if [ "$fail" -ne 0 ]; then
  echo
  echo "SQLite driver boundary violated: $driver must stay inside" >&2
  echo "app/internal/persistence and must not appear in any contracts/ or" >&2
  echo "plugins/ module's build list." >&2
  exit 1
fi

echo "sqlite driver boundary check passed ($count go.mod files scanned)"
