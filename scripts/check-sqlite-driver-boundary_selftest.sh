#!/usr/bin/env bash
# Proves check-sqlite-driver-boundary.sh actually catches a violation
# before it is trusted as a CI gate: run it against a fixture tree whose
# plugin module has a seeded requirement on the SQLite driver and require
# it to fail and name the module, then run it against a clean fixture tree
# and require it to pass.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
checker="$root/scripts/check-sqlite-driver-boundary.sh"

# The pinned version app/go.mod already requires, so `go get` below
# resolves from the local module cache this repository's own build already
# populated, without depending on network access in a sandboxed CI runner.
driver_version="$(cd "$root/app" && GOWORK=off go list -m -f '{{.Version}}' github.com/mattn/go-sqlite3)"

dirty="" clean=""
trap 'rm -rf "$dirty" "$clean"' EXIT

run_against() {
  local fixture="$1"
  DRIVER_BOUNDARY_CHECK_ROOT="$fixture" "$checker"
}

write_fixture_module() {
  local dir="$1"
  mkdir -p "$dir"
  cat > "$dir/go.mod" <<EOF
module example.com/fixture/$(basename "$dir")

go 1.25.6
EOF
  cat > "$dir/main.go" <<'EOF'
package main

func main() {}
EOF
}

# Dirty fixture: a plugin module that requires the driver must be caught.
# `go get` (not a hand-written require line) so go.sum gets a matching
# entry the same way app/go.mod's own real dependency did.
dirty=$(mktemp -d)
write_fixture_module "$dirty/plugins/seeded/src"
(cd "$dirty/plugins/seeded/src" && GOWORK=off go get "github.com/mattn/go-sqlite3@$driver_version")

if run_against "$dirty" >/tmp/sqlite-driver-boundary-selftest-dirty.log 2>&1; then
  echo "FAIL: checker passed against a fixture module requiring the SQLite driver" >&2
  cat /tmp/sqlite-driver-boundary-selftest-dirty.log >&2
  exit 1
fi
if ! grep -q "plugins/seeded/src/go.mod" /tmp/sqlite-driver-boundary-selftest-dirty.log; then
  echo "FAIL: checker did not name the offending module" >&2
  cat /tmp/sqlite-driver-boundary-selftest-dirty.log >&2
  exit 1
fi
echo "ok: checker fails and names the module on a seeded driver requirement"

# Clean fixture: no module requires the driver, nothing to report.
clean=$(mktemp -d)
write_fixture_module "$clean/plugins/clean/src"
if ! run_against "$clean" >/tmp/sqlite-driver-boundary-selftest-clean.log 2>&1; then
  echo "FAIL: checker failed against a clean fixture" >&2
  cat /tmp/sqlite-driver-boundary-selftest-clean.log >&2
  exit 1
fi
echo "ok: checker passes on a clean fixture"
